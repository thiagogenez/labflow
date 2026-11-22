// Command payment-api charges orders through the card payment provider and
// keeps each order's payment state.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/thiagogenez/labflow/libs/go/config"
	"github.com/thiagogenez/labflow/libs/go/httpserver"
	"github.com/thiagogenez/labflow/libs/go/idempotency"
	"github.com/thiagogenez/labflow/libs/go/logging"
	"github.com/thiagogenez/labflow/libs/go/postgres"
	"github.com/thiagogenez/labflow/libs/go/telemetry"
	"github.com/thiagogenez/labflow/services/payment-api/internal/payment"
	"github.com/thiagogenez/labflow/services/payment-api/migrations"
)

const service = "payment-api"

// version is reported as service.version; the image pipeline will set it
// with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		logging.New(os.Stderr, service, slog.LevelInfo).Error("exiting", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "serve":
		return serve(ctx)
	case "migrate":
		return migrate(ctx)
	case "healthcheck":
		return httpserver.Probe(ctx, config.FromEnviron().String("HTTP_ADDR", ":8080"))
	}
	return fmt.Errorf("unknown command %q: use serve, migrate or healthcheck", cmd)
}

func serve(ctx context.Context) error {
	env := config.FromEnviron()
	level, levelErr := logging.ParseLevel(env.String("LOG_LEVEL", "info"))
	httpCfg := httpserver.ConfigFromEnv(env)
	dsn := env.Required("DATABASE_URL")
	pspURL := env.URL("PSP_BASE_URL")
	pspKey := env.Required("PSP_SECRET_KEY")
	pspTimeout := env.Duration("PSP_TIMEOUT", 3*time.Second)
	resolveInterval := env.Duration("RESOLVE_INTERVAL", 10*time.Second)
	if err := errors.Join(levelErr, env.Err()); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	log := logging.New(os.Stdout, service, level)

	flushTraces, err := telemetry.Setup(ctx, service, version, log)
	if err != nil {
		return err
	}
	defer flushTraces()

	pool, err := postgres.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	idem := idempotency.NewStore(pool, log)
	go idem.RunJanitor(ctx, 10*time.Minute)
	svc := payment.NewService(payment.NewStore(pool), payment.NewProvider(pspURL, pspKey, pspTimeout), log)
	go svc.RunResolver(ctx, resolveInterval)

	mux := http.NewServeMux()
	payment.Register(mux, svc, idem, log)
	return httpserver.New(httpCfg, log, mux, pool.Ping).Run(ctx)
}

func migrate(ctx context.Context) error {
	env := config.FromEnviron()
	dsn := env.Required("MIGRATIONS_DATABASE_URL")
	// Long enough for real DDL, short enough that a stuck migration fails the
	// job instead of holding the service's start forever.
	timeout := env.Duration("MIGRATION_TIMEOUT", 10*time.Minute)
	if err := env.Err(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	log := logging.New(os.Stdout, service, slog.LevelInfo)
	return postgres.Migrate(ctx, dsn, log, idempotency.Migration, migrations.Migration)
}
