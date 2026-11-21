package httpserver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func newTestServer(routes *http.ServeMux, checks ...Check) *Server {
	return New(Config{Addr: ":0"}, slog.New(slog.DiscardHandler), routes, checks...)
}

func serve(s *Server, method, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

func TestReadiness(t *testing.T) {
	healthy := func(context.Context) error { return nil }
	broken := func(context.Context) error { return errors.New("down") }
	tests := []struct {
		name     string
		checks   []Check
		shutdown bool
		want     int
	}{
		{"all checks pass", []Check{healthy}, false, http.StatusNoContent},
		{"a check fails", []Check{healthy, broken}, false, http.StatusServiceUnavailable},
		{"shutting down", []Check{healthy}, true, http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(http.NewServeMux(), tt.checks...)
			s.shuttingDown.Store(tt.shutdown)
			if w := serve(s, http.MethodGet, "/readyz"); w.Code != tt.want {
				t.Fatalf("status %d, want %d", w.Code, tt.want)
			}
		})
	}
}

func TestErrorsAreProblems(t *testing.T) {
	routes := http.NewServeMux()
	routes.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) { panic("boom") })
	routes.HandleFunc("POST /things", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) })
	s := newTestServer(routes)
	tests := []struct {
		name, method, path string
		want               int
		wantAllow          string
	}{
		{"panic", http.MethodGet, "/boom", http.StatusInternalServerError, ""},
		{"unknown route", http.MethodGet, "/nope", http.StatusNotFound, ""},
		{"wrong method", http.MethodGet, "/things", http.StatusMethodNotAllowed, "POST"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := serve(s, tt.method, tt.path)
			if w.Code != tt.want || w.Header().Get("Content-Type") != "application/problem+json" {
				t.Fatalf("status %d, content type %q", w.Code, w.Header().Get("Content-Type"))
			}
			if w.Header().Get("Allow") != tt.wantAllow {
				t.Fatalf("Allow %q, want %q", w.Header().Get("Allow"), tt.wantAllow)
			}
		})
	}
	if w := serve(s, http.MethodPost, "/things"); w.Code != http.StatusCreated {
		t.Fatalf("matched route: status %d", w.Code)
	}
}

func TestRunDrainsAndStops(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"}, slog.New(slog.DiscardHandler), http.NewServeMux())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatalf("Run after cancel: %v", err)
	}
}

// A request in flight when shutdown starts must complete.
func TestShutdownDrainsInFlightRequests(t *testing.T) {
	started := make(chan struct{})
	routes := http.NewServeMux()
	routes.HandleFunc("GET /slow", func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	s := New(Config{Addr: "127.0.0.1:0", ShutdownTimeout: 5 * time.Second}, slog.New(slog.DiscardHandler), routes)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- s.Run(ctx) }()
	for s.addr.Load() == nil {
		time.Sleep(5 * time.Millisecond)
	}

	resp := make(chan int)
	go func() {
		r, err := http.Get("http://" + s.addr.Load().(string) + "/slow")
		if err != nil {
			resp <- 0
			return
		}
		_ = r.Body.Close()
		resp <- r.StatusCode
	}()
	<-started
	cancel()
	if code := <-resp; code != http.StatusOK {
		t.Fatalf("in-flight request got %d during shutdown, want 200", code)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestSpansNameTheServiceRoute(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	routes := http.NewServeMux()
	routes.HandleFunc("GET /v1/things/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	s := newTestServer(routes)
	serve(s, http.MethodGet, "/v1/things/42")
	serve(s, http.MethodGet, "/nope")

	spans := exporter.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("%d spans, want 2", len(spans))
	}
	route := func(i int) string {
		for _, a := range spans[i].Attributes {
			if a.Key == "http.route" {
				return a.Value.AsString()
			}
		}
		return ""
	}
	if spans[0].Name != "GET /v1/things/{id}" || route(0) != "/v1/things/{id}" {
		t.Errorf("matched: span %q route %q", spans[0].Name, route(0))
	}
	if strings.Contains(spans[1].Name, "/") || route(1) != "" {
		t.Errorf("unmatched: span %q route %q, want no route", spans[1].Name, route(1))
	}
}
