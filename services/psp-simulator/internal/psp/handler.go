// Package psp serves the simulated payment provider's API, shaped like a
// card processor's: the browser exchanges card details for a single-use token
// with the publishable key, and the merchant charges the token with the
// secret key and an Idempotency-Key.
package psp

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/thiagogenez/labflow/libs/go/api"
	"github.com/thiagogenez/labflow/libs/go/idempotency"
	"github.com/thiagogenez/labflow/services/psp-simulator/internal/card"
)

const (
	// maxAmount is the largest integer every JSON parser reads exactly (RFC 7493).
	maxAmount = 1<<53 - 1
	// ChargeTimeout bounds a charge's work once the caller has stopped
	// waiting; with the 5 s to store the outcome it fits the 20 s shutdown drain.
	ChargeTimeout = 15 * time.Second
)

var (
	charges = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "psp_charges_total",
		Help: "Charges created, by status at creation.",
	}, []string{"status"})
	currencyCode = regexp.MustCompile(`^[A-Z]{3}$`)
)

// Config holds the API keys and the simulated delays.
type Config struct {
	PublishableKey string
	SecretKey      string
	SlowDelay      time.Duration // processing time of the "slow" test cards
	PendingDelay   time.Duration // time until a pending charge settles
}

type handler struct {
	store *Store
	log   *slog.Logger
	cfg   Config
}

// Register adds the provider routes to mux.
func Register(mux *http.ServeMux, store *Store, idem *idempotency.Store, log *slog.Logger, cfg Config) {
	h := &handler{store: store, log: log, cfg: cfg}
	mux.Handle("POST /v1/tokens", bearer(cfg.PublishableKey, http.HandlerFunc(h.createToken)))
	mux.Handle("POST /v1/charges", bearer(cfg.SecretKey, idem.Middleware(http.HandlerFunc(h.createCharge))))
	mux.Handle("GET /v1/charges/{id}", bearer(cfg.SecretKey, http.HandlerFunc(h.getCharge)))
}

type tokenRequest struct {
	Number   string `json:"number"`
	ExpMonth int    `json:"exp_month"`
	ExpYear  int    `json:"exp_year"`
	CVC      string `json:"cvc"`
}

func (h *handler) createToken(w http.ResponseWriter, r *http.Request) {
	var req tokenRequest
	if err := api.DecodeJSON(w, r, &req); err != nil {
		api.Error(w, r, http.StatusBadRequest, err.Error())
		return
	}
	c, err := card.Parse(req.Number, req.ExpMonth, req.ExpYear, req.CVC, time.Now())
	var ce *card.Error
	if errors.As(err, &ce) {
		badRequest(w, r, ce.Code, ce.Message)
		return
	}
	t, err := h.store.CreateToken(r.Context(), c)
	if err != nil {
		h.fail(w, r, "create token", err)
		return
	}
	api.WriteJSON(w, http.StatusCreated, t)
}

type chargeRequest struct {
	Amount   int64             `json:"amount"`
	Currency string            `json:"currency"`
	Token    string            `json:"token"`
	Metadata map[string]string `json:"metadata"`
}

func (req chargeRequest) validate() (code, detail string) {
	switch {
	case req.Amount <= 0 || req.Amount > maxAmount:
		return "invalid_amount", "amount must be a positive integer in minor units"
	case !currencyCode.MatchString(req.Currency):
		return "invalid_currency", "currency must be an upper-case ISO 4217 code"
	case req.Token == "" || len(req.Token) > 255 || !api.Printable(req.Token):
		return "invalid_token", "token is required"
	case len(req.Metadata) > 20:
		return "invalid_metadata", "metadata holds at most 20 keys"
	}
	for k, v := range req.Metadata {
		if len(k) > 40 || len(v) > 500 || !api.Printable(k) || !api.Printable(v) {
			return "invalid_metadata", "metadata keys are at most 40 characters and values at most 500, without control characters"
		}
	}
	return "", ""
}

func (h *handler) createCharge(w http.ResponseWriter, r *http.Request) {
	var req chargeRequest
	if err := api.DecodeJSON(w, r, &req); err != nil {
		api.Error(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if code, detail := req.validate(); code != "" {
		badRequest(w, r, code, detail)
		return
	}
	// A real provider finishes a charge even when the caller has stopped
	// waiting, so the work below outlives the client connection, within limits.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), ChargeTimeout)
	defer cancel()
	outcome, err := h.store.Outcome(ctx, req.Token)
	if errors.Is(err, errUnknownToken) {
		badRequest(w, r, "invalid_token", "no such token")
		return
	}
	if err != nil {
		h.fail(w, r, "read token", err)
		return
	}
	if outcome == card.Slow {
		select {
		case <-time.After(h.cfg.SlowDelay):
		case <-ctx.Done():
			h.fail(w, r, "slow charge", ctx.Err())
			return
		}
	}
	c, created, err := h.store.CreateCharge(ctx, ChargeRequest{
		Token: req.Token, IdempotencyKey: idempotency.KeyFrom(r.Context()),
		Amount: req.Amount, Currency: req.Currency, Metadata: req.Metadata,
	}, outcome, h.cfg.PendingDelay)
	switch {
	case errors.Is(err, errTokenUsed):
		badRequest(w, r, "token_already_used", "a token pays for one charge only")
		return
	case errors.Is(err, errKeyReused):
		api.Error(w, r, http.StatusUnprocessableEntity, "this Idempotency-Key was used with a different request")
		return
	case err != nil:
		h.fail(w, r, "create charge", err)
		return
	}
	if created {
		charges.WithLabelValues(c.Status).Inc()
	}
	if c.Status == StatusFailed {
		api.WriteProblem(w, r, http.StatusPaymentRequired, "the card was declined",
			map[string]any{"code": "card_declined", "decline_code": c.FailureCode, "charge": c.ID})
		return
	}
	api.WriteJSON(w, http.StatusCreated, c)
}

func (h *handler) getCharge(w http.ResponseWriter, r *http.Request) {
	c, err := h.store.Charge(r.Context(), r.PathValue("id"))
	if errors.Is(err, errNotFound) {
		api.Error(w, r, http.StatusNotFound, "no such charge")
		return
	}
	if err != nil {
		h.fail(w, r, "read charge", err)
		return
	}
	api.WriteJSON(w, http.StatusOK, c)
}

func (h *handler) fail(w http.ResponseWriter, r *http.Request, op string, err error) {
	h.log.ErrorContext(r.Context(), op+" failed", "error", err)
	api.Error(w, r, http.StatusInternalServerError, "internal error")
}

func badRequest(w http.ResponseWriter, r *http.Request, code, detail string) {
	api.WriteProblem(w, r, http.StatusBadRequest, detail, map[string]any{"code": code})
}

// bearer admits requests that carry the API key, compared in constant time.
func bearer(key string, next http.Handler) http.Handler {
	want := []byte("Bearer " + key)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			api.Error(w, r, http.StatusUnauthorized, "a valid API key is required")
			return
		}
		next.ServeHTTP(w, r)
	})
}
