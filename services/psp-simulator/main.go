// Command psp-simulator stands in for an external card payment provider.
// It is a simulation: it never talks to a real card network.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
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
	"github.com/thiagogenez/labflow/services/psp-simulator/internal/card"
	"github.com/thiagogenez/labflow/services/psp-simulator/internal/psp"
	"github.com/thiagogenez/labflow/services/psp-simulator/migrations"
)

const (
	service = "psp-simulator"
	// slowDelayMargin keeps time inside the charge limit for the token lookup
	// and the charge insert around the simulated delay.
	slowDelayMargin = 5 * time.Second
)

// version is reported as service.version; the image pipeline will set it
// with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		logging.New(os.Stderr, service, slog.LevelInfo).Error("exiting", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer) error {
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		return serve(ctx)
	case "migrate":
		return migrate(ctx)
	case "healthcheck":
		return httpserver.Probe(ctx, config.FromEnviron().String("HTTP_ADDR", ":8080"))
	case "cards":
		return cards(args, stdout)
	}
	return fmt.Errorf("unknown command %q: use serve, migrate, healthcheck or cards", cmd)
}

func serve(ctx context.Context) error {
	env := config.FromEnviron()
	level, levelErr := logging.ParseLevel(env.String("LOG_LEVEL", "info"))
	httpCfg := httpserver.ConfigFromEnv(env)
	httpCfg.Public = true // browsers call the tokens endpoint directly
	dsn := env.Required("DATABASE_URL")
	cfg := psp.Config{
		PublishableKey: env.Required("PUBLISHABLE_KEY"),
		SecretKey:      env.Required("SECRET_KEY"),
		SlowDelay:      env.Duration("SLOW_DELAY", 10*time.Second),
		PendingDelay:   env.Duration("PENDING_DELAY", 30*time.Second),
	}
	env.Check(cfg.PublishableKey == "" || cfg.PublishableKey != cfg.SecretKey,
		"PUBLISHABLE_KEY and SECRET_KEY must differ: the publishable key is public")
	env.Check(cfg.SlowDelay <= psp.ChargeTimeout-slowDelayMargin,
		fmt.Sprintf("SLOW_DELAY must leave %s of the %s charge limit for database work", slowDelayMargin, psp.ChargeTimeout))
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

	mux := http.NewServeMux()
	psp.Register(mux, psp.NewStore(pool), idem, log, cfg)
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

// cards prints a valid test card as the JSON body of POST /v1/tokens.
func cards(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("cards", flag.ContinueOnError)
	brand := fs.String("brand", string(card.Visa), "visa, mastercard or amex")
	outcome := fs.String("outcome", string(card.Approve), "approve, decline, slow, pending or pending_decline")
	if err := fs.Parse(args); err != nil {
		return err
	}
	tc, err := card.Generate(card.Brand(*brand), card.Outcome(*outcome), time.Now())
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(tc)
}
