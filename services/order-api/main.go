// Command order-api runs checkout: it prices orders from the catalogue,
// charges them through payment-api and records paid orders with their
// order.placed event in a transactional outbox, which it publishes to the
// broker.
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/thiagogenez/labflow/libs/go/config"
	"github.com/thiagogenez/labflow/libs/go/httpserver"
	"github.com/thiagogenez/labflow/libs/go/idempotency"
	"github.com/thiagogenez/labflow/libs/go/logging"
	"github.com/thiagogenez/labflow/libs/go/outbox"
	"github.com/thiagogenez/labflow/libs/go/postgres"
	"github.com/thiagogenez/labflow/libs/go/rabbitmq"
	"github.com/thiagogenez/labflow/libs/go/telemetry"
	"github.com/thiagogenez/labflow/services/order-api/internal/order"
	"github.com/thiagogenez/labflow/services/order-api/migrations"
)

const service = "order-api"

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
	switch {
	case cmd == "serve":
		return serve(ctx)
	case cmd == "migrate":
		return postgres.MigrateFromEnv(ctx, service, migrations.All...)
	case cmd == "catalogue" && len(args) == 3 && args[1] == "import":
		return importCatalogue(ctx, args[2])
	case cmd == "healthcheck":
		return httpserver.Probe(ctx, config.FromEnviron().String("HTTP_ADDR", ":8080"))
	}
	return fmt.Errorf("unknown command %q: use serve, migrate, catalogue import <file> or healthcheck", cmd)
}

func serve(ctx context.Context) error {
	env := config.FromEnviron()
	level, levelErr := logging.ParseLevel(env.String("LOG_LEVEL", "info"))
	httpCfg := httpserver.ConfigFromEnv(env)
	dsn := postgres.DSNFromEnv(env, "DATABASE_URL")
	paymentURL := env.URL("PAYMENT_API_URL")
	ratesFile := env.Required("FLAT_RATES_FILE")
	brokerURL := rabbitmq.URLFromEnv(env)
	tokenKey, keyErr := base64.StdEncoding.DecodeString(env.Secret("ORDER_TOKEN_KEY"))
	if keyErr != nil {
		keyErr = errors.New("ORDER_TOKEN_KEY_FILE must hold base64")
	}
	if err := errors.Join(levelErr, keyErr, env.Err()); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	tokens, err := order.NewTokens(tokenKey)
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	rates, err := order.LoadFlatRates(ratesFile)
	if err != nil {
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
	store := order.NewStore(pool)
	go store.RunPendingGauge(ctx, 30*time.Second, log)
	svc := order.NewService(store, order.NewPayments(paymentURL), rates, log)
	// The relay runs beside checkout but never gates it: with the broker
	// down, orders are still taken and their events wait in the outbox.
	relay := outbox.NewRelay(pool, rabbitmq.NewPublisher(brokerURL, service+" outbox relay"), log)
	relayCtx, stopRelay := context.WithCancel(ctx)
	var relays sync.WaitGroup
	relays.Go(func() { relay.Run(relayCtx) })
	// Also when the server fails to start; the batch in flight finishes
	// before the pool closes.
	defer relays.Wait()
	defer stopRelay()

	mux := http.NewServeMux()
	order.Register(mux, svc, tokens, idem, log)
	httpCfg.Public = true // the storefront calls checkout directly; its traceparent is only a link (D61)
	return httpserver.New(httpCfg, log, mux, pool.Ping).Run(ctx)
}

// importCatalogue loads the catalogue file (D68). It runs as the application
// role: it changes data, not the schema.
func importCatalogue(ctx context.Context, path string) error {
	env := config.FromEnviron()
	dsn := postgres.DSNFromEnv(env, "DATABASE_URL")
	if err := env.Err(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	log := logging.New(os.Stdout, service, slog.LevelInfo)
	pool, err := postgres.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	res, err := order.Import(ctx, order.NewStore(pool), path)
	if err != nil {
		return err
	}
	log.InfoContext(ctx, "catalogue imported", "file", path, "changed", res.Changed, "unchanged", res.Unchanged, "retired", res.Retired)
	return nil
}
