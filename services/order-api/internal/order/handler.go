package order

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/thiagogenez/labflow/libs/go/api"
	"github.com/thiagogenez/labflow/libs/go/idempotency"
)

const (
	maxLines    = 50 // D60
	maxQuantity = 99 // D60
	// checkoutTimeout bounds a checkout once the client has stopped waiting;
	// with the 5 s to store the idempotent response and a margin for reading
	// the request, it fits the 20 s shutdown drain.
	checkoutTimeout = 12 * time.Second
)

// Register adds the public order routes to mux.
func Register(mux *http.ServeMux, svc *Service, tokens *Tokens, idem *idempotency.Store, log *slog.Logger) {
	h := &handler{svc: svc, tokens: tokens, log: log}
	// The access token is added outside the idempotency middleware, so the
	// stored response never holds it and a replay gets it re-derived (D56).
	mux.Handle("POST /v1/orders", h.withAccessToken(requireQuotedKey(idem.Middleware(http.HandlerFunc(h.create)))))
	mux.HandleFunc("GET /v1/orders/{id}", h.get)
	mux.HandleFunc("GET /v1/products", h.products)
	mux.HandleFunc("POST /v1/quotes", h.quote)
}

type handler struct {
	svc    *Service
	tokens *Tokens
	log    *slog.Logger
}

// view is an order as the API shows it: never the payment token.
type view struct {
	OrderID         string   `json:"order_id"`
	Status          string   `json:"status"`
	Customer        Customer `json:"customer"`
	ShippingAddress Address  `json:"shipping_address"`
	Priced
	DeclineCode string `json:"decline_code,omitempty"`
	CreatedAt   string `json:"created_at"`
}

func toView(o Order) view {
	return view{OrderID: o.ID, Status: o.Status, Customer: o.Customer, ShippingAddress: o.Address, Priced: o.Priced,
		DeclineCode: o.DeclineCode, CreatedAt: o.CreatedAt.UTC().Format(time.RFC3339)}
}

// create answers 201 when paid, 202 while the payment is pending and 402 when declined.
func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	var req CheckoutRequest
	if err := api.DecodeJSON(w, r, &req); err != nil {
		api.Error(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if msg := req.validate(); msg != "" {
		api.Error(w, r, http.StatusBadRequest, msg)
		return
	}
	// A charge must be recorded even if the client stops waiting, within limits.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), checkoutTimeout)
	defer cancel()
	o, err := h.svc.Checkout(ctx, idempotency.KeyFrom(r.Context()), idempotency.FingerprintFrom(r.Context()), req)
	var changed *errPriceChanged
	switch {
	case errors.As(err, &changed):
		api.WriteProblem(w, r, http.StatusConflict, "the total differs from the one shown; confirm the new quote",
			map[string]any{"code": "price_changed", "quote": changed.quote})
		return
	case errors.Is(err, errUnknownSKU), errors.Is(err, errTooLarge):
		api.Error(w, r, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, errKeyReused):
		api.Error(w, r, http.StatusUnprocessableEntity, "this Idempotency-Key was used for a different order")
		return
	case err != nil:
		h.log.ErrorContext(r.Context(), "checkout failed", "error", err)
		api.Error(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	switch o.Status {
	case StatusPaid:
		api.WriteJSON(w, http.StatusCreated, toView(o))
	case StatusPaymentFailed:
		api.WriteProblem(w, r, http.StatusPaymentRequired, "the payment was declined",
			map[string]any{"code": "payment_declined", "decline_code": o.DeclineCode, "order_id": o.ID, "order": toView(o)})
	default:
		api.WriteJSON(w, http.StatusAccepted, toView(o))
	}
}

// requireQuotedKey holds checkout to the quoted Structured Field String form
// of Idempotency-Key that D66 and the IETF draft specify.
func requireQuotedKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get(idempotency.Header); v != "" && !strings.HasPrefix(v, `"`) {
			api.Error(w, r, http.StatusBadRequest, `send the Idempotency-Key as a quoted string, such as "f81d4fae"`)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withAccessToken adds the order's access token to a 201, 202 or 402 answer,
// whose body names the order, and keeps every checkout answer out of caches.
func (h *handler) withAccessToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &bufferedResponse{header: w.Header(), status: http.StatusOK}
		next.ServeHTTP(rec, r)
		body := rec.body.Bytes()
		if rec.status == http.StatusCreated || rec.status == http.StatusAccepted || rec.status == http.StatusPaymentRequired {
			// RawMessage keeps every other value byte for byte.
			var v map[string]json.RawMessage
			var id string
			if json.Unmarshal(body, &v) == nil && json.Unmarshal(v["order_id"], &id) == nil && id != "" {
				v["access_token"], _ = json.Marshal(h.tokens.Issue(id))
				if b, err := json.Marshal(v); err == nil {
					body = append(b, '\n')
				}
			}
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(rec.status)
		_, _ = w.Write(body)
	})
}

// bufferedResponse holds a response so a wrapper can rewrite its body.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header         { return b.header }
func (b *bufferedResponse) WriteHeader(code int)        { b.status = code }
func (b *bufferedResponse) Write(p []byte) (int, error) { return b.body.Write(p) }

// get returns the order to the holder of its access token, sent as a bearer
// token; an unknown order and a wrong token both answer 404, so order IDs
// cannot be probed.
func (h *handler) get(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store") // personal data
	id := r.PathValue("id")
	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		w.Header().Set("WWW-Authenticate", "Bearer")
		api.Error(w, r, http.StatusUnauthorized, "the order's access token is required")
		return
	}
	if !canonicalUUIDv7(id) || !h.tokens.Valid(id, token) {
		api.Error(w, r, http.StatusNotFound, "no such order")
		return
	}
	o, err := h.svc.Get(r.Context(), id)
	if errors.Is(err, errNotFound) {
		api.Error(w, r, http.StatusNotFound, "no such order")
		return
	}
	if err != nil {
		h.log.ErrorContext(r.Context(), "read order failed", "order_id", id, "error", err)
		api.Error(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	api.WriteJSON(w, http.StatusOK, toView(o))
}

// bearerToken reads an Authorization header: the scheme is case-insensitive
// (RFC 9110, section 11.1) and may be followed by several spaces (RFC 6750).
func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(header, " ")
	token = strings.TrimLeft(token, " ")
	return token, ok && strings.EqualFold(scheme, "Bearer") && token != ""
}

// quote prices lines for a destination without storing anything, so the
// customer sees the total before ordering (D66).
func (h *handler) quote(w http.ResponseWriter, r *http.Request) {
	var req QuoteRequest
	if err := api.DecodeJSON(w, r, &req); err != nil {
		api.Error(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if msg := validateDelivery(req.Country, req.Speed, req.Items); msg != "" {
		api.Error(w, r, http.StatusBadRequest, msg)
		return
	}
	priced, err := h.svc.Quote(r.Context(), req)
	switch {
	case errors.Is(err, errUnknownSKU), errors.Is(err, errTooLarge):
		api.Error(w, r, http.StatusBadRequest, err.Error())
	case err != nil:
		h.log.ErrorContext(r.Context(), "quote failed", "error", err)
		api.Error(w, r, http.StatusInternalServerError, "internal error")
	default:
		api.WriteJSON(w, http.StatusOK, priced)
	}
}

func (h *handler) products(w http.ResponseWriter, r *http.Request) {
	products, err := h.svc.Products(r.Context())
	if err != nil {
		h.log.ErrorContext(r.Context(), "read catalogue failed", "error", err)
		api.Error(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"currency": currency, "products": products})
}

// canonicalUUIDv7 keeps anything but an ID order-api issues out of SQL.
func canonicalUUIDv7(s string) bool {
	u, err := uuid.Parse(s)
	return err == nil && u.String() == s && u.Version() == 7 && u.Variant() == uuid.RFC4122
}

// text checks a customer-supplied string: length in characters, no control
// characters (AGENTS.md).
func text(s string, minLen, maxLen int) bool {
	n := utf8.RuneCountInString(s)
	return n >= minLen && n <= maxLen && api.Printable(s)
}

// emailLocalChars are the characters an unquoted local part may hold.
const emailLocalChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.!#$%&'*+-/=?^_`{|}~"

// validEmail accepts the subset of the event contract's email format a shop
// needs: an unquoted ASCII local part of at most 64 characters and a domain
// of hostname labels, at most 254 characters in all. So every accepted
// address yields a valid order.placed and a deliverable SMTP envelope.
func validEmail(s string) bool {
	at := strings.LastIndexByte(s, '@')
	if len(s) > 254 || at < 1 {
		return false
	}
	local, domain := s[:at], s[at+1:]
	if len(local) > 64 || strings.HasPrefix(local, ".") || strings.HasSuffix(local, ".") || strings.Contains(local, "..") {
		return false
	}
	for _, c := range local {
		if !strings.ContainsRune(emailLocalChars, c) {
			return false
		}
	}
	labels := strings.Split(domain, ".")
	if len(domain) > 253 || len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if len(l) < 1 || len(l) > 63 || strings.HasPrefix(l, "-") || strings.HasSuffix(l, "-") {
			return false
		}
		for _, c := range l {
			if !hostnameChar(c) {
				return false
			}
		}
	}
	return true
}

// hostnameChar reports whether c may appear in a hostname label.
func hostnameChar(c rune) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-'
}

func (req CheckoutRequest) validate() string {
	c, a := req.Customer, req.Address
	switch {
	case !validEmail(c.Email):
		return "customer.email must be a plain email address such as ada@example.com"
	case !text(c.FirstName, 1, 100) || !text(c.LastName, 1, 100):
		return "customer.first_name and customer.last_name take 1 to 100 characters, without control characters"
	case !text(a.Line1, 1, 100) || !text(a.Line2, 0, 100) || !text(a.City, 1, 100) || !text(a.PostalCode, 1, 16):
		return "shipping_address needs line1, city (1 to 100 characters) and postal_code (1 to 16), without control characters"
	case req.PaymentToken == "" || len(req.PaymentToken) > 255 || !api.Printable(req.PaymentToken):
		return "payment_token is required: at most 255 bytes, without control characters"
	case req.ExpectedTotal <= 0 || req.ExpectedTotal > maxAmount:
		return "expected_total must be the gross total the customer was shown, in minor units"
	}
	return validateDelivery(a.Country, req.Speed, req.Items)
}

// validateDelivery checks what both a quote and an order need.
func validateDelivery(country, speed string, items []Item) string {
	switch {
	case zones[country] == "":
		return "the shipping country must be an EU country code such as DE"
	case speed != SpeedStandard && speed != SpeedExpress:
		return "shipping_speed must be standard or express"
	case len(items) == 0 || len(items) > maxLines:
		return "lines must hold 1 to 50 products"
	}
	seen := make(map[string]bool, len(items))
	for _, it := range items {
		switch {
		case !validSKU(it.SKU):
			return "every line needs a valid sku"
		case it.Quantity < 1 || it.Quantity > maxQuantity:
			return "every quantity must be 1 to 99"
		case seen[it.SKU]:
			return "each sku may appear in one line only"
		}
		seen[it.SKU] = true
	}
	return ""
}
