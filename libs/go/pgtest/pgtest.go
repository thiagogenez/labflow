// Package pgtest runs integration tests against a real PostgreSQL in a
// throwaway container. Import it only from tests.
package pgtest

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/thiagogenez/labflow/libs/go/postgres"
)

// Image matches the PostgreSQL image in Docker Compose.
const Image = "postgres:18.7-trixie@sha256:eb63372c1e05c755f55fa30cbd07aecc443676fc6c0fa90b9002d01537841e1a"

var (
	adminDSN string
	dbSeq    atomic.Int64
)

// Main starts one container for a package's tests, runs them and removes it.
// Use it as: func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }.
func Main(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	ctr, err := tcpostgres.Run(ctx, Image,
		tcpostgres.WithDatabase("pgtest"),
		tcpostgres.WithUsername("pgtest"),
		tcpostgres.WithPassword("pgtest"),
		tcpostgres.BasicWaitStrategies(),
	)
	// Run may return a container together with an error; remove it either way.
	defer func() {
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			fmt.Fprintln(os.Stderr, "pgtest: remove postgres container:", err)
		}
	}()
	if err == nil {
		adminDSN, err = ctr.ConnectionString(ctx, "sslmode=disable")
	}
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "pgtest: start postgres container:", err)
		return 1
	}
	return m.Run()
}

// NewDSN creates an empty database and returns its connection string; the
// database is dropped when the test ends.
func NewDSN(t *testing.T) string {
	t.Helper()
	if adminDSN == "" {
		t.Fatal("pgtest: call pgtest.Main from TestMain")
	}
	ctx := t.Context()
	name := fmt.Sprintf("test_%d", dbSeq.Add(1))
	admin, err := postgres.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		admin, err := postgres.Connect(ctx, adminDSN)
		if err != nil {
			t.Error(err)
			return
		}
		defer admin.Close()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	})
	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

// NewDatabase creates an empty database, applies the migrations and returns
// a pool to it. The database is dropped when the test ends.
func NewDatabase(t *testing.T, migrations ...postgres.Migration) *pgxpool.Pool {
	t.Helper()
	dsn := NewDSN(t)
	if err := postgres.Migrate(t.Context(), dsn, slog.New(slog.DiscardHandler), migrations...); err != nil {
		t.Fatal(err)
	}
	pool, err := postgres.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
