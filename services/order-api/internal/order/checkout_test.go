package order

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/santhosh-tekuri/jsonschema/v6"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/thiagogenez/labflow/libs/go/idempotency"
	"github.com/thiagogenez/labflow/libs/go/pgtest"
	"github.com/thiagogenez/labflow/services/order-api/migrations"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

// payRequest is what order-api sends payment-api.
type payRequest struct {
	Key      string `json:"-"`
	OrderID  string `json:"order_id"`
	Token    string `json:"token"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

// fakePayments stands in for payment-api: it records every charge request and
// answers as respond says, by default as payment-api does for a paid charge.
type fakePayments struct {
	mu       sync.Mutex
	requests []payRequest
	respond  func(payRequest) (int, string)
}

func (f *fakePayments) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req payRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	req.Key = r.Header.Get(idempotency.Header)
	f.mu.Lock()
	f.requests = append(f.requests, req)
	respond := f.respond
	f.mu.Unlock()
	status, body := respond(req)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func (f *fakePayments) answer(respond func(payRequest) (int, string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.respond = respond
}

func (f *fakePayments) calls() []payRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]payRequest(nil), f.requests...)
}

// payment-api's answers, as its handler renders them.
func paid(req payRequest) (int, string) {
	return http.StatusCreated, fmt.Sprintf(`{"order_id":%q,"status":"succeeded","amount":%d,"currency":%q,"provider_charge_id":"ch_1"}`,
		req.OrderID, req.Amount, req.Currency)
}

func pendingAtProvider(req payRequest) (int, string) {
	return http.StatusAccepted, fmt.Sprintf(`{"order_id":%q,"status":"pending","amount":%d,"currency":%q}`, req.OrderID, req.Amount, req.Currency)
}

func declined(req payRequest) (int, string) {
	return http.StatusPaymentRequired, fmt.Sprintf(`{"type":"about:blank","title":"Payment Required","status":402,`+
		`"detail":"the payment was declined","code":"payment_declined","decline_code":"insufficient_funds","order_id":%q}`, req.OrderID)
}

func fixed(status int, body string) func(payRequest) (int, string) {
	return func(payRequest) (int, string) { return status, body }
}

type testAPI struct {
	h      http.Handler
	pool   *pgxpool.Pool
	pay    *fakePayments
	tokens *Tokens
}

func newAPI(t *testing.T) *testAPI {
	t.Helper()
	pool := pgtest.NewDatabase(t, idempotency.Migration, migrations.Migration)
	store := NewStore(pool)
	if _, err := Import(t.Context(), store, "../../catalogue.json"); err != nil {
		t.Fatal(err)
	}
	pay := &fakePayments{respond: paid}
	srv := httptest.NewServer(pay)
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL)
	tokens, _ := NewTokens([]byte(strings.Repeat("k", 32)))
	log := slog.New(slog.DiscardHandler)
	mux := http.NewServeMux()
	Register(mux, NewService(store, NewPayments(base), testRates(t), log), tokens, idempotency.NewStore(pool, log), log)
	// Every request runs in a span, as behind libs/go/httpserver.
	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	traced := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, span := tp.Tracer("test").Start(r.Context(), "request")
		defer span.End()
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
	return &testAPI{h: traced, pool: pool, pay: pay, tokens: tokens}
}

func checkoutBody(mutate func(map[string]any)) string {
	req := map[string]any{
		"customer":         map[string]any{"email": "ada@example.com", "first_name": "Ada", "last_name": "Lovelace"},
		"shipping_address": map[string]any{"line1": "Musterstraße 1", "line2": "", "postal_code": "10115", "city": "Berlin", "country": "DE"},
		"shipping_speed":   "standard",
		"lines":            []any{map[string]any{"sku": "PIP-ADJ-100", "quantity": 1}, map[string]any{"sku": "NBK-A4-100", "quantity": 3}},
		"payment_token":    "tok_test",
		"expected_total":   13240, // 89.00 + 3 × 12.50 + 5.90 shipping
	}
	if mutate != nil {
		mutate(req)
	}
	b, _ := json.Marshal(req)
	return string(b)
}

func (a *testAPI) do(t *testing.T, method, path, key, auth, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		r.Header.Set(idempotency.Header, idempotency.Quote(key))
	}
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	a.h.ServeHTTP(w, r)
	var out map[string]any
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s: invalid JSON %q", method, path, w.Body)
		}
	}
	return w, out
}

func (a *testAPI) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := a.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (a *testAPI) exec(t *testing.T, query string) {
	t.Helper()
	if _, err := a.pool.Exec(context.Background(), query); err != nil {
		t.Fatal(err)
	}
}

func TestCheckoutOutcomes(t *testing.T) {
	const other = "01a1f874-3be8-7fa4-95f5-868d7d69c709"
	tests := []struct {
		name       string
		respond    func(payRequest) (int, string)
		wantCode   int
		wantStatus string
		wantEvents int
	}{
		{"paid", paid, http.StatusCreated, StatusPaid, 1},
		{"declined", declined, http.StatusPaymentRequired, StatusPaymentFailed, 0},
		{"pending at the provider", pendingAtProvider, http.StatusAccepted, StatusPendingPayment, 0},
		{"payment-api failing", fixed(http.StatusServiceUnavailable, `{}`), http.StatusAccepted, StatusPendingPayment, 0},
		{"paid answer for another order", func(r payRequest) (int, string) { r.OrderID = other; return paid(r) },
			http.StatusAccepted, StatusPendingPayment, 0},
		{"paid answer for another amount", func(r payRequest) (int, string) { r.Amount = 1; return paid(r) },
			http.StatusAccepted, StatusPendingPayment, 0},
		{"decline for another order", func(r payRequest) (int, string) { r.OrderID = other; return declined(r) },
			http.StatusAccepted, StatusPendingPayment, 0},
		{"free-text decline code", func(r payRequest) (int, string) {
			return http.StatusPaymentRequired, fmt.Sprintf(`{"code":"payment_declined","decline_code":"Call your bank","order_id":%q}`, r.OrderID)
		}, http.StatusAccepted, StatusPendingPayment, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newAPI(t)
			a.pay.answer(tt.respond)
			w, out := a.do(t, http.MethodPost, "/v1/orders", "k1", "", checkoutBody(nil))
			if w.Code != tt.wantCode {
				t.Fatalf("status %d, want %d: %v", w.Code, tt.wantCode, out)
			}
			id, _ := out["order_id"].(string)
			if got := a.count(t, `SELECT count(*) FROM orders WHERE status = $1`, tt.wantStatus); got != 1 {
				t.Fatalf("%d orders %s, want 1", got, tt.wantStatus)
			}
			if got := a.count(t, `SELECT count(*) FROM outbox`); got != tt.wantEvents {
				t.Fatalf("%d outbox events, want %d", got, tt.wantEvents)
			}
			if token, _ := out["access_token"].(string); !a.tokens.Valid(id, token) {
				t.Fatalf("missing or invalid access token: %v", out)
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("checkout answer is cacheable: %v", w.Header())
			}
			if o, _ := out["order"].(map[string]any); out["currency"] != "EUR" && o["currency"] != "EUR" {
				t.Fatalf("answer without its currency: %v", out)
			}
			if tt.wantCode == http.StatusPaymentRequired && (out["code"] != "payment_declined" || out["order"] == nil) {
				t.Fatalf("decline problem %v, want code payment_declined with the order", out)
			}
			// payment-api is asked for the order's own total, once, under its ID.
			want := payRequest{Key: idempotency.Quote(id), OrderID: id, Token: "tok_test", Amount: 13240, Currency: "EUR"}
			if calls := a.pay.calls(); len(calls) != 1 || calls[0] != want {
				t.Fatalf("payment-api calls %+v, want %+v", calls, want)
			}
		})
	}
}

// The event a paid checkout writes is valid against its contract, and its
// amounts add up as the schema cannot express (AGENTS.md).
func TestPaidCheckoutWritesAValidOrderPlacedEvent(t *testing.T) {
	a := newAPI(t)
	// A book at the reduced rate makes shipping split across both rates.
	mixed := checkoutBody(func(m map[string]any) {
		m["lines"] = append(m["lines"].([]any), map[string]any{"sku": "BOOK-LABTECH", "quantity": 1})
		m["expected_total"] = 18140
	})
	w, out := a.do(t, http.MethodPost, "/v1/orders", "k1", "", mixed)
	if w.Code != http.StatusCreated {
		t.Fatalf("checkout %d %v", w.Code, out)
	}
	var payload []byte
	var aggregateType, aggregateID string
	if err := a.pool.QueryRow(context.Background(), `SELECT aggregatetype, aggregateid, payload FROM outbox`).Scan(&aggregateType, &aggregateID, &payload); err != nil {
		t.Fatal(err)
	}
	if aggregateType != "orders" || aggregateID != out["order_id"] {
		t.Fatalf("outbox row %s/%s, want orders/%v", aggregateType, aggregateID, out["order_id"])
	}
	if err := checkOrderPlaced(payload); err != nil {
		t.Fatalf("%v\n%s", err, payload)
	}
	if bytes.Contains(payload, []byte("Lovelace")) || bytes.Contains(payload, []byte("Musterstraße")) {
		t.Fatalf("the event carries personal data beyond email and first name: %s", payload)
	}
}

// checkOrderPlaced checks an order.placed against its schema, with format
// assertion, and the sums and rates the schema cannot express (AGENTS.md).
func checkOrderPlaced(payload []byte) error {
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	schema, err := c.Compile("../../../../contracts/events/labflow.order.placed.v1.schema.json")
	if err != nil {
		return err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(payload))
	if err != nil {
		return err
	}
	if err := schema.Validate(doc); err != nil {
		return fmt.Errorf("order.placed does not match its schema: %w", err)
	}
	var e struct {
		Subject string `json:"subject"`
		Data    struct {
			OrderID string `json:"order_id"`
			Priced
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &e); err != nil {
		return err
	}
	d := e.Data
	if e.Subject != d.OrderID {
		return fmt.Errorf("subject %s differs from data.order_id %s", e.Subject, d.OrderID)
	}
	goodsRates := map[int]bool{}
	var sum Amounts
	for _, l := range d.Lines {
		if l.Net+l.Tax != l.Gross || l.UnitPriceGross*int64(l.Quantity) != l.Gross {
			return fmt.Errorf("line amounts do not add up: %+v", l)
		}
		goodsRates[l.TaxRateBps] = true
		sum = Amounts{Net: sum.Net + l.Net, Tax: sum.Tax + l.Tax, Gross: sum.Gross + l.Gross}
	}
	var shipping Amounts
	for _, part := range d.Shipping.TaxBreakdown {
		if part.Net+part.Tax != part.Gross {
			return fmt.Errorf("shipping part amounts do not add up: %+v", part)
		}
		if !goodsRates[part.TaxRateBps] {
			return fmt.Errorf("shipping part at %d bps, a rate no goods have, or twice", part.TaxRateBps)
		}
		delete(goodsRates, part.TaxRateBps)
		shipping = Amounts{Net: shipping.Net + part.Net, Tax: shipping.Tax + part.Tax, Gross: shipping.Gross + part.Gross}
	}
	switch {
	case len(goodsRates) != 0:
		return fmt.Errorf("goods rates %v have no shipping part", goodsRates)
	case shipping != d.Shipping.Amounts:
		return fmt.Errorf("shipping %+v differs from the sum of its parts %+v", d.Shipping.Amounts, shipping)
	case (Amounts{Net: sum.Net + shipping.Net, Tax: sum.Tax + shipping.Tax, Gross: sum.Gross + shipping.Gross}) != d.Totals:
		return fmt.Errorf("totals %+v differ from the lines plus shipping", d.Totals)
	}
	return nil
}

// The producer check passes the contract's example and rejects a shipping
// rate the goods do not have, which the schema alone accepts.
func TestOrderPlacedCheckRejectsAWrongShippingRate(t *testing.T) {
	b, err := os.ReadFile("../../../../contracts/events/examples/labflow.order.placed.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := checkOrderPlaced(b); err != nil {
		t.Fatalf("the contract example fails the producer check: %v", err)
	}
	var e map[string]any
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	parts := e["data"].(map[string]any)["shipping"].(map[string]any)["tax_breakdown"].([]any)
	parts[1].(map[string]any)["tax_rate_bps"] = 1900
	wrong, _ := json.Marshal(e)
	if checkOrderPlaced(wrong) == nil {
		t.Fatal("two shipping parts at 19 % passed the producer check")
	}
}

// Every accepted email is one the event contract accepts (D66).
func TestEmailsMatchTheContract(t *testing.T) {
	tests := []struct {
		email string
		want  bool
	}{
		{"ada@example.com", true},
		{"ada.lovelace+orders@mail.example.co.uk", true},
		{strings.Repeat("a", 64) + "@example.com", true},
		{strings.Repeat("a", 65) + "@example.com", false},
		{"Ada <ada@example.com>", false},
		{"ada@exa_mple.com", false},
		{"ada@-example.com", false},
		{"ada@" + strings.Repeat("a", 64) + ".com", false},
		{"ädä@example.com", false},
		{"info@müller.de", false},
		{"ada\u0085@example.com", false},
		{"ada\u202e@example.com", false},
		{".ada@example.com", false},
		{"ada@localhost", false},
		{"@example.com", false},
	}
	o := Order{ID: "01a1f874-3be8-7fa4-95f5-868d7d69c709", Customer: Customer{FirstName: "Ada"}}
	o.Priced, _ = price([]Item{{"PIP-ADJ-100", 1}},
		map[string]Product{"PIP-ADJ-100": {SKU: "PIP-ADJ-100", Name: "Pipette", UnitPriceGross: 8900, TaxRateBps: 1900}},
		"DE", SpeedStandard, testRates(t))
	ctx, span := sdktrace.NewTracerProvider().Tracer("test").Start(context.Background(), "test")
	defer span.End()
	for _, tt := range tests {
		if got := validEmail(tt.email); got != tt.want {
			t.Errorf("validEmail(%q) = %v, want %v", tt.email, got, tt.want)
			continue
		}
		if tt.want {
			o.Customer.Email = tt.email
			_, payload, err := placedEvent(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			if err := checkOrderPlaced(payload); err != nil {
				t.Errorf("%q is accepted at checkout but breaks the event contract: %v", tt.email, err)
			}
		}
	}
}

func TestReplayReturnsTheSameOrderAndToken(t *testing.T) {
	a := newAPI(t)
	_, first := a.do(t, http.MethodPost, "/v1/orders", "k1", "", checkoutBody(nil))
	w, replay := a.do(t, http.MethodPost, "/v1/orders", "k1", "", checkoutBody(nil))
	if w.Code != http.StatusCreated || w.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay %d, headers %v", w.Code, w.Header())
	}
	if replay["order_id"] != first["order_id"] || replay["access_token"] != first["access_token"] {
		t.Fatalf("replay %v differs from the first answer %v", replay, first)
	}
	if n := len(a.pay.calls()); n != 1 {
		t.Fatalf("payment-api called %d times, want once", n)
	}
}

// tokenStored reports whether a stored idempotent response holds an access
// token: the field, or the value anywhere in the body.
func tokenStored(body []byte, token string) bool {
	var v map[string]any
	_ = json.Unmarshal(body, &v)
	_, field := v["access_token"]
	return field || bytes.Contains(body, []byte(token))
}

// The stored idempotent responses never hold the access token (D56), on any
// answer that carries one.
func TestNoAccessTokenIsStored(t *testing.T) {
	if !tokenStored([]byte(`{"order_id":"x","access_token":"t0k"}`), "t0k") {
		t.Fatal("the check misses a stored token") // the assertion itself can fail
	}
	for _, respond := range []func(payRequest) (int, string){paid, declined, pendingAtProvider} {
		a := newAPI(t)
		a.pay.answer(respond)
		w, out := a.do(t, http.MethodPost, "/v1/orders", "k1", "", checkoutBody(nil))
		token, _ := out["access_token"].(string)
		if token == "" {
			t.Fatalf("answer %d carries no token", w.Code)
		}
		var body []byte
		if err := a.pool.QueryRow(context.Background(), `SELECT response_body FROM idempotency_keys WHERE key = 'k1'`).Scan(&body); err != nil {
			t.Fatal(err)
		}
		if tokenStored(body, token) {
			t.Fatalf("the stored %d response holds the access token: %s", w.Code, body)
		}
	}
}

// A charge that succeeds while recording it fails answers 500, leaves the
// order pending without an event, and the client's retry with the same key
// resumes it: one order, one charge key, one event.
func TestRetryAfterFailedCommitResumesTheOrder(t *testing.T) {
	a := newAPI(t)
	a.exec(t, `CREATE FUNCTION fail_insert() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected'; END $$;
		CREATE TRIGGER fail_outbox BEFORE INSERT ON outbox FOR EACH ROW EXECUTE FUNCTION fail_insert()`)
	if w, _ := a.do(t, http.MethodPost, "/v1/orders", "k1", "", checkoutBody(nil)); w.Code != http.StatusInternalServerError {
		t.Fatalf("failed commit answered %d, want 500", w.Code)
	}
	if a.count(t, `SELECT count(*) FROM orders WHERE status = 'pending_payment'`) != 1 || a.count(t, `SELECT count(*) FROM outbox`) != 0 {
		t.Fatal("after the failed commit the order is not pending, or an event was written")
	}
	a.exec(t, `DROP TRIGGER fail_outbox ON outbox`)
	w, out := a.do(t, http.MethodPost, "/v1/orders", "k1", "", checkoutBody(nil))
	if w.Code != http.StatusCreated {
		t.Fatalf("retry %d %v, want 201", w.Code, out)
	}
	calls := a.pay.calls()
	if len(calls) != 2 || calls[0] != calls[1] || calls[0].OrderID != out["order_id"] {
		t.Fatalf("charge calls %+v, want the same order and key twice", calls)
	}
	if a.count(t, `SELECT count(*) FROM orders`) != 1 || a.count(t, `SELECT count(*) FROM outbox`) != 1 {
		t.Fatal("want one order and one event")
	}
	// The event written on the retry, from the reloaded order, is valid too.
	var payload []byte
	if err := a.pool.QueryRow(context.Background(), `SELECT payload FROM outbox`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if err := checkOrderPlaced(payload); err != nil {
		t.Fatalf("%v\n%s", err, payload)
	}
}

// A retry whose idempotency record is gone, as after the 24-hour retention,
// resumes the same order and charge key; a paid order is never charged again.
func TestRetryAfterLostRecordResumesTheOrder(t *testing.T) {
	a := newAPI(t)
	a.pay.answer(fixed(http.StatusServiceUnavailable, `{}`))
	_, first := a.do(t, http.MethodPost, "/v1/orders", "k1", "", checkoutBody(nil))
	a.exec(t, `DELETE FROM idempotency_keys`)
	a.pay.answer(paid)
	w, retry := a.do(t, http.MethodPost, "/v1/orders", "k1", "", checkoutBody(nil))
	if w.Code != http.StatusCreated || retry["order_id"] != first["order_id"] {
		t.Fatalf("retry %d %v, want order %v paid", w.Code, retry["order_id"], first["order_id"])
	}
	a.exec(t, `DELETE FROM idempotency_keys`)
	a.do(t, http.MethodPost, "/v1/orders", "k1", "", checkoutBody(nil))
	if calls := a.pay.calls(); len(calls) != 2 || calls[0] != calls[1] {
		t.Fatalf("charge calls %+v, want two identical ones, none after paid", calls)
	}
}

// "The same request" means the same bytes, before and after the shared
// middleware releases its record, so the answer never depends on which record
// survived.
func TestKeyReusedForAnotherRequestIsRejected(t *testing.T) {
	for _, other := range []string{
		checkoutBody(func(m map[string]any) { m["shipping_speed"] = "express" }),
		checkoutBody(nil) + "\n",
	} {
		a := newAPI(t)
		a.do(t, http.MethodPost, "/v1/orders", "k1", "", checkoutBody(nil))
		if w, _ := a.do(t, http.MethodPost, "/v1/orders", "k1", "", other); w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("same key, other body, record kept: %d, want 422", w.Code)
		}
		a.exec(t, `DELETE FROM idempotency_keys`)
		if w, _ := a.do(t, http.MethodPost, "/v1/orders", "k1", "", other); w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("same key, other body, record gone: %d, want 422", w.Code)
		}
	}
}

func TestCheckoutValidation(t *testing.T) {
	a := newAPI(t)
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"email with a display name", func(m map[string]any) { m["customer"].(map[string]any)["email"] = "Ada <ada@example.com>" }},
		{"email beyond the contract", func(m map[string]any) {
			m["customer"].(map[string]any)["email"] = strings.Repeat("a", 65) + "@example.com"
		}},
		{"control character in a name", func(m map[string]any) { m["customer"].(map[string]any)["first_name"] = "Ada\u0007" }},
		{"country outside the EU", func(m map[string]any) { m["shipping_address"].(map[string]any)["country"] = "CH" }},
		{"unknown speed", func(m map[string]any) { m["shipping_speed"] = "overnight" }},
		{"no lines", func(m map[string]any) { m["lines"] = []any{} }},
		{"too many units", func(m map[string]any) { m["lines"] = []any{map[string]any{"sku": "PIP-ADJ-100", "quantity": 100}} }},
		{"repeated sku", func(m map[string]any) {
			m["lines"] = []any{map[string]any{"sku": "PIP-ADJ-100", "quantity": 1}, map[string]any{"sku": "PIP-ADJ-100", "quantity": 2}}
		}},
		{"unknown sku", func(m map[string]any) { m["lines"] = []any{map[string]any{"sku": "NOPE-1", "quantity": 1}} }},
		{"client-supplied price", func(m map[string]any) {
			m["lines"] = []any{map[string]any{"sku": "PIP-ADJ-100", "quantity": 1, "unit_price_gross": 1}}
		}},
		{"too many lines", func(m map[string]any) {
			lines := make([]any, 51)
			for i := range lines {
				lines[i] = map[string]any{"sku": "PIP-ADJ-100", "quantity": 1}
			}
			m["lines"] = lines
		}},
		{"no expected total", func(m map[string]any) { delete(m, "expected_total") }},
		{"payment token over 255 bytes", func(m map[string]any) { m["payment_token"] = strings.Repeat("é", 128) }},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if w, out := a.do(t, http.MethodPost, "/v1/orders", fmt.Sprintf("v%d", i), "", checkoutBody(tt.mutate)); w.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %v", w.Code, out)
			}
		})
	}
	if w, _ := a.do(t, http.MethodPost, "/v1/orders", "", "", checkoutBody(nil)); w.Code != http.StatusBadRequest {
		t.Fatalf("missing Idempotency-Key: %d, want 400", w.Code)
	}
	bare := httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(checkoutBody(nil)))
	bare.Header.Set(idempotency.Header, "k-bare")
	w := httptest.NewRecorder()
	a.h.ServeHTTP(w, bare)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unquoted Idempotency-Key: %d, want 400", w.Code)
	}
	if n := len(a.pay.calls()) + a.count(t, `SELECT count(*) FROM orders`); n != 0 {
		t.Fatalf("invalid checkouts reached payment-api or stored orders (%d)", n)
	}
}

func TestGetOrderNeedsItsAccessToken(t *testing.T) {
	a := newAPI(t)
	_, created := a.do(t, http.MethodPost, "/v1/orders", "k1", "", checkoutBody(nil))
	id, token := created["order_id"].(string), created["access_token"].(string)
	other := "01a1f874-3be8-7fa4-95f5-868d7d69c709"
	tests := []struct {
		name, id, auth string
		want           int
	}{
		{"own token", id, "Bearer " + token, http.StatusOK},
		{"lower-case scheme", id, "bearer " + token, http.StatusOK},
		{"two spaces", id, "Bearer  " + token, http.StatusOK},
		{"no token", id, "", http.StatusUnauthorized},
		{"another scheme", id, "Basic " + token, http.StatusUnauthorized},
		{"another order's token", id, "Bearer " + a.tokens.Issue(other), http.StatusNotFound},
		{"unknown order", other, "Bearer " + a.tokens.Issue(other), http.StatusNotFound},
		{"not a UUIDv7", "1", "Bearer " + token, http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, out := a.do(t, http.MethodGet, "/v1/orders/"+tt.id, "", tt.auth, "")
			if w.Code != tt.want || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status %d, Cache-Control %q, want %d and no-store: %v", w.Code, w.Header().Get("Cache-Control"), tt.want, out)
			}
			if tt.want == http.StatusOK && (out["status"] != StatusPaid || out["shipping_address"].(map[string]any)["city"] != "Berlin") {
				t.Fatalf("order %v", out)
			}
		})
	}
}

func TestProductsAndImport(t *testing.T) {
	a := newAPI(t)
	w, out := a.do(t, http.MethodGet, "/v1/products", "", "", "")
	if w.Code != http.StatusOK || len(out["products"].([]any)) != 17 {
		t.Fatalf("products %d %v", w.Code, out)
	}
	res, err := Import(t.Context(), NewStore(a.pool), "../../catalogue.json")
	if err != nil || res != (ImportResult{Unchanged: 17}) {
		t.Fatalf("re-import %+v %v, want every product unchanged", res, err)
	}
	a.exec(t, `UPDATE products SET unit_price_gross = 1 WHERE sku = 'PIP-ADJ-100'`)
	if res, err = Import(t.Context(), NewStore(a.pool), "../../catalogue.json"); err != nil || res != (ImportResult{Changed: 1, Unchanged: 16}) {
		t.Fatalf("import after a drift %+v %v, want one change", res, err)
	}
}

func TestImportRetiresProductsMissingFromTheFile(t *testing.T) {
	a := newAPI(t)
	path := filepath.Join(t.TempDir(), "catalogue.json")
	small := `[{"sku":"BKR-GLS-250","name":"Glass beaker, 250 mL","unit_price_gross":590,"tax_rate_bps":1900,"weight_grams":150}]`
	if err := os.WriteFile(path, []byte(small), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Import(t.Context(), NewStore(a.pool), path)
	if err != nil || res != (ImportResult{Unchanged: 1, Retired: 16}) {
		t.Fatalf("import %+v %v, want 16 retired", res, err)
	}
	if _, out := a.do(t, http.MethodGet, "/v1/products", "", "", ""); len(out["products"].([]any)) != 1 {
		t.Fatalf("retired products are still listed: %v", out)
	}
	if w, out := a.do(t, http.MethodPost, "/v1/orders", "k1", "", checkoutBody(nil)); w.Code != http.StatusBadRequest {
		t.Fatalf("a retired product was ordered: %d %v", w.Code, out)
	}
	// Putting it back in the file puts it back on sale.
	if res, err := Import(t.Context(), NewStore(a.pool), "../../catalogue.json"); err != nil || res.Changed != 16 {
		t.Fatalf("re-import %+v %v, want 16 back on sale", res, err)
	}
}

// A file with data after its array is refused before anything is retired.
func TestImportRefusesTrailingData(t *testing.T) {
	a := newAPI(t)
	one := `[{"sku":"BKR-GLS-250","name":"Glass beaker, 250 mL","unit_price_gross":590,"tax_rate_bps":1900,"weight_grams":150}]`
	for _, trailing := range []string{one + "\n[]", one + " garbage"} {
		path := filepath.Join(t.TempDir(), "catalogue.json")
		if err := os.WriteFile(path, []byte(trailing), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Import(t.Context(), NewStore(a.pool), path); err == nil {
			t.Fatalf("a catalogue with trailing data was imported: %q", trailing)
		}
	}
	if n := a.count(t, `SELECT count(*) FROM products WHERE retired_at IS NULL`); n != 17 {
		t.Fatalf("%d products on sale after refused imports, want 17", n)
	}
}

// The customer pays only the total they were shown (D66).
func TestChangedTotalIsNotCharged(t *testing.T) {
	a := newAPI(t)
	w, out := a.do(t, http.MethodPost, "/v1/orders", "k1", "", checkoutBody(func(m map[string]any) { m["expected_total"] = 13000 }))
	if w.Code != http.StatusConflict || out["code"] != "price_changed" {
		t.Fatalf("status %d %v, want 409 price_changed", w.Code, out)
	}
	if total := out["quote"].(map[string]any)["totals"].(map[string]any)["gross_amount"]; total != float64(13240) {
		t.Fatalf("quote total %v, want 13240", total)
	}
	if n := a.count(t, `SELECT count(*) FROM orders`); n != 0 || len(a.pay.calls()) != 0 {
		t.Fatalf("%d orders and %d charges after a price change, want none", n, len(a.pay.calls()))
	}
}

func TestQuote(t *testing.T) {
	a := newAPI(t)
	body := `{"shipping_country":"FR","shipping_speed":"express","lines":[{"sku":"PIP-ADJ-100","quantity":2},{"sku":"BOOK-SAFETY","quantity":1}]}`
	w, out := a.do(t, http.MethodPost, "/v1/quotes", "", "", body)
	if w.Code != http.StatusOK {
		t.Fatalf("quote %d %v", w.Code, out)
	}
	// 2 × 89.00 + 39.00 + 19.90 express to EU-1.
	totals := out["totals"].(map[string]any)
	shipping := out["shipping"].(map[string]any)
	if totals["gross_amount"] != float64(23690) || shipping["zone"] != "EU-1" || len(shipping["tax_breakdown"].([]any)) != 2 {
		t.Fatalf("quote %v", out)
	}
	if n := a.count(t, `SELECT count(*) FROM orders`); n != 0 {
		t.Fatalf("a quote stored %d orders", n)
	}
	if w, _ := a.do(t, http.MethodPost, "/v1/quotes", "", "", `{"shipping_country":"CH","shipping_speed":"standard","lines":[{"sku":"PIP-ADJ-100","quantity":1}]}`); w.Code != http.StatusBadRequest {
		t.Fatalf("a quote outside the EU: %d, want 400", w.Code)
	}
}
