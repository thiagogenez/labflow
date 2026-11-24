package outbox

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/thiagogenez/labflow/libs/go/pgtest"
	"github.com/thiagogenez/labflow/libs/go/rabbitmq"
	"github.com/thiagogenez/labflow/libs/go/rabbitmqtest"
)

const (
	queue    = "email-worker"
	placed   = "labflow.order.placed.v1"
	unbound  = "labflow.order.unbound.v1"
	traceID  = "4bf92f3577b34da6a3ce929d0e0e4736"
	parentID = "00f067aa0ba902b7"
)

// nearly7Days is an event age in its last hour before 7 days, when no store
// may keep it any more (D64). Literal, so that a wrong keep fails the tests.
const nearly7Days = 7*24*time.Hour - 30*time.Minute

func TestMain(m *testing.M) {
	os.Exit(rabbitmqtest.Main(func() int { return pgtest.Main(m) }))
}

func newRelay(t *testing.T) (*Relay, *pgxpool.Pool) {
	t.Helper()
	rabbitmqtest.Purge(t, queue)
	pool := pgtest.NewDatabase(t, Migration)
	pub := rabbitmq.NewPublisher(rabbitmqtest.URL(t, "order-api"), "test")
	t.Cleanup(pub.Close)
	return NewRelay(pool, pub, slog.New(slog.DiscardHandler)), pool
}

// write commits events of the given types and returns their IDs.
func write(t *testing.T, pool *pgxpool.Pool, types ...string) []string {
	t.Helper()
	ids := make([]string, len(types))
	err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		for i, typ := range types {
			ids[i] = uuid.Must(uuid.NewV7()).String()
			payload := fmt.Sprintf(`{"id":%q,"type":%q,"traceparent":"00-%s-%s-01"}`, ids[i], typ, traceID, parentID)
			if err := Write(t.Context(), tx, Event{ID: ids[i], Topic: "orders", AggregateID: "o1", Type: typ, Payload: []byte(payload)}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func ids(t *testing.T, pool *pgxpool.Pool, where string) []string {
	t.Helper()
	rows, _ := pool.Query(t.Context(), "SELECT id::text FROM outbox WHERE "+where+" ORDER BY created_at, id")
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestRelayPublishesEachEventOnce(t *testing.T) {
	r, pool := newRelay(t)
	want := write(t, pool, placed, placed, placed)
	before := testutil.ToFloat64(published.WithLabelValues(placed))
	if n, err := r.publishBatch(t.Context()); n != 3 || err != nil {
		t.Fatalf("publishBatch = %d, %v", n, err)
	}
	if got := testutil.ToFloat64(published.WithLabelValues(placed)) - before; got != 3 {
		t.Errorf("outbox_events_published_total grew by %v, want 3", got)
	}
	for _, id := range want {
		if d := rabbitmqtest.Get(t, queue); d.MessageId != id {
			t.Errorf("message %s, want %s in outbox order", d.MessageId, id)
		}
	}
	if left := ids(t, pool, "published_at IS NULL"); len(left) != 0 {
		t.Errorf("unpublished after the batch: %v", left)
	}
	if n, err := r.publishBatch(t.Context()); n != 0 || err != nil {
		t.Errorf("second publishBatch = %d, %v; want nothing left", n, err)
	}
}

// A published event expires in its queue when its lifetime ends, counted
// from its creation, so the outbox and the queue share 7 days (D64).
func TestPublishedEventCarriesWhatIsLeftOfItsLifetime(t *testing.T) {
	r, pool := newRelay(t)
	id := write(t, pool, placed)[0]
	if _, err := pool.Exec(t.Context(), "UPDATE outbox SET created_at = now() - interval '6 days' WHERE id = $1", id); err != nil {
		t.Fatal(err)
	}
	if _, err := r.publishBatch(t.Context()); err != nil {
		t.Fatal(err)
	}
	d := rabbitmqtest.Get(t, queue)
	left, err := strconv.ParseInt(d.Expiration, 10, 64)
	if day := (24 * time.Hour).Milliseconds(); err != nil || left > day || left < day-(10*time.Second).Milliseconds() {
		t.Errorf("expiration = %q, want about a day in milliseconds", d.Expiration)
	}
}

// An event the broker refuses stays in the outbox and does not hold back
// the events it accepted.
func TestRelayKeepsWhatTheBrokerRefused(t *testing.T) {
	r, pool := newRelay(t)
	written := write(t, pool, placed, unbound, placed)
	_, err := r.publishBatch(t.Context())
	if err == nil || !strings.Contains(err.Error(), written[1]) || !strings.Contains(err.Error(), "NO_ROUTE") {
		t.Fatalf("publishBatch error = %v, want one naming %s and NO_ROUTE", err, written[1])
	}
	if left := ids(t, pool, "published_at IS NULL"); len(left) != 1 || left[0] != written[1] {
		t.Errorf("unpublished = %v, want only %s", left, written[1])
	}
}

// Events past their lifetime are never published, and are deleted in
// bounded batches, counted once deleted (D64); fresh events still go out
// before the backlog of expired ones is gone.
func TestRelayExpiresEventsPastTheirLifetime(t *testing.T) {
	r, pool := newRelay(t)
	if _, err := pool.Exec(t.Context(), `INSERT INTO outbox (id, aggregatetype, aggregateid, type, payload, created_at)
		SELECT gen_random_uuid(), 'orders', 'o1', $1, '{}', now() - make_interval(secs => $2)
		FROM generate_series(1, $3)`, placed, nearly7Days.Seconds(), sweepBatch+1); err != nil {
		t.Fatal(err)
	}
	fresh := write(t, pool, placed)[0]
	before := testutil.ToFloat64(expired.WithLabelValues(placed))
	if n, err := r.publishBatch(t.Context()); n != 1 || err != nil {
		t.Fatalf("publishBatch = %d, %v; want only the fresh event", n, err)
	}
	if d := rabbitmqtest.Get(t, queue); d.MessageId != fresh {
		t.Errorf("published %s, want %s", d.MessageId, fresh)
	}
	r.expire(t.Context())
	if left := len(ids(t, pool, "published_at IS NULL")); left != 1 {
		t.Errorf("%d expired events left after one sweep, want 1", left)
	}
	r.expire(t.Context())
	if left := ids(t, pool, "true"); len(left) != 1 || left[0] != fresh {
		t.Errorf("outbox = %d rows, want only %s", len(left), fresh)
	}
	if got := testutil.ToFloat64(expired.WithLabelValues(placed)) - before; got != sweepBatch+1 {
		t.Errorf("outbox_events_expired_total grew by %v, want %d", got, sweepBatch+1)
	}
}

// An event crossing keep while a batch publishes it is left to the batch:
// expiry skips the rows a batch holds, at once, instead of deleting an
// event the broker may accept or waiting for the batch.
func TestExpirySkipsAnEventBeingPublished(t *testing.T) {
	r, pool := newRelay(t)
	connectThenHang(t, r, pool)
	id := write(t, pool, placed)[0]
	if _, err := pool.Exec(t.Context(), "UPDATE outbox SET created_at = now() - make_interval(secs => $2) WHERE id = $1",
		id, (keep - 2*time.Second).Seconds()); err != nil {
		t.Fatal(err)
	}
	var batch sync.WaitGroup
	batch.Go(func() { _, _ = r.publishBatch(t.Context()) }) // waits 7 s on the hung broker
	time.Sleep(3 * time.Second)                             // the event is now past keep
	before := testutil.ToFloat64(expired.WithLabelValues(placed))
	start := time.Now()
	if !r.expire(t.Context()) || time.Since(start) > time.Second {
		t.Errorf("expire failed or waited %v", time.Since(start))
	}
	batch.Wait()
	if testutil.ToFloat64(expired.WithLabelValues(placed)) != before || len(ids(t, pool, "id = '"+id+"'")) != 1 {
		t.Error("expire deleted an event being published")
	}
}

// Sweeps run often enough to drain a backlog before any event turns 7 days
// old.
func TestCleanupLeavesHeadroom(t *testing.T) {
	if headroom := 7*24*time.Hour - keep; headroom < 2*cleanupInterval {
		t.Errorf("keep leaves %v before 7 days, less than two sweeps of %v", headroom, cleanupInterval)
	}
}

// A maintenance step ends at its own deadline when the database stops
// answering, so the gauges and sweeps carry on once it answers again.
func TestMaintenanceStepEndsWhenTheDatabaseHangs(t *testing.T) {
	r, _ := newRelay(t)
	pgtest.Pause(t)
	done := make(chan bool)
	go func() { done <- bounded(t.Context(), r.measure) }()
	select {
	case ok := <-done:
		if ok {
			t.Error("measure succeeded against a paused database")
		}
	case <-time.After(maintainInterval + 2*time.Second):
		t.Fatal("measure still waiting on the paused database")
	}
}

// While another replica holds the lock, a relay publishes nothing, so two
// replicas never publish the same batch (D48).
func TestRelayWithoutTheLockPublishesNothing(t *testing.T) {
	r, pool := newRelay(t)
	write(t, pool, placed)
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(t.Context(), "SELECT pg_advisory_xact_lock($1)", lockID); err != nil {
		t.Fatal(err)
	}
	if n, err := r.publishBatch(t.Context()); n != 0 || err != nil {
		t.Errorf("publishBatch = %d, %v; want nothing while the lock is held", n, err)
	}
	_ = tx.Rollback(t.Context())
	if n, err := r.publishBatch(t.Context()); n != 1 || err != nil {
		t.Errorf("publishBatch after release = %d, %v", n, err)
	}
}

// connectThenHang publishes one event, so the relay holds a connection, and
// then freezes the broker, which keeps the connection open without answering.
func connectThenHang(t *testing.T, r *Relay, pool *pgxpool.Pool) {
	t.Helper()
	write(t, pool, placed)
	if _, err := r.publishBatch(t.Context()); err != nil {
		t.Fatal(err)
	}
	rabbitmqtest.Pause(t)
}

// A hung broker costs one batch, not the database: publishing stops early
// enough for the transaction to record what was accepted and end on time.
func TestBatchEndsInTimeWhenTheBrokerHangs(t *testing.T) {
	r, pool := newRelay(t)
	connectThenHang(t, r, pool)
	write(t, pool, placed)
	ctx, cancel := context.WithTimeout(t.Context(), batchTimeout)
	defer cancel()
	start := time.Now()
	_, err := r.publishBatch(ctx)
	if took := time.Since(start); err == nil || took > batchTimeout-commitMargin+time.Second {
		t.Errorf("publishBatch = %v after %v; want a broker error within %v", err, took, batchTimeout-commitMargin)
	}
	if left := ids(t, pool, "published_at IS NULL"); len(left) != 1 {
		t.Errorf("unpublished = %v, want the event kept", left)
	}
}

// The commit margin comes off the batch's deadline, not off the time left
// when publishing starts.
func TestPublishLeavesTheCommitMarginOfTheBatch(t *testing.T) {
	r, pool := newRelay(t)
	connectThenHang(t, r, pool)
	write(t, pool, placed)
	ctx, cancel := context.WithTimeout(t.Context(), commitMargin+time.Second)
	defer cancel()
	start := time.Now()
	if _, err := r.publishBatch(ctx); err == nil || time.Since(start) > commitMargin {
		t.Errorf("publishBatch = %v after %v; want a broker error within %v", err, time.Since(start), time.Second)
	}
}

// While the broker hangs, the backlog gauges keep reporting the waiting
// event, apart from the batch stuck on it, so the age alert fires (D44).
func TestRunMeasuresTheBacklogWhileTheBrokerHangs(t *testing.T) {
	r, pool := newRelay(t)
	connectThenHang(t, r, pool)
	unpublished.Set(0)
	write(t, pool, placed)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	defer func() { cancel(); <-done }()
	time.Sleep(2 * time.Second) // the first batch waits on the broker for 7 s
	if got := testutil.ToFloat64(unpublished); got != 1 {
		t.Errorf("outbox_unpublished_events = %v, want 1", got)
	}
}

func TestPublishContinuesTheTraceOfTheChange(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	r, pool := newRelay(t)
	written := write(t, pool, placed)
	if _, err := r.publishBatch(t.Context()); err != nil {
		t.Fatal(err)
	}
	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("%d spans, want 1", len(spans))
	}
	s := spans[0]
	if s.Name() != "publish labflow.events" || s.SpanKind() != trace.SpanKindProducer ||
		s.SpanContext().TraceID().String() != traceID || s.Parent().SpanID().String() != parentID {
		t.Errorf("span %q kind %v trace %s parent %s", s.Name(), s.SpanKind(), s.SpanContext().TraceID(), s.Parent().SpanID())
	}
	want := attribute.String("messaging.message.id", written[0])
	found := false
	for _, a := range s.Attributes() {
		found = found || a == want
	}
	if !found {
		t.Errorf("attributes %v lack %v", s.Attributes(), want)
	}
}

// A failed attempt adds no span: a retry every few seconds would otherwise
// flood the checkout's trace.
func TestFailedPublishAddsNoSpan(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	for name, url := range map[string]string{
		"unroutable event":   rabbitmqtest.URL(t, "order-api"),
		"unreachable broker": "amqp://order-api:order-api@127.0.0.1:1/labflow",
	} {
		t.Run(name, func(t *testing.T) {
			pool := pgtest.NewDatabase(t, Migration)
			pub := rabbitmq.NewPublisher(url, "test")
			t.Cleanup(pub.Close)
			write(t, pool, unbound)
			if _, err := NewRelay(pool, pub, slog.New(slog.DiscardHandler)).publishBatch(t.Context()); err == nil {
				t.Fatal("publishBatch succeeded")
			}
			if n := len(rec.Ended()); n != 0 {
				t.Errorf("%d spans, want none", n)
			}
		})
	}
}

// The retention gauge follows the oldest row whether published or not, so a
// failing deletion shows before personal data turns 7 days old; the
// timestamp shows when the gauges were last current.
func TestMeasureReportsTheOldestEventPublishedOrNot(t *testing.T) {
	for _, published := range []bool{true, false} {
		t.Run(fmt.Sprintf("oldest published %v", published), func(t *testing.T) {
			r, pool := newRelay(t)
			w := write(t, pool, placed, placed)
			q := "UPDATE outbox SET created_at = now() - interval '3 days' WHERE id = $1"
			if published {
				q = "UPDATE outbox SET created_at = now() - interval '3 days', published_at = now() WHERE id = $1"
			}
			if _, err := pool.Exec(t.Context(), q, w[0]); err != nil {
				t.Fatal(err)
			}
			before := time.Now()
			if !r.measure(t.Context()) {
				t.Fatal("measure failed")
			}
			if got, want := testutil.ToFloat64(oldestEvent), (72 * time.Hour).Seconds(); got < want || got > want+60 {
				t.Errorf("outbox_oldest_event_age_seconds = %v, want about %v", got, want)
			}
			if at := testutil.ToFloat64(lastMeasured); at < float64(before.Unix()) {
				t.Errorf("outbox_last_measured_timestamp_seconds = %v, not updated", at)
			}
		})
	}
}

func TestCleanupDeletesOnlyOldPublishedEvents(t *testing.T) {
	r, pool := newRelay(t)
	w := write(t, pool, placed, placed, placed, placed)
	// w[0] published and in its last hour before 7 days; w[1] published,
	// 6 days old; w[2] unpublished for 8 days, which expire handles; w[3]
	// published now.
	age := func(id string, d time.Duration, publish bool) {
		q := "UPDATE outbox SET created_at = now() - make_interval(secs => $2)"
		if publish {
			q += ", published_at = now()"
		}
		if _, err := pool.Exec(t.Context(), q+" WHERE id = $1", id, d.Seconds()); err != nil {
			t.Fatal(err)
		}
	}
	age(w[0], nearly7Days, true)
	age(w[1], 6*24*time.Hour, true)
	age(w[2], 8*24*time.Hour, false)
	age(w[3], 0, true)
	if !r.cleanup(t.Context()) {
		t.Fatal("cleanup did not finish")
	}
	if left, want := ids(t, pool, "true"), []string{w[2], w[1], w[3]}; fmt.Sprint(left) != fmt.Sprint(want) {
		t.Errorf("left %v, want %v", left, want)
	}
}

// A cleanup that fails is retried on the next turn, not an hour later.
func TestRunRetriesAFailedCleanup(t *testing.T) {
	r, pool := newRelay(t)
	old := write(t, pool, placed)[0]
	if _, err := pool.Exec(t.Context(), "UPDATE outbox SET created_at = now() - interval '7 days', published_at = now() WHERE id = $1", old); err != nil {
		t.Fatal(err)
	}
	// Another transaction holds the row, so the first cleanup times out.
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(t.Context(), "SELECT 1 FROM outbox WHERE id = $1 FOR UPDATE", old); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	defer func() { cancel(); <-done }()
	time.Sleep(3 * time.Second) // past the 2-second lock timeout
	_ = tx.Rollback(t.Context())
	deadline := time.Now().Add(10 * time.Second)
	for len(ids(t, pool, "true")) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the failed cleanup was not retried")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Run publishes what is written while it runs and returns once its context
// ends.
func TestRunPublishesAndStops(t *testing.T) {
	r, pool := newRelay(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	id := write(t, pool, placed)[0]
	if d := rabbitmqtest.Get(t, queue); d.MessageId != id {
		t.Errorf("message %s, want %s", d.MessageId, id)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(batchTimeout + 5*time.Second):
		t.Fatal("Run did not return after its context ended")
	}
}

// A relay started after shutdown began publishes nothing.
func TestRunWithAnEndedContextPublishesNothing(t *testing.T) {
	r, pool := newRelay(t)
	write(t, pool, placed)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r.Run(ctx)
	if left := ids(t, pool, "published_at IS NULL"); len(left) != 1 {
		t.Errorf("unpublished = %v, want the event kept", left)
	}
}
