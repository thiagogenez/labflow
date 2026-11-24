package migrations_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/thiagogenez/labflow/libs/go/idempotency"
	"github.com/thiagogenez/labflow/libs/go/outbox"
	"github.com/thiagogenez/labflow/libs/go/pgtest"
	"github.com/thiagogenez/labflow/libs/go/postgres"
	"github.com/thiagogenez/labflow/libs/go/rabbitmq"
	"github.com/thiagogenez/labflow/libs/go/rabbitmqtest"
	"github.com/thiagogenez/labflow/services/order-api/internal/order"
	"github.com/thiagogenez/labflow/services/order-api/migrations"
)

func TestMain(m *testing.M) {
	os.Exit(rabbitmqtest.Main(func() int { return pgtest.Main(m) }))
}

// A database from before libs/go/outbox, holding an event not yet published,
// takes the library's migration, and the relay then publishes that event
// intact with order-api's broker permissions.
func TestUpgradeKeepsPendingEvents(t *testing.T) {
	ctx := t.Context()
	log := slog.New(slog.DiscardHandler)
	dsn := pgtest.NewDSN(t)
	if err := postgres.Migrate(ctx, dsn, log, idempotency.Migration, migrations.Migration); err != nil {
		t.Fatal(err)
	}
	pool, err := postgres.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	id := uuid.Must(uuid.NewV7()).String()
	payload := fmt.Sprintf(`{"id": %q, "type": %q, "subject": "o1"}`, id, order.TypeOrderPlaced)
	if _, err := pool.Exec(ctx, `INSERT INTO outbox (id, aggregatetype, aggregateid, type, payload)
		VALUES ($1, 'orders', 'o1', $2, $3)`, id, order.TypeOrderPlaced, payload); err != nil {
		t.Fatal(err)
	}

	if err := postgres.Migrate(ctx, dsn, log, migrations.All...); err != nil {
		t.Fatal(err)
	}
	rabbitmqtest.Purge(t, "email-worker")
	relay := outbox.NewRelay(pool, rabbitmq.NewPublisher(rabbitmqtest.URL(t, "order-api"), "test"), log)
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		relay.Run(runCtx)
		close(done)
	}()
	defer func() { stop(); <-done }()

	d := rabbitmqtest.Get(t, "email-worker")
	var got, want any
	if err := json.Unmarshal(d.Body, &got); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(payload), &want)
	if d.MessageId != id || !reflect.DeepEqual(got, want) {
		t.Errorf("published %s %s, want %s %s", d.MessageId, d.Body, id, payload)
	}
}

// The upgrade does not block checkout: with a write open on the outbox, the
// library's migrations wait for it, while further writes go through. A
// plain CREATE INDEX, even IF NOT EXISTS, would queue its lock behind the
// open write and make every later write wait.
func TestUpgradeDoesNotBlockWrites(t *testing.T) {
	ctx := t.Context()
	log := slog.New(slog.DiscardHandler)
	dsn := pgtest.NewDSN(t)
	if err := postgres.Migrate(ctx, dsn, log, idempotency.Migration, migrations.Migration); err != nil {
		t.Fatal(err)
	}
	pool, err := postgres.Connect(ctx, dsn) // the application's 2-second lock timeout
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	insert := func(q interface {
		Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	}) error {
		_, err := q.Exec(ctx, `INSERT INTO outbox (id, aggregatetype, aggregateid, type, payload)
			VALUES ($1, 'orders', 'o1', $2, '{}')`, uuid.Must(uuid.NewV7()).String(), order.TypeOrderPlaced)
		return err
	}
	open, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = open.Rollback(context.Background()) }()
	if err := insert(open); err != nil {
		t.Fatal(err)
	}
	migrated := make(chan error, 1)
	go func() { migrated <- postgres.Migrate(context.Background(), dsn, log, migrations.All...) }()
	// Wait until the migration waits on a lock behind the open write.
	for waiting := 0; waiting == 0; {
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query ILIKE '%INDEX%outbox%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-migrated:
			t.Fatalf("the migration ended without waiting for the open write: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	if err := insert(pool); err != nil {
		t.Errorf("a write during the upgrade failed: %v", err)
	}
	if err := open.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-migrated; err != nil {
		t.Fatal(err)
	}
	var valid int
	err = pool.QueryRow(ctx, `SELECT count(*) FROM pg_index
		WHERE indexrelid IN ('outbox_unpublished'::regclass, 'outbox_published'::regclass) AND indisvalid`).Scan(&valid)
	if err != nil || valid != 2 {
		t.Errorf("valid outbox indexes = %d, %v; want 2", valid, err)
	}
}

// An index build interrupted by a failure leaves an invalid index; goose runs
// the migration again, which must replace it.
func TestIndexMigrationReplacesAnInvalidIndex(t *testing.T) {
	ctx := t.Context()
	log := slog.New(slog.DiscardHandler)
	dsn := pgtest.NewDSN(t)
	if err := postgres.Migrate(ctx, dsn, log, migrations.All...); err != nil {
		t.Fatal(err)
	}
	pool, err := postgres.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	// As an interrupted CREATE INDEX CONCURRENTLY leaves them, with the
	// migration not recorded.
	for _, q := range []string{
		`UPDATE pg_index SET indisvalid = false WHERE indexrelid IN ('outbox_unpublished'::regclass, 'outbox_published'::regclass)`,
		`DELETE FROM goose.outbox_version WHERE version_id = 2`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := postgres.Migrate(ctx, dsn, log, migrations.All...); err != nil {
		t.Fatal(err)
	}
	var valid int
	err = pool.QueryRow(ctx, `SELECT count(*) FROM pg_index
		WHERE indexrelid IN ('outbox_unpublished'::regclass, 'outbox_published'::regclass) AND indisvalid`).Scan(&valid)
	if err != nil || valid != 2 {
		t.Errorf("valid outbox indexes = %d, %v; want 2", valid, err)
	}
}
