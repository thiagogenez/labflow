package order

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/thiagogenez/labflow/libs/go/idempotency"
)

const (
	// maxResponseBytes bounds what is read from payment-api.
	maxResponseBytes = 64 << 10
	// paymentTimeout bounds one call to payment-api, body included, well
	// inside checkoutTimeout.
	paymentTimeout = 5 * time.Second
)

// Payment outcomes, also the order_checkouts_total labels.
const (
	outcomePaid     = "paid"
	outcomeDeclined = "declined"
	outcomePending  = "pending"
	outcomeUnknown  = "unknown"
)

var (
	paymentCalls = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "order_payment_api_duration_seconds",
		Help:    "Duration of charge calls to payment-api, by outcome.",
		Buckets: prometheus.DefBuckets,
	}, []string{"outcome"})
	// declineCode bounds what the provider's decline reason may be before it
	// is stored, logged and shown to the customer.
	declineCode = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
)

// paymentResult is payment-api's answer about an order's charge.
type paymentResult struct {
	Outcome     string
	DeclineCode string
}

// errUnknownOutcome means payment-api's answer does not settle the payment:
// the order stays pending for the reconciler (D51).
var errUnknownOutcome = errors.New("payment outcome unknown")

// Payments calls payment-api.
type Payments struct {
	base   *url.URL
	client *http.Client
}

// NewPayments returns a client whose every call, body included, is bounded by paymentTimeout.
func NewPayments(base *url.URL) *Payments {
	return &Payments{base: base, client: &http.Client{Timeout: paymentTimeout, Transport: otelhttp.NewTransport(http.DefaultTransport)}}
}

// Pay charges the order's token for its total. The order ID is the
// idempotency key, so repeating the call never charges twice (D51).
func (p *Payments) Pay(ctx context.Context, o Order) (paymentResult, error) {
	start := time.Now()
	res, err := p.pay(ctx, o)
	outcome := res.Outcome
	if err != nil {
		outcome = outcomeUnknown
	}
	paymentCalls.WithLabelValues(outcome).Observe(time.Since(start).Seconds())
	return res, err
}

func (p *Payments) pay(ctx context.Context, o Order) (paymentResult, error) {
	gross := o.Priced.Totals.Gross
	body, _ := json.Marshal(map[string]any{"order_id": o.ID, "token": o.PaymentToken, "amount": gross, "currency": currency}) // cannot fail: plain data
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base.JoinPath("/v1/payments").String(), bytes.NewReader(body))
	if err != nil {
		return paymentResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(idempotency.Header, idempotency.Quote(o.ID))
	resp, err := p.client.Do(req)
	if err != nil {
		return paymentResult{}, fmt.Errorf("%w: %w", errUnknownOutcome, err)
	}
	defer func() { _ = resp.Body.Close() }() // a failed close of a read body loses nothing
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(raw) > maxResponseBytes {
		return paymentResult{}, fmt.Errorf("%w: unreadable %d response", errUnknownOutcome, resp.StatusCode)
	}
	// An answer about another order or amount settles nothing.
	var answer struct {
		OrderID     string `json:"order_id"`
		Amount      int64  `json:"amount"`
		Currency    string `json:"currency"`
		Status      any    `json:"status"` // a string on a payment, a number on a problem
		Code        string `json:"code"`
		DeclineCode string `json:"decline_code"`
	}
	if json.Unmarshal(raw, &answer) != nil {
		answer.OrderID = ""
	}
	switch {
	case resp.StatusCode == http.StatusCreated && answer.OrderID == o.ID && answer.Amount == gross &&
		answer.Currency == currency && answer.Status == "succeeded":
		return paymentResult{Outcome: outcomePaid}, nil
	case resp.StatusCode == http.StatusPaymentRequired && answer.OrderID == o.ID &&
		answer.Code == "payment_declined" && declineCode.MatchString(answer.DeclineCode):
		return paymentResult{Outcome: outcomeDeclined, DeclineCode: answer.DeclineCode}, nil
	case resp.StatusCode == http.StatusAccepted && answer.OrderID == o.ID && answer.Amount == gross &&
		answer.Currency == currency && answer.Status == "pending":
		return paymentResult{Outcome: outcomePending}, nil
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity:
		// order-api sent a request payment-api refuses, which a retry cannot
		// fix: an error worth alerting on.
		return paymentResult{}, fmt.Errorf("payment-api rejected the payment request with %d", resp.StatusCode)
	}
	// 409 (a concurrent call with this key), 5xx, and a 201, 202 or 402 that
	// does not match the order leave the charge unknown.
	return paymentResult{}, fmt.Errorf("%w: payment-api returned %d", errUnknownOutcome, resp.StatusCode)
}
