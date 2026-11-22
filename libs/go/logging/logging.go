// Package logging builds the structured JSON logger every service uses.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// New returns a JSON logger that adds the service name to every record and the
// trace and span IDs of the record's context, so log lines correlate by trace.
func New(w io.Writer, service string, level slog.Level) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(traceHandler{h}).With("service", service)
}

// ParseLevel accepts debug, info, warn or error.
func ParseLevel(s string) (slog.Level, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return 0, fmt.Errorf("invalid log level %q: %w", s, err)
	}
	return l, nil
}

type traceHandler struct{ slog.Handler }

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, r)
}

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{h.Handler.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{h.Handler.WithGroup(name)}
}
