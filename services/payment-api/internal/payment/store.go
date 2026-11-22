package payment

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Payment statuses.
const (
	StatusPending   = "pending"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

var errNotFound = errors.New("payment not found")

// Payment is the charge for one order.
type Payment struct {
	OrderID     string
	Token       string
	Amount      int64
	Currency    string
	Status      string
	ChargeID    string
	DeclineCode string
	Attempts    int
}

// Store persists payments.
type Store struct{ pool *pgxpool.Pool }

// NewStore wraps a pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const columns = `order_id::text, token, amount, currency, status, coalesce(psp_charge_id, ''), coalesce(decline_code, ''), attempts`

func scan(row pgx.Row) (Payment, error) {
	var p Payment
	err := row.Scan(&p.OrderID, &p.Token, &p.Amount, &p.Currency, &p.Status, &p.ChargeID, &p.DeclineCode, &p.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, errNotFound
	}
	return p, err
}

// Create records a new pending payment. If the order already has one, it
// returns that payment and false.
func (s *Store) Create(ctx context.Context, p Payment) (Payment, bool, error) {
	created, err := scan(s.pool.QueryRow(ctx, `INSERT INTO payments (order_id, token, amount, currency, status)
		VALUES ($1, $2, $3, $4, 'pending') ON CONFLICT (order_id) DO NOTHING RETURNING `+columns,
		p.OrderID, p.Token, p.Amount, p.Currency))
	if errors.Is(err, errNotFound) {
		existing, err := s.Get(ctx, p.OrderID)
		return existing, false, err
	}
	return created, err == nil, err
}

// Get returns the order's payment.
func (s *Store) Get(ctx context.Context, orderID string) (Payment, error) {
	return scan(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM payments WHERE order_id = $1`, orderID))
}

// Record stores the provider's answer on a pending payment and reports
// whether this call changed it. A payment that has already settled is
// returned unchanged.
func (s *Store) Record(ctx context.Context, orderID string, c providerCharge) (Payment, bool, error) {
	p, err := scan(s.pool.QueryRow(ctx, `UPDATE payments
		SET status = $2, psp_charge_id = coalesce(NULLIF($3, ''), psp_charge_id),
		    decline_code = NULLIF($4, ''), updated_at = now()
		WHERE order_id = $1 AND status = 'pending' RETURNING `+columns,
		orderID, c.Status, c.ID, c.FailureCode))
	if errors.Is(err, errNotFound) {
		p, err = s.Get(ctx, orderID)
		return p, false, err
	}
	return p, err == nil, err
}

// ClaimNext takes the pending payment untouched for longest, if it has been
// untouched for lease. Touching it gives the caller lease to settle it before
// anyone may claim it again.
func (s *Store) ClaimNext(ctx context.Context, lease time.Duration) (Payment, bool, error) {
	p, err := scan(s.pool.QueryRow(ctx, `UPDATE payments SET updated_at = now(), attempts = attempts + 1
		WHERE order_id = (
			SELECT order_id FROM payments
			WHERE status = 'pending' AND updated_at < now() - make_interval(secs => $1)
			ORDER BY updated_at LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING `+columns, lease.Seconds()))
	if errors.Is(err, errNotFound) {
		return Payment{}, false, nil
	}
	return p, err == nil, err
}

// OldestPending returns the age of the oldest pending payment, or zero.
func (s *Store) OldestPending(ctx context.Context) (time.Duration, error) {
	var seconds float64
	err := s.pool.QueryRow(ctx, `SELECT coalesce(extract(epoch FROM now() - min(created_at)), 0)::float8
		FROM payments WHERE status = 'pending'`).Scan(&seconds)
	return time.Duration(seconds * float64(time.Second)), err
}
