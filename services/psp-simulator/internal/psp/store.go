package psp

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thiagogenez/labflow/services/psp-simulator/internal/card"
)

// Charge statuses.
const (
	StatusPending   = "pending"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// The only decline the simulator produces.
const declineCode = "insufficient_funds"

var (
	errUnknownToken = errors.New("unknown token")
	errTokenUsed    = errors.New("token already used")
	errKeyReused    = errors.New("idempotency key already used for a different charge")
	errNotFound     = errors.New("charge not found")
)

// Token is a single-use reference to a card.
type Token struct {
	ID      string    `json:"id"`
	Object  string    `json:"object"`
	Card    TokenCard `json:"card"`
	Created time.Time `json:"created"`
}

// TokenCard is the part of the card a merchant may see.
type TokenCard struct {
	Brand card.Brand `json:"brand"`
	Last4 string     `json:"last4"`
}

// Charge is a payment attempt on a token.
type Charge struct {
	ID          string            `json:"id"`
	Object      string            `json:"object"`
	Amount      int64             `json:"amount"`
	Currency    string            `json:"currency"`
	Status      string            `json:"status"`
	FailureCode string            `json:"failure_code,omitempty"`
	Metadata    map[string]string `json:"metadata"`
	Created     time.Time         `json:"created"`
}

// ChargeRequest is a merchant's request to charge a token.
type ChargeRequest struct {
	Token          string
	IdempotencyKey string
	Amount         int64
	Currency       string
	Metadata       map[string]string
}

// Store persists tokens and charges.
type Store struct{ pool *pgxpool.Pool }

// NewStore wraps a pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// CreateToken stores the card and returns its token.
func (s *Store) CreateToken(ctx context.Context, c card.Card) (Token, error) {
	t := Token{ID: "tok_" + rand.Text(), Object: "token", Card: TokenCard{Brand: c.Brand, Last4: c.Last4}}
	err := s.pool.QueryRow(ctx, `INSERT INTO tokens (id, brand, last4, outcome) VALUES ($1, $2, $3, $4)
		RETURNING created_at`, t.ID, c.Brand, c.Last4, c.Outcome).Scan(&t.Created)
	return t, err
}

// Outcome returns what a charge on the token will do.
func (s *Store) Outcome(ctx context.Context, tokenID string) (card.Outcome, error) {
	var o card.Outcome
	err := s.pool.QueryRow(ctx, `SELECT outcome FROM tokens WHERE id = $1`, tokenID).Scan(&o)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errUnknownToken
	}
	return o, err
}

// CreateCharge charges the token once and reports whether it created the
// charge. Repeating the same request with the same idempotency key returns
// the charge it created, even after the HTTP idempotency record expired; the
// key with a different request fails with errKeyReused, and another key on a
// used token with errTokenUsed.
func (s *Store) CreateCharge(ctx context.Context, req ChargeRequest, outcome card.Outcome, pendingDelay time.Duration) (Charge, bool, error) {
	c := Charge{ID: "ch_" + rand.Text(), Object: "charge", Amount: req.Amount, Currency: req.Currency, Metadata: req.Metadata}
	var resolvesTo *string
	switch outcome {
	case card.Decline:
		c.Status, c.FailureCode = StatusFailed, declineCode
	case card.Pending, card.PendingDecline:
		c.Status = StatusPending
		final := StatusSucceeded
		if outcome == card.PendingDecline {
			final = StatusFailed
		}
		resolvesTo = &final
	default:
		c.Status = StatusSucceeded
	}
	if c.Metadata == nil {
		c.Metadata = map[string]string{}
	}
	meta, err := json.Marshal(c.Metadata)
	if err != nil {
		return Charge{}, false, err
	}
	err = s.pool.QueryRow(ctx, `INSERT INTO charges
		(id, token_id, idempotency_key, amount, currency, status, failure_code, resolves_to, resolve_at, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8, now() + make_interval(secs => $9), $10)
		ON CONFLICT DO NOTHING RETURNING created_at`,
		c.ID, req.Token, req.IdempotencyKey, req.Amount, req.Currency, c.Status, c.FailureCode,
		resolvesTo, pendingDelay.Seconds(), meta).Scan(&c.Created)
	if !errors.Is(err, pgx.ErrNoRows) {
		return c, err == nil, err
	}
	var id string
	var same bool
	err = s.pool.QueryRow(ctx, `SELECT id, token_id = $2 AND amount = $3 AND currency = $4 AND metadata = $5::jsonb
		FROM charges WHERE idempotency_key = $1`,
		req.IdempotencyKey, req.Token, req.Amount, req.Currency, meta).Scan(&id, &same)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Charge{}, false, errTokenUsed
	case err != nil:
		return Charge{}, false, err
	case !same:
		return Charge{}, false, errKeyReused
	}
	existing, err := s.Charge(ctx, id)
	return existing, false, err
}

// Charge returns a charge, settling it first if it was pending and its time has come.
func (s *Store) Charge(ctx context.Context, id string) (Charge, error) {
	if _, err := s.pool.Exec(ctx, `UPDATE charges
		SET status = resolves_to,
		    failure_code = CASE WHEN resolves_to = 'failed' THEN $2 END
		WHERE id = $1 AND status = 'pending' AND resolve_at <= now()`, id, declineCode); err != nil {
		return Charge{}, err
	}
	c := Charge{Object: "charge"}
	var failure *string
	var meta []byte
	err := s.pool.QueryRow(ctx, `SELECT id, amount, currency, status, failure_code, metadata, created_at
		FROM charges WHERE id = $1`, id).Scan(&c.ID, &c.Amount, &c.Currency, &c.Status, &failure, &meta, &c.Created)
	if errors.Is(err, pgx.ErrNoRows) {
		return Charge{}, errNotFound
	}
	if err != nil {
		return Charge{}, err
	}
	if failure != nil {
		c.FailureCode = *failure
	}
	if err := json.Unmarshal(meta, &c.Metadata); err != nil {
		return Charge{}, err
	}
	return c, nil
}
