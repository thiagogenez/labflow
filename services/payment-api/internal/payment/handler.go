package payment

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/thiagogenez/labflow/libs/go/api"
	"github.com/thiagogenez/labflow/libs/go/idempotency"
)

const (
	// maxAmount is the largest integer every JSON parser reads exactly (RFC 7493).
	maxAmount = 1<<53 - 1
	// payTimeout bounds a payment's work once order-api has stopped waiting;
	// with the 5 s to store the outcome it fits the 20 s shutdown drain.
	payTimeout = 10 * time.Second
)

// Register adds the payment routes to mux. They are internal: order-api is
// their only caller.
func Register(mux *http.ServeMux, svc *Service, idem *idempotency.Store, log *slog.Logger) {
	h := &handler{svc: svc, log: log}
	mux.Handle("POST /v1/payments", idem.Middleware(http.HandlerFunc(h.create)))
	mux.HandleFunc("GET /v1/payments/{order_id}", h.get)
}

type handler struct {
	svc *Service
	log *slog.Logger
}

type createRequest struct {
	OrderID  string `json:"order_id"`
	Token    string `json:"token"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

// view is a payment as the API shows it; the token stays private.
type view struct {
	OrderID          string `json:"order_id"`
	Status           string `json:"status"`
	Amount           int64  `json:"amount"`
	Currency         string `json:"currency"`
	ProviderChargeID string `json:"provider_charge_id,omitempty"`
	DeclineCode      string `json:"decline_code,omitempty"`
}

func toView(p Payment) view {
	return view{OrderID: p.OrderID, Status: p.Status, Amount: p.Amount, Currency: p.Currency,
		ProviderChargeID: p.ChargeID, DeclineCode: p.DeclineCode}
}

// orderUUID accepts order IDs as order-api issues them (D57): UUIDv7 in the
// lower-case hyphenated form PostgreSQL returns.
func orderUUID(s string) bool {
	u, err := uuid.Parse(s)
	return err == nil && u.String() == s && u.Version() == 7 && u.Variant() == uuid.RFC4122
}

func (req createRequest) validate() string {
	switch {
	case !orderUUID(req.OrderID):
		return "order_id must be a lower-case hyphenated UUIDv7"
	case req.Token == "" || len(req.Token) > 255 || !api.Printable(req.Token):
		return "token is required: at most 255 characters, without control characters"
	case req.Amount <= 0 || req.Amount > maxAmount:
		return "amount must be a positive integer in minor units"
	case req.Currency != "EUR":
		return "currency must be EUR"
	}
	return ""
}

// create answers 201 when paid, 202 while the outcome is pending and 402 when declined.
func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := api.DecodeJSON(w, r, &req); err != nil {
		api.Error(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if msg := req.validate(); msg != "" {
		api.Error(w, r, http.StatusBadRequest, msg)
		return
	}
	// The outcome must be recorded even if order-api stops waiting, within limits.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), payTimeout)
	defer cancel()
	p, err := h.svc.Pay(ctx, Payment{OrderID: req.OrderID, Token: req.Token, Amount: req.Amount, Currency: req.Currency})
	switch {
	case errors.Is(err, errConflict):
		api.Error(w, r, http.StatusConflict, "this order already has a payment with different details")
		return
	case err != nil:
		h.log.ErrorContext(r.Context(), "payment failed", "order_id", req.OrderID, "error", err)
		api.Error(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	switch p.Status {
	case StatusSucceeded:
		api.WriteJSON(w, http.StatusCreated, toView(p))
	case StatusFailed:
		api.WriteProblem(w, r, http.StatusPaymentRequired, "the payment was declined",
			map[string]any{"code": "payment_declined", "decline_code": p.DeclineCode, "order_id": p.OrderID})
	default:
		api.WriteJSON(w, http.StatusAccepted, toView(p))
	}
}

func (h *handler) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("order_id")
	if !orderUUID(id) {
		api.Error(w, r, http.StatusBadRequest, "order_id must be a lower-case hyphenated UUIDv7")
		return
	}
	p, err := h.svc.Get(r.Context(), id)
	if errors.Is(err, errNotFound) {
		api.Error(w, r, http.StatusNotFound, "no payment for this order")
		return
	}
	if err != nil {
		h.log.ErrorContext(r.Context(), "read payment failed", "order_id", id, "error", err)
		api.Error(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	api.WriteJSON(w, http.StatusOK, toView(p))
}
