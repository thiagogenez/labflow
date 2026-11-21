package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/thiagogenez/labflow/libs/go/idempotency"
)

// maxResponseBytes bounds what is read from the provider.
const maxResponseBytes = 64 << 10

// rejectionCodes are the provider's answers to a request it will never accept;
// they end the payment. Any other 400 is an unknown outcome.
var rejectionCodes = map[string]bool{
	"invalid_token": true, "token_already_used": true, "invalid_amount": true,
	"invalid_currency": true, "invalid_metadata": true,
}

// unknownOutcome means the provider's answer does not settle the payment; the
// reason is a bounded metric label.
type unknownOutcome struct {
	reason string
	err    error
}

func (u *unknownOutcome) Error() string { return u.reason + ": " + u.err.Error() }
func (u *unknownOutcome) Unwrap() error { return u.err }

func unknown(reason string, format string, args ...any) error {
	return &unknownOutcome{reason: reason, err: fmt.Errorf(format, args...)}
}

// providerCharge is the provider's view of a charge.
type providerCharge struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	FailureCode string `json:"failure_code"`
	// Rejected marks a request the provider refused as invalid, as opposed
	// to a card decline; both end the payment.
	Rejected bool `json:"-"`
}

// Provider calls the card payment provider's API.
type Provider struct {
	base      *url.URL
	secretKey string
	client    *http.Client
}

// NewProvider returns a client whose every call, body included, is bounded by timeout.
func NewProvider(base *url.URL, secretKey string, timeout time.Duration) *Provider {
	return &Provider{base: base, secretKey: secretKey,
		client: &http.Client{Timeout: timeout, Transport: otelhttp.NewTransport(http.DefaultTransport)}}
}

// Charge asks the provider to charge the payment's token. The order ID is the
// idempotency key, so repeating the call never charges twice: the provider
// returns the original outcome instead.
func (p *Provider) Charge(ctx context.Context, pay Payment) (providerCharge, error) {
	body, err := json.Marshal(map[string]any{
		"amount": pay.Amount, "currency": pay.Currency, "token": pay.Token,
		"metadata": map[string]string{"order_id": pay.OrderID},
	})
	if err != nil {
		return providerCharge{}, err
	}
	status, resp, err := p.do(ctx, http.MethodPost, "/v1/charges", pay.OrderID, body)
	if err != nil {
		return providerCharge{}, err
	}
	switch status {
	case http.StatusCreated:
		var c providerCharge
		if err := json.Unmarshal(resp, &c); err != nil || c.ID == "" || (c.Status != StatusSucceeded && c.Status != StatusPending) {
			return providerCharge{}, unknown("malformed", "a created charge without an ID or a known status")
		}
		return c, nil
	case http.StatusPaymentRequired, http.StatusBadRequest:
		var problem struct {
			Charge      string `json:"charge"`
			Code        string `json:"code"`
			DeclineCode string `json:"decline_code"`
		}
		if err := json.Unmarshal(resp, &problem); err != nil {
			return providerCharge{}, unknown("malformed", "a %d without a problem body", status)
		}
		if status == http.StatusPaymentRequired {
			if problem.Code != "card_declined" || problem.Charge == "" || problem.DeclineCode == "" {
				return providerCharge{}, unknown("malformed", "a 402 that is not a complete card decline")
			}
			return providerCharge{ID: problem.Charge, Status: StatusFailed, FailureCode: problem.DeclineCode}, nil
		}
		// A retry with this payment's key finds its own charge, so a used
		// token here means another order spent it: final for this payment.
		if !rejectionCodes[problem.Code] {
			return providerCharge{}, unknown("malformed", "a 400 with an unknown code")
		}
		return providerCharge{Status: StatusFailed, FailureCode: problem.Code, Rejected: true}, nil
	case http.StatusConflict:
		return providerCharge{}, unknown("busy", "the provider is still processing this charge")
	}
	return providerCharge{}, unknown(statusReason(status), "provider returned %d", status)
}

// Get reads a charge's current state.
func (p *Provider) Get(ctx context.Context, chargeID string) (providerCharge, error) {
	status, resp, err := p.do(ctx, http.MethodGet, "/v1/charges/"+url.PathEscape(chargeID), "", nil)
	if err != nil {
		return providerCharge{}, err
	}
	if status != http.StatusOK {
		return providerCharge{}, unknown(statusReason(status), "provider returned %d", status)
	}
	var c providerCharge
	if err := json.Unmarshal(resp, &c); err != nil || c.ID != chargeID ||
		(c.Status != StatusPending && c.Status != StatusSucceeded && c.Status != StatusFailed) ||
		(c.Status == StatusFailed && c.FailureCode == "") {
		return providerCharge{}, unknown("malformed", "a charge that is not %s or has an unknown status", chargeID)
	}
	return c, nil
}

func (p *Provider) do(ctx context.Context, method, path, idemKey string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, p.base.JoinPath(path).String(), bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.secretKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idemKey != "" {
		req.Header.Set(idempotency.Header, idempotency.Quote(idemKey))
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, nil, transportOutcome(ctx, "call provider", err)
	}
	defer func() { _ = resp.Body.Close() }() // a failed close of a read body loses nothing
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return 0, nil, transportOutcome(ctx, "read provider response", err)
	}
	if len(b) > maxResponseBytes {
		return 0, nil, unknown("malformed", "a response larger than %d bytes", maxResponseBytes)
	}
	return resp.StatusCode, b, nil
}

// transportOutcome tells our own cancellation (a resolver round ending) from a
// provider timeout and from a network failure.
func transportOutcome(ctx context.Context, op string, err error) error {
	var netErr net.Error
	switch {
	case ctx.Err() != nil:
		return unknown("canceled", "%s: %w", op, err)
	case errors.As(err, &netErr) && netErr.Timeout():
		return unknown("timeout", "%s: %w", op, err)
	}
	return unknown("network", "%s: %w", op, err)
}

func statusReason(status int) string {
	switch {
	case status >= http.StatusInternalServerError:
		return "http_5xx"
	case status >= http.StatusBadRequest:
		return "http_4xx"
	}
	return "unexpected_status"
}
