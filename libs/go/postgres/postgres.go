// Package postgres opens connection pools and runs schema migrations.
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/thiagogenez/labflow/libs/go/config"
	"github.com/thiagogenez/labflow/libs/go/logging"
)

// Every statement and lock wait is bounded, so a stalled database fails a
// request instead of holding it, its connection and shutdown.
const (
	connectTimeout   = 5 * time.Second
	statementTimeout = "5s"
	lockTimeout      = "2s"
	// Migrations may run long statements, but must not queue behind a long
	// transaction while every query waits behind them; the job retries.
	migrationLockTimeout = "5s"
	// schemaLockID serialises creating the goose schema across migrators.
	schemaLockID = 7_271_011
)

// Connect opens a pool and checks that the database answers.
func Connect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse postgres DSN: %w", err)
	}
	cfg.ConnConfig.ConnectTimeout = connectTimeout
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = statementTimeout
	cfg.ConnConfig.RuntimeParams["lock_timeout"] = lockTimeout
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open postgres pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return pool, nil
}

// Migration is one set of goose SQL migrations with its own version table, so
// a shared library and the service can each own part of a schema.
type Migration struct {
	FS fs.FS
	// Name names the version table goose.<Name> in the goose schema, which the
	// application role cannot write to.
	Name string
}

// NewMigration returns the goose migrations embedded under dir in fsys, with
// the version table goose.<name>.
func NewMigration(fsys fs.FS, dir, name string) Migration {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err) // the embedded directory is fixed at build time
	}
	return Migration{FS: sub, Name: name}
}

// Migrate applies the migrations in order. A session advisory lock lets
// several replicas start at once without racing.
func Migrate(ctx context.Context, dsn string, log *slog.Logger, migrations ...Migration) error {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("parse migration DSN: %w", err)
	}
	cfg.ConnectTimeout = connectTimeout
	cfg.RuntimeParams["lock_timeout"] = migrationLockTimeout
	db := stdlib.OpenDB(*cfg)
	defer func() { _ = db.Close() }() // nothing to recover once migrations ran or failed
	if err := createGooseSchema(ctx, db); err != nil {
		return err
	}
	for _, m := range migrations {
		locker, err := lock.NewPostgresSessionLocker()
		if err != nil {
			return fmt.Errorf("create migration lock: %w", err)
		}
		p, err := goose.NewProvider(goose.DialectPostgres, db, m.FS,
			goose.WithTableName("goose."+m.Name), goose.WithSessionLocker(locker), goose.WithSlog(log))
		if err != nil {
			return fmt.Errorf("prepare migrations %s: %w", m.Name, err)
		}
		results, err := p.Up(ctx)
		if err != nil {
			return fmt.Errorf("apply migrations %s: %w", m.Name, err)
		}
		log.InfoContext(ctx, "migrations applied", "set", m.Name, "count", len(results))
	}
	return nil
}

// createGooseSchema creates the version-table schema under an advisory lock,
// so migrators starting together on a fresh database do not collide.
func createGooseSchema(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("create goose schema: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", schemaLockID); err != nil {
		return fmt.Errorf("lock goose schema: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS goose"); err != nil {
		return fmt.Errorf("create goose schema: %w", err)
	}
	return tx.Commit()
}

// MigrateFromEnv runs a service's `migrate` command: it applies the
// migrations to MIGRATIONS_DATABASE_URL, the schema owner's connection,
// within MIGRATION_TIMEOUT. The timeout is long enough for real DDL and short
// enough that a stuck migration fails the job instead of holding the
// service's start forever.
func MigrateFromEnv(ctx context.Context, service string, migrations ...Migration) error {
	env := config.FromEnviron()
	dsn := env.Required("MIGRATIONS_DATABASE_URL")
	timeout := env.Duration("MIGRATION_TIMEOUT", 10*time.Minute)
	if err := env.Err(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return Migrate(ctx, dsn, logging.New(os.Stdout, service, slog.LevelInfo), migrations...)
}
