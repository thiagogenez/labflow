package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thiagogenez/labflow/libs/go/idempotency"
	"github.com/thiagogenez/labflow/libs/go/pgtest"
	"github.com/thiagogenez/labflow/services/payment-api/migrations"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

// fakeProvider answers charges with a scripted response and records the
// idempotency keys it received.
type fakeProvider struct {
	mu     sync.Mutex
	charge func(w http.ResponseWriter)
	get    func(w http.ResponseWriter)
	keys   []string
}

func (f *fakeProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	charge, get := f.charge, f.get
	if r.Method == http.MethodPost {
		f.keys = append(f.keys, r.Header.Get(idempotency.Header))
	}
	f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer sk_test" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodPost {
		charge(w)
		return
	}
	get(w)
}

func (f *fakeProvider) script(charge, get func(http.ResponseWriter)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.charge, f.get = charge, get
}

func (f *fakeProvider) chargeKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keys...)
}

func respond(status int, body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func delayed(d time.Duration, then func(http.ResponseWriter)) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		time.Sleep(d)
		then(w)
	}
}

var chargeSeq atomic.Int64

// approved answers like a provider that charged the card, with a fresh charge ID.
func approved(w http.ResponseWriter) {
	respond(http.StatusCreated, `{"id":"ch_`+strconv.FormatInt(chargeSeq.Add(1), 10)+`","status":"succeeded"}`)(w)
}

var hang = delayed(200*time.Millisecond, approved)

type env struct {
	api      http.Handler
	svc      *Service
	provider *fakeProvider
	pool     *pgxpool.Pool
}

func newEnv(t *testing.T) env { return newEnvLogging(t, io.Discard) }

func newEnvLogging(t *testing.T, logs io.Writer) env {
	t.Helper()
	pool := pgtest.NewDatabase(t, idempotency.Migration, migrations.Migration)
	fake := &fakeProvider{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL)
	log := slog.New(slog.NewJSONHandler(logs, nil))
	svc := NewService(NewStore(pool), NewProvider(base, "sk_test", 50*time.Millisecond), log)
	mux := http.NewServeMux()
	Register(mux, svc, idempotency.NewStore(pool, log), log)
	return env{api: mux, svc: svc, provider: fake, pool: pool}
}

// ageAll makes every pending payment claimable by a resolver with a lease
// shorter than a minute.
func (e env) ageAll(t *testing.T) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE payments SET updated_at = now() - interval '1 minute' WHERE status = 'pending'`); err != nil {
		t.Fatal(err)
	}
}

// resolve runs one bounded resolver pass, as RunResolver does.
func (e env) resolve(t *testing.T) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return e.svc.ResolvePending(ctx, 30*time.Second)
}

func orderID() string { return uuid.Must(uuid.NewV7()).String() }

func (e env) pay(t *testing.T, id string, amount int64) (int, map[string]any) {
	t.Helper()
	body := `{"order_id":"` + id + `","token":"tok_1","amount":` + strconv.FormatInt(amount, 10) + `,"currency":"EUR"}`
	r := httptest.NewRequest(http.MethodPost, "/v1/payments", strings.NewReader(body))
	r.Header.Set(idempotency.Header, idempotency.Quote(uuid.NewString()))
	return e.do(t, r)
}

func (e env) status(t *testing.T, id string) string {
	t.Helper()
	_, out := e.do(t, httptest.NewRequest(http.MethodGet, "/v1/payments/"+id, nil))
	s, _ := out["status"].(string)
	return s
}

func (e env) do(t *testing.T, r *http.Request) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	e.api.ServeHTTP(w, r)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON %q", w.Body)
	}
	return w.Code, out
}

func TestPay(t *testing.T) {
	tests := []struct {
		name       string
		charge     func(http.ResponseWriter)
		wantCode   int
		wantStatus string
	}{
		{"approved", approved, http.StatusCreated, StatusSucceeded},
		{"declined", respond(http.StatusPaymentRequired, `{"charge":"ch_2","code":"card_declined","decline_code":"insufficient_funds"}`), http.StatusPaymentRequired, StatusFailed},
		{"request rejected", respond(http.StatusBadRequest, `{"code":"invalid_token"}`), http.StatusPaymentRequired, StatusFailed},
		{"token spent by another order", respond(http.StatusBadRequest, `{"code":"token_already_used"}`), http.StatusPaymentRequired, StatusFailed},
		{"decline without details", respond(http.StatusPaymentRequired, `{}`), http.StatusAccepted, StatusPending},
		{"unknown 400 code", respond(http.StatusBadRequest, `{"code":"something_new"}`), http.StatusAccepted, StatusPending},
		{"contradictory decline", respond(http.StatusPaymentRequired, `{"charge":"ch_x","code":"rate_limited","decline_code":"x"}`), http.StatusAccepted, StatusPending},
		{"oversized response", respond(http.StatusCreated, `{"id":"ch_big","status":"succeeded"}`+strings.Repeat(" ", maxResponseBytes)), http.StatusAccepted, StatusPending},
		{"pending at provider", respond(http.StatusCreated, `{"id":"ch_3","status":"pending"}`), http.StatusAccepted, StatusPending},
		{"provider timeout", hang, http.StatusAccepted, StatusPending},
		{"provider error", respond(http.StatusInternalServerError, `{}`), http.StatusAccepted, StatusPending},
		{"provider busy", respond(http.StatusConflict, `{}`), http.StatusAccepted, StatusPending},
		{"success without a charge id", respond(http.StatusCreated, `{"status":"succeeded"}`), http.StatusAccepted, StatusPending},
		{"unknown charge status", respond(http.StatusCreated, `{"id":"ch_4","status":"canceled"}`), http.StatusAccepted, StatusPending},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.provider.script(tt.charge, nil)
			id := orderID()
			code, out := e.pay(t, id, 13240)
			if code != tt.wantCode {
				t.Fatalf("status %d, want %d: %v", code, tt.wantCode, out)
			}
			if got := e.status(t, id); got != tt.wantStatus {
				t.Fatalf("stored status %q, want %q", got, tt.wantStatus)
			}
		})
	}
}

func TestResolverSettlesUnknownOutcomeWithTheSameKey(t *testing.T) {
	e := newEnv(t)
	e.provider.script(hang, nil)
	id := orderID()
	if code, _ := e.pay(t, id, 13240); code != http.StatusAccepted {
		t.Fatalf("first attempt: status %d, want 202", code)
	}

	e.provider.script(respond(http.StatusCreated, `{"id":"ch_9","status":"succeeded"}`), nil)
	e.ageAll(t)
	if n := e.resolve(t); n != 1 {
		t.Fatalf("ResolvePending tried %d payments, want 1", n)
	}
	if got := e.status(t, id); got != StatusSucceeded {
		t.Fatalf("status %q after resolving, want succeeded", got)
	}
	want := idempotency.Quote(id)
	if keys := e.provider.chargeKeys(); len(keys) != 2 || keys[0] != want || keys[1] != want {
		t.Fatalf("provider saw idempotency keys %v, want %s twice", keys, want)
	}
}

func TestResolverReadsPendingCharges(t *testing.T) {
	e := newEnv(t)
	e.provider.script(respond(http.StatusCreated, `{"id":"ch_5","status":"pending"}`),
		respond(http.StatusOK, `{"id":"ch_5","status":"failed","failure_code":"insufficient_funds"}`))
	id := orderID()
	e.pay(t, id, 500)
	e.ageAll(t)
	e.resolve(t)
	if got := e.status(t, id); got != StatusFailed {
		t.Fatalf("status %q, want failed", got)
	}
	if n := len(e.provider.chargeKeys()); n != 1 {
		t.Fatalf("the resolver charged again (%d charges) instead of reading the charge", n)
	}
}

// Two resolvers sharing the queue settle every payment, each exactly once.
func TestConcurrentResolversChargeEachPaymentOnce(t *testing.T) {
	e := newEnv(t)
	e.provider.script(respond(http.StatusInternalServerError, `{}`), nil)
	const payments = 8
	want := map[string]bool{}
	for range payments {
		id := orderID()
		want[idempotency.Quote(id)] = true
		e.pay(t, id, 100)
	}
	e.ageAll(t)
	e.provider.script(delayed(20*time.Millisecond, approved), nil)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			<-start
			e.resolve(t)
		})
	}
	close(start)
	wg.Wait()

	seen := map[string]int{}
	for _, k := range e.provider.chargeKeys()[payments:] {
		seen[k]++
	}
	if len(seen) != payments {
		t.Fatalf("resolvers charged %d distinct payments, want %d", len(seen), payments)
	}
	for k, n := range seen {
		if !want[k] || n != 1 {
			t.Fatalf("payment %s was charged %d times by the resolvers", k, n)
		}
	}
	for k := range want {
		if got := e.status(t, strings.Trim(k, `"`)); got != StatusSucceeded {
			t.Fatalf("payment %s is %q after resolving, want succeeded", k, got)
		}
	}
}

func TestSettledPaymentIsNotChargedAgain(t *testing.T) {
	e := newEnv(t)
	e.provider.script(approved, nil)
	id := orderID()
	e.pay(t, id, 100)
	if code, _ := e.pay(t, id, 100); code != http.StatusCreated || len(e.provider.chargeKeys()) != 1 {
		t.Fatalf("repeat: status %d, %d charges", code, len(e.provider.chargeKeys()))
	}
	if code, _ := e.pay(t, id, 200); code != http.StatusConflict {
		t.Fatalf("same order, other amount: status %d, want 409", code)
	}
}

func TestValidation(t *testing.T) {
	e := newEnv(t)
	id := orderID()
	for name, body := range map[string]string{
		"bad order id":          `{"order_id":"42","token":"tok_1","amount":1,"currency":"EUR"}`,
		"urn order id":          `{"order_id":"urn:uuid:` + id + `","token":"tok_1","amount":1,"currency":"EUR"}`,
		"upper-case order id":   `{"order_id":"` + strings.ToUpper(id) + `","token":"tok_1","amount":1,"currency":"EUR"}`,
		"uuid v4 order id":      `{"order_id":"` + uuid.NewString() + `","token":"tok_1","amount":1,"currency":"EUR"}`,
		"zero amount":           `{"order_id":"` + id + `","token":"tok_1","amount":0,"currency":"EUR"}`,
		"huge amount":           `{"order_id":"` + id + `","token":"tok_1","amount":9007199254740992,"currency":"EUR"}`,
		"usd":                   `{"order_id":"` + id + `","token":"tok_1","amount":1,"currency":"USD"}`,
		"missing token":         `{"order_id":"` + id + `","amount":1,"currency":"EUR"}`,
		"control char in token": `{"order_id":"` + id + `","token":"tok\u0000","amount":1,"currency":"EUR"}`,
		"unknown field":         `{"order_id":"` + id + `","token":"tok_1","amount":1,"currency":"EUR","card":"4111"}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/payments", strings.NewReader(body))
			r.Header.Set(idempotency.Header, idempotency.Quote(uuid.NewString()))
			if code, out := e.do(t, r); code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %v", code, out)
			}
		})
	}
	if code, _ := e.do(t, httptest.NewRequest(http.MethodGet, "/v1/payments/urn:uuid:"+id, nil)); code != http.StatusBadRequest {
		t.Fatalf("GET with a URN: status %d, want 400", code)
	}
}

func TestFailedChargeWithoutCodeStaysPending(t *testing.T) {
	e := newEnv(t)
	e.provider.script(respond(http.StatusCreated, `{"id":"ch_5","status":"pending"}`),
		respond(http.StatusOK, `{"id":"ch_5","status":"failed"}`))
	id := orderID()
	e.pay(t, id, 500)
	e.ageAll(t)
	e.resolve(t)
	if got := e.status(t, id); got != StatusPending {
		t.Fatalf("status %q, want pending: a failure without its code is malformed", got)
	}
}

func TestProviderContentStaysOutOfLogs(t *testing.T) {
	var logs bytes.Buffer
	e := newEnvLogging(t, &logs)
	e.provider.script(respond(http.StatusBadRequest, `{"code":"MARKER-FROM-UPSTREAM"}`), nil)
	e.pay(t, orderID(), 100)
	if strings.Contains(logs.String(), "MARKER-FROM-UPSTREAM") {
		t.Fatalf("provider content reached the logs: %s", logs.String())
	}
}
