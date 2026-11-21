// Package telemetry installs OpenTelemetry tracing for a service.
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/contrib/exporters/autoexport"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

const flushTimeout = 5 * time.Second

// Setup installs the global tracer provider and the W3C Trace Context
// propagator. The exporter follows the standard OTEL_* environment variables;
// OTEL_TRACES_EXPORTER=none keeps trace IDs for log correlation without
// exporting spans. Export errors go to log as JSON. The returned function
// flushes pending spans, waiting at most five seconds.
func Setup(ctx context.Context, service, version string, log *slog.Logger) (func(), error) {
	exporter, err := autoexport.NewSpanExporter(ctx)
	if err != nil {
		return nil, fmt.Errorf("create span exporter: %w", err)
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(service), semconv.ServiceVersion(version)),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, fmt.Errorf("build resource: %w", err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		log.Warn("opentelemetry error", "error", err)
	}))
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
		defer cancel()
		_ = tp.Shutdown(ctx) // best effort: losing the last spans must not fail shutdown
	}, nil
}
