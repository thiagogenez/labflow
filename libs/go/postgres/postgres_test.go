package postgres_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/thiagogenez/labflow/libs/go/config"
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

func TestMigrateFromEnvNeedsItsConnection(t *testing.T) {
	t.Setenv("MIGRATIONS_DATABASE_URL", "")
	if err := postgres.MigrateFromEnv(t.Context(), "test"); err == nil || !strings.Contains(err.Error(), "MIGRATIONS_DATABASE_URL") {
		t.Fatalf("MigrateFromEnv without a connection: %v, want a configuration error", err)
	}
}

func TestDSNFromEnv(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(secret, []byte("p@ss/w0rd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, url, want, wantErr string
	}{
		{name: "password from the file, escaped", url: "postgres://orders_app@postgres:5432/orders?sslmode=disable",
			want: "postgres://orders_app:p%40ss%2Fw0rd@postgres:5432/orders?sslmode=disable"},
		{name: "password in the URL", url: "postgres://orders_app:0ldPass@postgres/orders", wantErr: "DATABASE_URL must not carry a password"},
		{name: "password in the query", url: "postgres://orders_app@postgres/orders?password=0ldPass", wantErr: "DATABASE_URL must not carry a password"},
		{name: "encoded query key", url: "postgres://orders_app@postgres/orders?%70assword=0ldPass", wantErr: "DATABASE_URL must not carry a password"},
		{name: "query key padded with spaces", url: "postgres://orders_app@postgres/orders?%20password%20=0ldPass", wantErr: "DATABASE_URL must not carry a password"},
		{name: "query pgx splits differently", url: "postgres://orders_app@postgres/orders?password=0ldPass;x", wantErr: "DATABASE_URL must not carry a password"},
		{name: "passfile in the query", url: "postgres://orders_app@postgres/orders?passfile=/tmp/pg", wantErr: "DATABASE_URL must not carry a password"},
		{name: "key password in the query", url: "postgres://orders_app@postgres/orders?sslpassword=0ldPass", wantErr: "DATABASE_URL must not carry a password"},
		{name: "no user", url: "postgres://postgres/orders", wantErr: "DATABASE_URL must be a postgres:// URL"},
		{name: "not postgres", url: "mysql://orders_app@db/orders", wantErr: "DATABASE_URL must be a postgres:// URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", tc.url)
			t.Setenv("DATABASE_PASSWORD_FILE", secret)
			env := config.FromEnviron()
			got := postgres.DSNFromEnv(env, "DATABASE_URL")
			err := env.Err()
			if tc.wantErr == "" && (err != nil || got != tc.want) {
				t.Errorf("DSNFromEnv = %q, %v; want %q", got, err, tc.want)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Errorf("error = %v, want %q", err, tc.wantErr)
			}
			if err != nil && (strings.Contains(err.Error(), "0ldPass") || strings.Contains(err.Error(), "p@ss")) {
				t.Errorf("error repeats a password: %v", err)
			}
		})
	}
}
