// Package outbox implements the transactional outbox (D48). A service writes
// each event in the transaction that changes its state, and Relay publishes
// the written events to the broker: an event exists exactly when its change
// does, and reaches the broker at least once.
package outbox

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/thiagogenez/labflow/libs/go/postgres"
	"github.com/thiagogenez/labflow/libs/go/rabbitmq"
)

const (
	// lockID is the advisory lock a relay holds while it publishes a batch,
	// so one replica publishes at a time.
	lockID    = 7_271_013
	batchSize = 100
	// pollInterval is the wait after a batch that emptied the outbox. The
	// confirmation email has 60 seconds (D44).
	pollInterval = time.Second
	maxBackoff   = 30 * time.Second
	// batchTimeout bounds one batch, including on shutdown, when the batch in
	// flight finishes so a deployment does not publish it twice.
	batchTimeout = 10 * time.Second
	// commitMargin is kept at the end of each batch for recording what the
	// broker accepted, so a slow broker never leaves accepted events
	// unmarked, to be published again.
	commitMargin = 3 * time.Second
	// maintainInterval spaces measuring, expiring and cleaning up, which run
	// apart from publishing so that neither delays the other.
	maintainInterval = 5 * time.Second
	// lifetime is how long an event may be processed (D64). A published
	// event carries what is left of it as its message expiration, so the
	// outbox and the consumer's queue share one 7-day budget, as long as
	// RabbitMQ, which expires messages only at the head of a queue, reaches
	// it in time.
	lifetime = 7 * 24 * time.Hour
	// keep is how long an event stays in the outbox: an older one is never
	// published, and is deleted whether published or not.
	keep = lifetime - time.Hour
	// cleanupInterval spaces the sweeps of published events past keep; half
	// of keep's hour is left for a sweep to drain its backlog, a batch per
	// turn, before any event turns 7 days old.
	cleanupInterval = 30 * time.Minute
	// sweepBatch rows are deleted per statement, well inside the 5-second
	// statement timeout; the rest wait for the next turn.
	sweepBatch = 1000
	// loggedIDs bounds the event IDs one log record lists.
	loggedIDs = 10
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migration creates the outbox table where it is missing. A service applies
// it after its own migrations.
var Migration = postgres.NewMigration(migrationFS, "migrations", "outbox_version")

var (
	published = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "outbox_events_published_total",
		Help: "Events the broker accepted from the outbox and the relay recorded as published, by event type.",
	}, []string{"type"})
	expired = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "outbox_events_expired_total",
		Help: "Events deleted unpublished at the outbox's 7-day limit (D64), by event type; the backlog age alert fires long before.",
	}, []string{"type"})
	failedBatches = promauto.NewCounter(prometheus.CounterOpts{
		Name: "outbox_publish_failures_total",
		Help: "Outbox batches that failed at the broker or the database; their events are published again.",
	})
	unpublished = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "outbox_unpublished_events",
		Help: "Events written to the outbox and not yet accepted by the broker.",
	})
	oldestUnpublished = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "outbox_oldest_unpublished_age_seconds",
		Help: "Age of the oldest event not yet accepted by the broker; it grows while the broker is unreachable.",
	})
	oldestEvent = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "outbox_oldest_event_age_seconds",
		Help: "Age of the oldest event in the outbox, published or not; above 7 days less 30 minutes, the deletion that keeps personal data under 7 days may be failing.",
	})
	lastMeasured = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "outbox_last_measured_timestamp_seconds",
		Help: "When the outbox gauges were last measured; while it falls behind, they show old values.",
	})
)

// Event is one row of the outbox.
type Event struct {
	// ID is the CloudEvents id, a UUID.
	ID string
	// Topic is the producer's topic, such as orders (D62).
	Topic string
	// AggregateID is the order ID.
	AggregateID string
	// Type is the CloudEvents type, the RabbitMQ routing key.
	Type string
	// Payload is the CloudEvent in structured JSON mode.
	Payload []byte
}

// Write adds an event to the outbox inside the caller's transaction.
func Write(ctx context.Context, tx pgx.Tx, e Event) error {
	_, err := tx.Exec(ctx, `INSERT INTO outbox (id, aggregatetype, aggregateid, type, payload)
		VALUES ($1, $2, $3, $4, $5)`, e.ID, e.Topic, e.AggregateID, e.Type, e.Payload)
	return err
}

// Relay publishes outbox events to RabbitMQ and deletes them before they are
// 7 days old.
type Relay struct {
	pool   *pgxpool.Pool
	pub    *rabbitmq.Publisher
	log    *slog.Logger
	tracer trace.Tracer
}

// NewRelay returns a relay; Run starts it.
func NewRelay(pool *pgxpool.Pool, pub *rabbitmq.Publisher, log *slog.Logger) *Relay {
	return &Relay{pool: pool, pub: pub, log: log, tracer: otel.Tracer("github.com/thiagogenez/labflow/libs/go/outbox")}
}

// Run publishes until ctx ends, then finishes the batch in flight and closes
// the broker connection. Every replica runs it; an advisory lock lets one
// publish each batch, and a crashed replica's lock is released with its
// connection, so no leader election is needed. Beside publishing, it keeps
// the backlog gauges current and deletes old events.
func (r *Relay) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() { r.maintain(ctx) })
	defer wg.Wait()
	defer r.pub.Close()
	var backoff time.Duration
	for ctx.Err() == nil {
		// Detached from ctx, so the batch in flight finishes on shutdown.
		bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), batchTimeout)
		n, err := r.publishBatch(bctx)
		cancel()
		wait := pollInterval
		switch {
		case err != nil:
			failedBatches.Inc()
			backoff = min(max(2*backoff, pollInterval), maxBackoff)
			wait = backoff
			r.log.WarnContext(ctx, "outbox publish failed; retrying", "error", err, "retry_in", wait.String())
		case n == batchSize:
			backoff, wait = 0, 0 // more are waiting
		default:
			backoff = 0
		}
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
	}
}

type row struct {
	ID          string
	Type        string
	Payload     []byte
	TraceParent string
	// LeftMillis is what is left of the event's lifetime.
	LeftMillis int64
}

// publishBatch publishes the oldest unpublished events still in their
// lifetime, if this replica gets the lock, and marks the ones the broker
// accepted. It reports how many it tried. A crash after publishing and
// before the commit publishes them again with the same message ID, which
// consumers deduplicate (D49).
func (r *Relay) publishBatch(ctx context.Context) (int, error) {
	var tried int
	var pubErr error
	var accepted []row
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var locked bool
		if err := tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock($1)", lockID).Scan(&locked); err != nil || !locked {
			return err
		}
		rows, _ := tx.Query(ctx, `SELECT id, type, payload, coalesce(payload->>'traceparent', ''),
				(extract(epoch FROM created_at + make_interval(secs => $3) - now()) * 1000)::bigint
			FROM outbox WHERE published_at IS NULL AND created_at >= now() - make_interval(secs => $1)
			ORDER BY created_at, id LIMIT $2 FOR UPDATE`, keep.Seconds(), batchSize, lifetime.Seconds())
		batch, err := pgx.CollectRows(rows, pgx.RowToStructByPos[row])
		if err != nil || len(batch) == 0 {
			return err
		}
		tried = len(batch)
		msgs := make([]rabbitmq.Message, len(batch))
		for i, e := range batch {
			msgs[i] = rabbitmq.Message{ID: e.ID, RoutingKey: e.Type, Body: e.Payload, TTL: time.Duration(e.LeftMillis) * time.Millisecond}
		}
		start := time.Now()
		pctx, cancel := context.WithDeadline(ctx, publishDeadline(ctx))
		errs := r.pub.Publish(pctx, msgs)
		cancel()
		var ids []string
		var failed []error
		for i, err := range errs {
			if err == nil {
				ids = append(ids, batch[i].ID)
				accepted = append(accepted, batch[i])
				r.span(ctx, batch[i], start)
			} else {
				failed = append(failed, fmt.Errorf("event %s: %w", batch[i].ID, err))
			}
		}
		if len(failed) > 0 {
			// The first reason says why; the rest usually repeat it.
			pubErr = fmt.Errorf("%d of %d events not accepted: %w", len(failed), len(batch), failed[0])
		}
		_, err = tx.Exec(ctx, "UPDATE outbox SET published_at = now() WHERE id = ANY($1)", ids)
		return err
	})
	if err == nil {
		// Counted once committed, so an event published again after a
		// failed commit is not counted twice.
		for _, e := range accepted {
			published.WithLabelValues(e.Type).Inc()
		}
	}
	return tried, errors.Join(pubErr, err)
}

// publishDeadline leaves commitMargin of the batch's deadline for recording
// the outcome, however long the database took before publishing.
func publishDeadline(ctx context.Context) time.Time {
	d, ok := ctx.Deadline()
	if !ok {
		d = time.Now().Add(batchTimeout)
	}
	return d.Add(-commitMargin)
}

// span records an accepted publish as a child of the change that wrote the
// event, so the checkout's trace shows when its event left (D61). Failed
// attempts are logged and counted instead: a span for each retry would
// flood the trace while the broker fails.
func (r *Relay) span(ctx context.Context, e row, start time.Time) {
	parent := propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier{"traceparent": e.TraceParent})
	_, s := r.tracer.Start(parent, "publish "+rabbitmq.Exchange, trace.WithSpanKind(trace.SpanKindProducer), trace.WithTimestamp(start),
		trace.WithAttributes(
			attribute.String("messaging.system", "rabbitmq"),
			attribute.String("messaging.operation.type", "send"),
			attribute.String("messaging.destination.name", rabbitmq.Exchange),
			attribute.String("messaging.rabbitmq.destination.routing_key", e.Type),
			attribute.String("messaging.message.id", e.ID),
		))
	s.End()
}

// maintain measures the backlog, expires unpublished events and deletes
// published ones every interval until ctx ends. It needs no lock: a delete
// repeated by another replica deletes nothing.
func (r *Relay) maintain(ctx context.Context) {
	t := time.NewTicker(maintainInterval)
	defer t.Stop()
	var nextCleanup time.Time
	for {
		bounded(ctx, r.measure)
		bounded(ctx, r.expire)
		if !time.Now().Before(nextCleanup) && bounded(ctx, r.cleanup) {
			nextCleanup = time.Now().Add(cleanupInterval)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// bounded runs one maintenance step under its own deadline: the server's
// statement timeout cannot end a wait on a connection that stopped
// answering, which would stall every later step.
func bounded(ctx context.Context, step func(context.Context) bool) bool {
	ctx, cancel := context.WithTimeout(ctx, maintainInterval)
	defer cancel()
	return step(ctx)
}

// measure updates the backlog and retention gauges. Every replica measures,
// so none keeps reporting a stale value after another has taken over. Each
// subquery reads one of the two partial indexes.
func (r *Relay) measure(ctx context.Context) bool {
	var n int
	var age, oldest float64
	err := r.pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM outbox WHERE published_at IS NULL),
			coalesce(extract(epoch FROM now() - (SELECT min(created_at) FROM outbox WHERE published_at IS NULL)), 0)::float8,
			coalesce(extract(epoch FROM now() - least(
				(SELECT min(created_at) FROM outbox WHERE published_at IS NULL),
				(SELECT min(created_at) FROM outbox WHERE published_at IS NOT NULL))), 0)::float8`).Scan(&n, &age, &oldest)
	if err != nil {
		r.warn(ctx, "measure outbox backlog failed", err)
		return false
	}
	unpublished.Set(float64(n))
	oldestUnpublished.Set(age)
	oldestEvent.Set(oldest)
	lastMeasured.SetToCurrentTime()
	return true
}

// expire deletes up to sweepBatch unpublished events past keep, which
// publishBatch no longer selects, and logs and counts them once deleted. It
// skips the rows a batch has locked: an event crossing keep while being
// published is published, not expired.
func (r *Relay) expire(ctx context.Context) bool {
	rows, _ := r.pool.Query(ctx, `DELETE FROM outbox WHERE id IN (
			SELECT id FROM outbox WHERE published_at IS NULL AND created_at < now() - make_interval(secs => $1)
			ORDER BY created_at LIMIT $2 FOR UPDATE SKIP LOCKED)
		RETURNING id::text, type`, keep.Seconds(), sweepBatch)
	gone, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ ID, Type string }])
	if err != nil {
		r.warn(ctx, "expire outbox events failed", err)
		return false
	}
	if len(gone) == 0 {
		return true
	}
	ids := make([]string, 0, loggedIDs)
	for _, e := range gone {
		expired.WithLabelValues(e.Type).Inc()
		if len(ids) < loggedIDs {
			ids = append(ids, e.ID)
		}
	}
	r.log.ErrorContext(ctx, "outbox events expired unpublished", "count", len(gone), "older_than", keep.String(), "first_event_ids", ids)
	return true
}

// cleanup deletes up to sweepBatch published events past keep and reports
// whether none is left.
func (r *Relay) cleanup(ctx context.Context) bool {
	tag, err := r.pool.Exec(ctx, `DELETE FROM outbox WHERE id IN (
		SELECT id FROM outbox WHERE published_at IS NOT NULL AND created_at < now() - make_interval(secs => $1)
		ORDER BY created_at LIMIT $2)`, keep.Seconds(), sweepBatch)
	if err != nil {
		r.warn(ctx, "delete published outbox events failed; retrying", err)
		return false
	}
	if n := tag.RowsAffected(); n > 0 {
		r.log.InfoContext(ctx, "published outbox events deleted", "count", n)
	}
	return tag.RowsAffected() < sweepBatch
}

// warn logs a failure, except one caused by shutdown.
func (r *Relay) warn(ctx context.Context, msg string, err error) {
	if ctx.Err() == nil {
		r.log.WarnContext(ctx, msg, "error", err)
	}
}
