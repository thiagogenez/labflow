package order

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	checkouts = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "order_checkouts_total",
		Help: "Checkout requests that reached the payment step, by outcome: paid, declined, pending at the provider, or unknown (payment-api unreachable or inconsistent).",
	}, []string{"outcome"})
	priceChanges = promauto.NewCounter(prometheus.CounterOpts{
		Name: "order_price_changes_total",
		Help: "Checkouts refused because the total differed from the one the customer was shown.",
	})
	errUnknownSKU = errors.New("unknown SKU")
	// errKeyReused means the idempotency key already belongs to a different order.
	errKeyReused = errors.New("the idempotency key was used for a different order")
)

// errPriceChanged carries the current quote when the total differs from the
// one the customer was shown (D66).
type errPriceChanged struct{ quote Priced }

func (e *errPriceChanged) Error() string {
	return "the total differs from the one the customer was shown"
}

// QuoteRequest is what pricing needs: no personal data.
type QuoteRequest struct {
	Country string `json:"shipping_country"`
	Speed   string `json:"shipping_speed"`
	Items   []Item `json:"lines"`
}

// CheckoutRequest is what the customer submits; prices come from the catalogue.
type CheckoutRequest struct {
	Customer      Customer `json:"customer"`
	Address       Address  `json:"shipping_address"`
	Speed         string   `json:"shipping_speed"`
	Items         []Item   `json:"lines"`
	PaymentToken  string   `json:"payment_token"`
	ExpectedTotal int64    `json:"expected_total"`
}

// Service runs checkout.
type Service struct {
	store    *Store
	payments *Payments
	rates    FlatRates
	log      *slog.Logger
}

// NewService wires the store, the payment-api client and the flat rates.
func NewService(store *Store, payments *Payments, rates FlatRates, log *slog.Logger) *Service {
	return &Service{store: store, payments: payments, rates: rates, log: log}
}

// Quote prices the items for a destination, storing nothing.
func (s *Service) Quote(ctx context.Context, req QuoteRequest) (Priced, error) {
	skus := make([]string, len(req.Items))
	for i, it := range req.Items {
		skus[i] = it.SKU
	}
	products, err := s.store.ProductsBySKU(ctx, skus)
	if err != nil {
		return Priced{}, err
	}
	return price(req.Items, products, req.Country, req.Speed, s.rates)
}

// Checkout records the order under the idempotency key and charges it unless
// it is already settled. fingerprint is the idempotency middleware's, so both
// judge "the same request" alike. A retry with the same key resumes the same
// order, so a lost response never charges twice; a new order is priced first
// and refused, uncharged, when its total is not the one the customer was
// shown (D66).
func (s *Service) Checkout(ctx context.Context, key string, fingerprint []byte, req CheckoutRequest) (Order, error) {
	o, storedFingerprint, err := s.store.ByKey(ctx, key)
	if errors.Is(err, errNotFound) {
		o, storedFingerprint, err = s.create(ctx, key, fingerprint, req)
	}
	if err != nil {
		return Order{}, err
	}
	if !bytes.Equal(storedFingerprint, fingerprint) {
		return Order{}, errKeyReused
	}
	if o.Status != StatusPendingPayment {
		return o, nil
	}
	o, err = s.pay(ctx, o)
	if err != nil {
		return Order{}, fmt.Errorf("order %s: %w", o.ID, err)
	}
	return o, nil
}

func (s *Service) create(ctx context.Context, key string, fingerprint []byte, req CheckoutRequest) (Order, []byte, error) {
	priced, err := s.Quote(ctx, QuoteRequest{Country: req.Address.Country, Speed: req.Speed, Items: req.Items})
	if err != nil {
		return Order{}, nil, err
	}
	if priced.Totals.Gross != req.ExpectedTotal {
		priceChanges.Inc()
		return Order{}, nil, &errPriceChanged{quote: priced}
	}
	o := Order{ID: uuid.Must(uuid.NewV7()).String(), Customer: req.Customer, Address: req.Address,
		Priced: priced, PaymentToken: req.PaymentToken}
	created, err := s.store.Create(ctx, key, fingerprint, &o)
	if err != nil || created {
		return o, fingerprint, err
	}
	// A concurrent request with the same key created the order first.
	return s.store.ByKey(ctx, key)
}

// pay charges a pending order and records the outcome. It returns the order
// as stored, unchanged when the outcome is unknown.
func (s *Service) pay(ctx context.Context, o Order) (Order, error) {
	res, err := s.payments.Pay(ctx, o)
	switch {
	case errors.Is(err, errUnknownOutcome):
		checkouts.WithLabelValues(outcomeUnknown).Inc()
		s.log.WarnContext(ctx, "payment outcome unknown; the order stays pending", "order_id", o.ID, "error", err)
		return o, nil
	case err != nil:
		return o, err
	}
	checkouts.WithLabelValues(res.Outcome).Inc()
	switch res.Outcome {
	case outcomePaid:
		eventID, payload, err := placedEvent(ctx, o)
		if err != nil {
			return o, err
		}
		changed, err := s.store.MarkPaid(ctx, o.ID, eventID, payload)
		if err != nil {
			return o, err
		}
		if changed {
			s.log.InfoContext(ctx, "order paid", "order_id", o.ID, "event_id", eventID)
		}
	case outcomeDeclined:
		changed, err := s.store.MarkFailed(ctx, o.ID, res.DeclineCode)
		if err != nil {
			return o, err
		}
		if changed {
			s.log.InfoContext(ctx, "order payment declined", "order_id", o.ID, "decline_code", res.DeclineCode)
		}
	case outcomePending:
		s.log.InfoContext(ctx, "payment pending at the provider; the order stays pending until reconciled", "order_id", o.ID)
		return o, nil
	}
	// Read back: a concurrent retry may have settled the order first.
	return s.store.Get(ctx, o.ID)
}

// Get returns an order.
func (s *Service) Get(ctx context.Context, id string) (Order, error) {
	return s.store.Get(ctx, id)
}

// Products returns the products on sale.
func (s *Service) Products(ctx context.Context) ([]Product, error) {
	return s.store.Products(ctx)
}
