package idempotency_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thiagogenez/labflow/libs/go/idempotency"
	"github.com/thiagogenez/labflow/libs/go/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

// handler echoes the body and counts how often it really ran.
type handler struct {
	calls   atomic.Int32
	status  atomic.Int32
	release chan struct{}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := h.calls.Add(1)
	if h.release != nil {
		<-h.release
	}
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "text/plain")
	if s := h.status.Load(); s != 0 {
		w.WriteHeader(int(s))
	} else {
		w.WriteHeader(http.StatusCreated)
	}
	_, _ = io.WriteString(w, string(body)+" #"+string(rune('0'+n)))
}

func newStore(t *testing.T) *idempotency.Store {
	pool := pgtest.NewDatabase(t, idempotency.Migration)
	return idempotency.NewStore(pool, slog.New(slog.DiscardHandler))
}

func send(h http.Handler, key, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/things", strings.NewReader(body))
	if key != "" {
		r.Header.Set(idempotency.Header, idempotency.Quote(key))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestReplayAndValidation(t *testing.T) {
	inner := &handler{}
	h := newStore(t).Middleware(inner)

	steps := []struct {
		name, key, body string
		wantStatus      int
		wantBody        string
		wantReplayed    bool
		wantCalls       int32
	}{
		{"missing key", "", "a", http.StatusBadRequest, "", false, 0},
		{"key too long", strings.Repeat("k", 256), "a", http.StatusBadRequest, "", false, 0},
		{"first request runs", "k1", "a", http.StatusCreated, "a #1", false, 1},
		{"retry replays", "k1", "a", http.StatusCreated, "a #1", true, 1},
		{"same key, other body", "k1", "b", http.StatusUnprocessableEntity, "", false, 1},
		{"new key runs", "k2", "a", http.StatusCreated, "a #2", false, 2},
	}
	for _, s := range steps {
		w := send(h, s.key, s.body)
		if w.Code != s.wantStatus {
			t.Fatalf("%s: status %d, want %d (%s)", s.name, w.Code, s.wantStatus, w.Body)
		}
		if s.wantBody != "" && w.Body.String() != s.wantBody {
			t.Fatalf("%s: body %q, want %q", s.name, w.Body, s.wantBody)
		}
		if got := w.Header().Get("Idempotent-Replayed") == "true"; got != s.wantReplayed {
			t.Fatalf("%s: replayed %v, want %v", s.name, got, s.wantReplayed)
		}
		if got := inner.calls.Load(); got != s.wantCalls {
			t.Fatalf("%s: handler ran %d times, want %d", s.name, got, s.wantCalls)
		}
	}
}

func TestInFlightRetryGetsConflict(t *testing.T) {
	inner := &handler{release: make(chan struct{})}
	h := newStore(t).Middleware(inner)

	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- send(h, "k", "a") }()
	waitFor(t, func() bool { return inner.calls.Load() == 1 })

	if w := send(h, "k", "a"); w.Code != http.StatusConflict || w.Header().Get("Retry-After") == "" {
		t.Fatalf("in-flight retry: status %d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}
	close(inner.release)
	if w := <-done; w.Code != http.StatusCreated {
		t.Fatalf("first request: status %d", w.Code)
	}
}

func TestServerErrorReleasesKey(t *testing.T) {
	inner := &handler{}
	inner.status.Store(http.StatusServiceUnavailable)
	h := newStore(t).Middleware(inner)

	if w := send(h, "k", "a"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("first: status %d", w.Code)
	}
	inner.status.Store(0)
	if w := send(h, "k", "a"); w.Code != http.StatusCreated || inner.calls.Load() != 2 {
		t.Fatalf("retry after 503: status %d, calls %d", w.Code, inner.calls.Load())
	}
}

func TestExpiredLeaseIsTakenOver(t *testing.T) {
	inner := &handler{release: make(chan struct{})}
	store := newStore(t)
	store.Lease = 50 * time.Millisecond
	h := store.Middleware(inner)

	first := make(chan *httptest.ResponseRecorder)
	go func() { first <- send(h, "k", "a") }()
	waitFor(t, func() bool { return inner.calls.Load() == 1 })
	time.Sleep(100 * time.Millisecond)

	second := make(chan *httptest.ResponseRecorder)
	go func() { second <- send(h, "k", "a") }()
	waitFor(t, func() bool { return inner.calls.Load() == 2 })
	close(inner.release)
	w1, w2 := <-first, <-second
	if w1.Code != http.StatusCreated || w2.Code != http.StatusCreated {
		t.Fatalf("statuses %d and %d", w1.Code, w2.Code)
	}

	// Whichever finished first, the stored outcome is the successor's: the
	// request that lost its lease cannot overwrite it.
	if w := send(h, "k", "a"); w.Body.String() != w2.Body.String() {
		t.Fatalf("replayed %q, want the successor's %q", w.Body, w2.Body)
	}
}

func TestLateServerErrorKeepsSuccessorsOutcome(t *testing.T) {
	store := newStore(t)
	store.Lease = 50 * time.Millisecond
	var calls atomic.Int32
	release := make(chan struct{})
	h := store.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			<-release // the first request overruns its lease, then fails
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))

	first := make(chan *httptest.ResponseRecorder)
	go func() { first <- send(h, "k", "a") }()
	waitFor(t, func() bool { return calls.Load() == 1 })
	time.Sleep(100 * time.Millisecond)
	if w := send(h, "k", "a"); w.Code != http.StatusCreated {
		t.Fatalf("successor: status %d", w.Code)
	}
	close(release)
	if w := <-first; w.Code != http.StatusServiceUnavailable {
		t.Fatalf("late first request: status %d", w.Code)
	}
	if w := send(h, "k", "a"); w.Code != http.StatusCreated || w.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("after the late 503: status %d, replayed %q", w.Code, w.Header().Get("Idempotent-Replayed"))
	}
}

func TestKeyHeaderFormat(t *testing.T) {
	h := newStore(t).Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, idempotency.KeyFrom(r.Context()))
	}))
	tests := []struct {
		name    string
		values  []string
		want    int
		wantKey string
	}{
		{"structured string", []string{`"abc-123"`}, http.StatusOK, "abc-123"},
		{"escaped quote", []string{`"a\"b"`}, http.StatusOK, `a"b`},
		{"string with parameters", []string{`"abc";p=1`}, http.StatusOK, "abc"},
		{"inner list", []string{`"a", "b"`}, http.StatusBadRequest, ""},
		{"bare value", []string{"0190aa52-7a3c-7e41-9d6b-5d2c3a1b9f00"}, http.StatusOK, "0190aa52-7a3c-7e41-9d6b-5d2c3a1b9f00"},
		{"missing", nil, http.StatusBadRequest, ""},
		{"two fields", []string{`"a"`, `"b"`}, http.StatusBadRequest, ""},
		{"empty string", []string{`""`}, http.StatusBadRequest, ""},
		{"unterminated", []string{`"abc`}, http.StatusBadRequest, ""},
		{"bad escape", []string{`"a\nb"`}, http.StatusBadRequest, ""},
		{"space in bare value", []string{"a b"}, http.StatusBadRequest, ""},
		{"too long", []string{`"` + strings.Repeat("k", 256) + `"`}, http.StatusBadRequest, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/things", strings.NewReader(tt.name))
			for _, v := range tt.values {
				r.Header.Add(idempotency.Header, v)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tt.want || (tt.wantKey != "" && w.Body.String() != tt.wantKey) {
				t.Fatalf("status %d body %q, want %d %q", w.Code, w.Body, tt.want, tt.wantKey)
			}
		})
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
