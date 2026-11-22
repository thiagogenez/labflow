// Package httpserver runs an HTTP service with the probes, metrics, tracing,
// request logging and graceful shutdown every LabFlow service needs.
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/thiagogenez/labflow/libs/go/api"
	"github.com/thiagogenez/labflow/libs/go/config"
)

const (
	readinessTimeout = 2 * time.Second
	// requestTimeout bounds a request's own work, below the 30 s write
	// timeout; work a handler detaches on purpose sets its own limit.
	requestTimeout = 20 * time.Second
)

var (
	requests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_server_requests_total",
		Help: "HTTP requests served, by method, route pattern and status code.",
	}, []string{"method", "route", "code"})
	duration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_server_request_duration_seconds",
		Help:    "HTTP request duration, by method and route pattern.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "route"})
)

// Config holds the listener and shutdown settings.
type Config struct {
	Addr string
	// ShutdownDelay keeps serving, with readiness failing, so load balancers
	// stop routing before connections drain.
	ShutdownDelay   time.Duration
	ShutdownTimeout time.Duration
	// Public marks an internet-facing service: an inbound traceparent becomes
	// a span link instead of the parent, so clients cannot choose trace IDs.
	Public bool
}

// ConfigFromEnv reads HTTP_ADDR, SHUTDOWN_DELAY and SHUTDOWN_TIMEOUT.
func ConfigFromEnv(e *config.Env) Config {
	return Config{
		Addr:            e.String("HTTP_ADDR", ":8080"),
		ShutdownDelay:   e.Duration("SHUTDOWN_DELAY", 5*time.Second),
		ShutdownTimeout: e.Duration("SHUTDOWN_TIMEOUT", 20*time.Second),
	}
}

// Check reports whether a dependency the service needs is available.
type Check func(context.Context) error

// Server serves app behind /healthz, /readyz and /metrics.
type Server struct {
	cfg          Config
	log          *slog.Logger
	srv          *http.Server
	checks       []Check
	shuttingDown atomic.Bool
	addr         atomic.Value // the bound address, once listening
}

// New builds a server around the service's routes. Requests other than the
// probes are traced, measured and logged with their route pattern; unknown
// routes and methods answer with problem details; a panic becomes a 500.
func New(cfg Config, log *slog.Logger, routes *http.ServeMux, checks ...Check) *Server {
	s := &Server{cfg: cfg, log: log, checks: checks}
	traced := otelhttp.NewHandler(s.observe(problemFallback(routes)), "http.server",
		otelhttp.WithPublicEndpointFn(func(*http.Request) bool { return cfg.Public }),
	)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /readyz", s.ready)
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hide this catch-all pattern from otelhttp: spans take the service's
		// route, or no route when nothing matches.
		r.Pattern = ""
		traced.ServeHTTP(w, r)
	}))
	s.srv = &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	return s
}

// Run serves until ctx is cancelled, then fails readiness, waits
// ShutdownDelay and drains in-flight requests within ShutdownTimeout.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("http server: %w", err)
	}
	s.addr.Store(ln.Addr().String())
	errc := make(chan error, 1)
	go func() { errc <- s.srv.Serve(ln) }()
	s.log.Info("http server listening", "addr", ln.Addr().String())

	select {
	case err := <-errc:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}
	s.shuttingDown.Store(true)
	s.log.Info("shutting down", "readiness_grace", s.cfg.ShutdownDelay.String())
	select {
	case err := <-errc:
		return fmt.Errorf("http server: %w", err)
	case <-time.After(s.cfg.ShutdownDelay):
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.ShutdownTimeout)
	defer cancel()
	if err := s.srv.Shutdown(sctx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	s.log.Info("http server stopped")
	return nil
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if s.shuttingDown.Load() {
		api.Error(w, r, http.StatusServiceUnavailable, "shutting down")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
	defer cancel()
	for _, check := range s.checks {
		if err := check(ctx); err != nil {
			s.log.WarnContext(ctx, "readiness check failed", "error", err)
			api.Error(w, r, http.StatusServiceUnavailable, "a dependency is unavailable")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// observe records metrics and one log line per request, after the router has
// set the route pattern, and turns a panic into a 500 response.
func (s *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		inner := r.WithContext(ctx)
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v) // net/http's signal to abort the response silently
				}
				s.log.ErrorContext(r.Context(), "panic serving request", "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
				if !rec.wrote {
					api.Error(rec, r, http.StatusInternalServerError, "internal error")
				}
			}
			// Expose the router's pattern to otelhttp, which names the span
			// after it once this handler returns.
			r.Pattern = inner.Pattern
			route := r.Pattern
			if route == "" {
				route = "unmatched"
			} else {
				// otelhttp saw the outer catch-all route; record the path
				// template, without the method, as the conventions define it.
				trace.SpanFromContext(r.Context()).SetAttributes(semconv.HTTPRoute(route[strings.IndexByte(route, '/'):]))
			}
			method := knownMethod(r.Method)
			elapsed := time.Since(start)
			requests.WithLabelValues(method, route, fmt.Sprint(rec.status)).Inc()
			duration.WithLabelValues(method, route).Observe(elapsed.Seconds())
			s.log.InfoContext(r.Context(), "request",
				"method", r.Method, "route", route, "status", rec.status, "duration_ms", elapsed.Milliseconds())
		}()
		next.ServeHTTP(rec, inner)
	})
}

// knownMethod keeps the method label bounded.
func knownMethod(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return m
	}
	return "OTHER"
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status, r.wrote = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Probe calls the readiness endpoint on addr; container health checks use it
// because distroless images have no shell or curl.
func Probe(ctx context.Context, addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("parse address %q: %w", addr, err)
	}
	ctx, cancel := context.WithTimeout(ctx, readinessTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+port+"/readyz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }() // a failed close of a read body loses nothing
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("readyz returned %d", resp.StatusCode)
	}
	return nil
}

// problemFallback answers requests that match no route, or match a route with
// another method, with problem details instead of ServeMux's plain text.
func problemFallback(routes *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := routes.Handler(r); pattern != "" {
			routes.ServeHTTP(w, r)
			return
		}
		probe := &headerRecorder{header: http.Header{}, status: http.StatusNotFound}
		routes.ServeHTTP(probe, r)
		if allow := probe.header.Get("Allow"); allow != "" {
			w.Header().Set("Allow", allow)
		}
		api.Error(w, r, probe.status, "")
	})
}

// headerRecorder keeps the status and headers ServeMux chose and drops its body.
type headerRecorder struct {
	header http.Header
	status int
}

func (h *headerRecorder) Header() http.Header         { return h.header }
func (h *headerRecorder) WriteHeader(code int)        { h.status = code }
func (h *headerRecorder) Write(b []byte) (int, error) { return len(b), nil }
