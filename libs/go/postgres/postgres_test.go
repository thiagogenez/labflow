package postgres_test

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"testing/fstest"
	"time"

	"github.com/thiagogenez/labflow/libs/go/pgtest"
	"github.com/thiagogenez/labflow/libs/go/postgres"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

// A migration that runs too long must fail at the caller's deadline.
func TestMigrateStopsAtTheDeadline(t *testing.T) {
	slow := postgres.Migration{Name: "slow_version", FS: fstest.MapFS{
		"00001_slow.sql": {Data: []byte("-- +goose Up\nSELECT pg_sleep(30);\n")},
	}}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	start := time.Now()
	err := postgres.Migrate(ctx, pgtest.NewDSN(t), slog.New(slog.DiscardHandler), slow)
	if err == nil || time.Since(start) > 10*time.Second {
		t.Fatalf("Migrate returned %v after %s, want an error within the deadline", err, time.Since(start))
	}
}

// Two migrators starting together on a fresh database must both succeed.
func TestConcurrentMigrators(t *testing.T) {
	m := postgres.Migration{Name: "db_version", FS: fstest.MapFS{
		"00001_t.sql": {Data: []byte("-- +goose Up\nCREATE TABLE t (id int);\n")},
	}}
	dsn := pgtest.NewDSN(t)
	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- postgres.Migrate(t.Context(), dsn, slog.New(slog.DiscardHandler), m) }()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent migrate: %v", err)
		}
	}
}
