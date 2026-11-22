package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/thiagogenez/labflow/libs/go/logging"
)

func TestLoggerAddsServiceAndTrace(t *testing.T) {
	var buf bytes.Buffer
	log := logging.New(&buf, "payment-api", slog.LevelInfo)
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{0x4b, 0xf9}, SpanID: trace.SpanID{0x01}, TraceFlags: trace.FlagsSampled,
	})
	log.InfoContext(trace.ContextWithSpanContext(context.Background(), sc), "hello")

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["service"] != "payment-api" || got["trace_id"] != sc.TraceID().String() || got["span_id"] != sc.SpanID().String() {
		t.Fatalf("missing correlation fields: %v", got)
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{"debug": slog.LevelDebug, "INFO": slog.LevelInfo, "warn": slog.LevelWarn} {
		if got, err := logging.ParseLevel(in); err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := logging.ParseLevel("loud"); err == nil {
		t.Error("ParseLevel accepted an invalid level")
	}
}
