// Package payment charges orders through the card payment provider and keeps
// each order's payment state.
package payment

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// resolveWorkers settle pending payments in parallel; each claims one at a time.
const resolveWorkers = 4

var (
	settled = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "payments_settled_total",
		Help: "Payments that reached a final status, by status.",
	}, []string{"status"})
	rejections = promauto.NewCounter(prometheus.CounterOpts{
		Name: "payment_provider_rejections_total",
		Help: "Payments failed because the provider rejected the request itself, not the card: an integration fault.",
	})
	unknownOutcomes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "payment_provider_unknown_outcomes_total",
		Help: "Provider calls whose outcome is unknown, by reason; the payment stays pending.",
	}, []string{"reason"})
	oldestPending = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "payments_pending_oldest_age_seconds",
		Help: "Age of the oldest pending payment, measured by the resolver.",
	})
	tracer = otel.Tracer("github.com/thiagogenez/labflow/services/payment-api/internal/payment")
)

// errConflict means the order already has a payment with different details.
var errConflict = errors.New("the order already has a different payment")

// Service charges payments and settles the ones left pending.
type Service struct {
	store    *Store
	provider *Provider
	log      *slog.Logger
}

// NewService wires the store and the provider client.
func NewService(store *Store, provider *Provider, log *slog.Logger) *Service {
	return &Service{store: store, provider: provider, log: log}
}

// Pay records the payment, then charges it. A payment the order already has
// is returned as it is, or charged again if still pending: the provider's
// idempotency key makes that safe.
func (s *Service) Pay(ctx context.Context, p Payment) (Payment, error) {
	stored, created, err := s.store.Create(ctx, p)
	if err != nil {
		return Payment{}, err
	}
	if !created && (stored.Amount != p.Amount || stored.Currency != p.Currency || stored.Token != p.Token) {
		return Payment{}, errConflict
	}
	if stored.Status != StatusPending {
		return stored, nil
	}
	return s.attempt(ctx, stored)
}

// Get returns the order's payment.
func (s *Service) Get(ctx context.Context, orderID string) (Payment, error) {
	return s.store.Get(ctx, orderID)
}

// attempt asks the provider about a pending payment: it reads the charge when
// the provider has already assigned one, and repeats the idempotent charge
// otherwise. An unknown outcome leaves the payment pending.
func (s *Service) attempt(ctx context.Context, p Payment) (Payment, error) {
	var (
		c   providerCharge
		err error
	)
	if p.ChargeID != "" {
		c, err = s.provider.Get(ctx, p.ChargeID)
	} else {
		c, err = s.provider.Charge(ctx, p)
	}
	var u *unknownOutcome
	if errors.As(err, &u) {
		unknownOutcomes.WithLabelValues(u.reason).Inc()
		s.log.WarnContext(ctx, "payment outcome unknown; it stays pending",
			"order_id", p.OrderID, "attempts", p.Attempts, "reason", u.reason, "error", u.err)
		return p, nil
	}
	if err != nil {
		return Payment{}, err
	}
	if c.Status == StatusPending && c.ID == p.ChargeID {
		return p, nil
	}
	updated, changed, err := s.store.Record(ctx, p.OrderID, c)
	if err != nil {
		return Payment{}, err
	}
	if changed && updated.Status != StatusPending {
		settled.WithLabelValues(updated.Status).Inc()
		if c.Rejected {
			rejections.Inc()
		}
		s.log.InfoContext(ctx, "payment settled", "order_id", p.OrderID, "status", updated.Status,
			"decline_code", updated.DeclineCode, "attempts", p.Attempts)
	}
	return updated, nil
}

// ResolvePending settles the pending payments untouched for lease, with
// several workers that each claim one payment at a time, until none is left
// or ctx ends. It returns how many it tried.
func (s *Service) ResolvePending(ctx context.Context, lease time.Duration) int {
	var tried atomic.Int64
	var wg sync.WaitGroup
	for range resolveWorkers {
		wg.Go(func() {
			for ctx.Err() == nil {
				p, ok, err := s.store.ClaimNext(ctx, lease)
				if err != nil {
					if ctx.Err() == nil {
						s.log.WarnContext(ctx, "claim pending payment failed", "error", err)
					}
					return
				}
				if !ok {
					return
				}
				tried.Add(1)
				s.resolve(ctx, p)
			}
		})
	}
	wg.Wait()
	return int(tried.Load())
}

func (s *Service) resolve(ctx context.Context, p Payment) {
	ctx, span := tracer.Start(ctx, "resolve payment")
	defer span.End()
	span.SetAttributes(attribute.String("labflow.order_id", p.OrderID), attribute.Int("labflow.attempts", p.Attempts))
	if _, err := s.attempt(ctx, p); err != nil {
		s.log.ErrorContext(ctx, "resolve payment failed", "order_id", p.OrderID, "error", err)
	}
}

// RunResolver settles pending payments every interval until ctx ends. Each
// round stops by the next one, so a claim's lease (one interval) outlives the
// work done under it. Several replicas may run it: each claims different
// payments.
func (s *Service) RunResolver(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.measureBacklog(ctx)
			round, cancel := context.WithTimeout(ctx, interval)
			s.ResolvePending(round, interval)
			cancel()
		}
	}
}

// measureBacklog sets the oldest-pending gauge before a round, with its own
// deadline, so the gauge keeps moving while rounds run out of time.
func (s *Service) measureBacklog(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	age, err := s.store.OldestPending(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "measure pending backlog failed", "error", err)
		return
	}
	oldestPending.Set(age.Seconds())
}
