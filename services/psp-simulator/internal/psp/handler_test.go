package psp

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thiagogenez/labflow/libs/go/idempotency"
	"github.com/thiagogenez/labflow/libs/go/pgtest"
	"github.com/thiagogenez/labflow/services/psp-simulator/internal/card"
	"github.com/thiagogenez/labflow/services/psp-simulator/migrations"
)

const (
	pk = "pk_test_local"
	sk = "sk_test_local"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

func newAPI(t *testing.T, cfg Config) (http.Handler, *pgxpool.Pool) {
	t.Helper()
	pool := pgtest.NewDatabase(t, idempotency.Migration, migrations.Migration)
	log := slog.New(slog.DiscardHandler)
	cfg.PublishableKey, cfg.SecretKey = pk, sk
	mux := http.NewServeMux()
	Register(mux, NewStore(pool), idempotency.NewStore(pool, log), log, cfg)
	return mux, pool
}

func call(t *testing.T, h http.Handler, method, path, key, idemKey, body string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	if idemKey != "" {
		r.Header.Set(idempotency.Header, idempotency.Quote(idemKey))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out map[string]any
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s: invalid JSON %q", method, path, w.Body)
		}
	}
	return w.Code, out
}

func token(t *testing.T, h http.Handler, b card.Brand, o card.Outcome) string {
	t.Helper()
	tc, err := card.Generate(b, o, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(tc)
	code, out := call(t, h, http.MethodPost, "/v1/tokens", pk, "", string(body))
	if code != http.StatusCreated {
		t.Fatalf("create token: %d %v", code, out)
	}
	resp, _ := json.Marshal(out)
	if strings.Contains(string(resp), tc.Number) || strings.Contains(string(resp), "exp_") {
		t.Fatalf("the token response leaks card details: %s", resp)
	}
	return out["id"].(string)
}

func chargeBody(tok string) string {
	return `{"amount":13240,"currency":"EUR","token":"` + tok + `","metadata":{"order_id":"o-1"}}`
}

func TestCharges(t *testing.T) {
	h, _ := newAPI(t, Config{SlowDelay: 10 * time.Millisecond})
	tests := []struct {
		name       string
		brand      card.Brand
		outcome    card.Outcome
		wantStatus int
		wantCharge string
		wantCode   string
	}{
		{"approved", card.Visa, card.Approve, http.StatusCreated, StatusSucceeded, ""},
		{"declined", card.Mastercard, card.Decline, http.StatusPaymentRequired, "", "insufficient_funds"},
		{"slow but approved", card.Amex, card.Slow, http.StatusCreated, StatusSucceeded, ""},
		{"pending", card.Visa, card.Pending, http.StatusCreated, StatusPending, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, out := call(t, h, http.MethodPost, "/v1/charges", sk, "key-"+tt.name, chargeBody(token(t, h, tt.brand, tt.outcome)))
			if code != tt.wantStatus {
				t.Fatalf("status %d, want %d: %v", code, tt.wantStatus, out)
			}
			if tt.wantCharge != "" && out["status"] != tt.wantCharge {
				t.Fatalf("charge status %v, want %s", out["status"], tt.wantCharge)
			}
			if tt.wantCode != "" && (out["decline_code"] != tt.wantCode || out["charge"] == nil) {
				t.Fatalf("problem %v, want decline_code %s and a charge id", out, tt.wantCode)
			}
		})
	}
}

func TestPendingChargesSettleWhenDue(t *testing.T) {
	h, pool := newAPI(t, Config{PendingDelay: time.Hour})
	for outcome, want := range map[card.Outcome]string{card.Pending: StatusSucceeded, card.PendingDecline: StatusFailed} {
		_, created := call(t, h, http.MethodPost, "/v1/charges", sk, "key-"+string(outcome), chargeBody(token(t, h, card.Visa, outcome)))
		id := created["id"].(string)
		if _, got := call(t, h, http.MethodGet, "/v1/charges/"+id, sk, "", ""); got["status"] != StatusPending {
			t.Fatalf("%s: settled before it was due: %v", outcome, got["status"])
		}
		if _, err := pool.Exec(context.Background(), `UPDATE charges SET resolve_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		if _, got := call(t, h, http.MethodGet, "/v1/charges/"+id, sk, "", ""); got["status"] != want {
			t.Fatalf("%s: status %v, want %s", outcome, got["status"], want)
		}
	}
}

func TestTokenPaysOnce(t *testing.T) {
	h, _ := newAPI(t, Config{})
	tok := token(t, h, card.Visa, card.Approve)
	_, first := call(t, h, http.MethodPost, "/v1/charges", sk, "k1", chargeBody(tok))

	code, replay := call(t, h, http.MethodPost, "/v1/charges", sk, "k1", chargeBody(tok))
	if code != http.StatusCreated || replay["id"] != first["id"] {
		t.Fatalf("idempotent retry: %d %v, want the first charge %v", code, replay["id"], first["id"])
	}
	if code, out := call(t, h, http.MethodPost, "/v1/charges", sk, "k2", chargeBody(tok)); code != http.StatusBadRequest || out["code"] != "token_already_used" {
		t.Fatalf("second charge on the token: %d %v", code, out)
	}
}

// A retry must find its charge even if the provider died after charging and
// before it stored the idempotent response.
func TestRetryAfterLostResponseReturnsTheCharge(t *testing.T) {
	h, pool := newAPI(t, Config{})
	tok := token(t, h, card.Visa, card.Approve)
	_, first := call(t, h, http.MethodPost, "/v1/charges", sk, "k1", chargeBody(tok))

	// The state a crash between the charge and the stored response leaves.
	if _, err := pool.Exec(context.Background(), `UPDATE idempotency_keys
		SET completed = false, response_status = NULL, response_body = NULL, locked_until = now() - interval '1 second'
		WHERE key = 'k1'`); err != nil {
		t.Fatal(err)
	}
	code, retry := call(t, h, http.MethodPost, "/v1/charges", sk, "k1", chargeBody(tok))
	if code != http.StatusCreated || retry["id"] != first["id"] || retry["status"] != StatusSucceeded {
		t.Fatalf("retry after lost response: %d %v, want the charge %v", code, retry, first["id"])
	}
}

func TestRejectedRequests(t *testing.T) {
	h, _ := newAPI(t, Config{})
	tests := []struct {
		name, method, path, key, idemKey, body string
		want                                   int
		wantCode                               string
	}{
		{"token without key", http.MethodPost, "/v1/tokens", "", "", `{}`, http.StatusUnauthorized, ""},
		{"charge with publishable key", http.MethodPost, "/v1/charges", pk, "k", chargeBody("tok_x"), http.StatusUnauthorized, ""},
		{"charge without idempotency key", http.MethodPost, "/v1/charges", sk, "", chargeBody("tok_x"), http.StatusBadRequest, ""},
		{"unknown token", http.MethodPost, "/v1/charges", sk, "k1", chargeBody("tok_x"), http.StatusBadRequest, "invalid_token"},
		{"negative amount", http.MethodPost, "/v1/charges", sk, "k2", `{"amount":-1,"currency":"EUR","token":"tok_x"}`, http.StatusBadRequest, "invalid_amount"},
		{"lower-case currency", http.MethodPost, "/v1/charges", sk, "k3", `{"amount":1,"currency":"eur","token":"tok_x"}`, http.StatusBadRequest, "invalid_currency"},
		{"control character in metadata", http.MethodPost, "/v1/charges", sk, "k4", `{"amount":1,"currency":"EUR","token":"tok_x","metadata":{"a":"b\u0000"}}`, http.StatusBadRequest, "invalid_metadata"},
		{"bad card", http.MethodPost, "/v1/tokens", pk, "", `{"number":"4111111111111112","exp_month":12,"exp_year":2030,"cvc":"123"}`, http.StatusBadRequest, "incorrect_number"},
		{"expiry year overflow", http.MethodPost, "/v1/tokens", pk, "", `{"number":"4111111111111111","exp_month":12,"exp_year":40000,"cvc":"123"}`, http.StatusBadRequest, "invalid_expiry_year"},
		{"unknown charge", http.MethodGet, "/v1/charges/ch_missing", sk, "", "", http.StatusNotFound, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, out := call(t, h, tt.method, tt.path, tt.key, tt.idemKey, tt.body)
			if code != tt.want || (tt.wantCode != "" && out["code"] != tt.wantCode) {
				t.Fatalf("got %d %v, want %d %s", code, out, tt.want, tt.wantCode)
			}
		})
	}
}

// After the HTTP idempotency record expires, the charge itself still tells an
// identical retry from a different request with the same key.
func TestKeyOutlivesItsHTTPRecord(t *testing.T) {
	h, pool := newAPI(t, Config{})
	tok := token(t, h, card.Visa, card.Approve)
	_, first := call(t, h, http.MethodPost, "/v1/charges", sk, "k1", chargeBody(tok))
	if _, err := pool.Exec(context.Background(), `DELETE FROM idempotency_keys`); err != nil {
		t.Fatal(err)
	}
	if code, retry := call(t, h, http.MethodPost, "/v1/charges", sk, "k1", chargeBody(tok)); code != http.StatusCreated || retry["id"] != first["id"] {
		t.Fatalf("identical retry: %d %v, want the charge %v", code, retry["id"], first["id"])
	}
	other := `{"amount":1,"currency":"EUR","token":"` + tok + `","metadata":{"order_id":"o-1"}}`
	if code, out := call(t, h, http.MethodPost, "/v1/charges", sk, "k1", other); code != http.StatusUnprocessableEntity {
		t.Fatalf("same key, other amount: %d %v, want 422", code, out)
	}
}
