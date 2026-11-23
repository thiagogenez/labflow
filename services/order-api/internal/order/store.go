package order

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var errNotFound = errors.New("order not found")

// Store persists the catalogue, orders and the outbox.
type Store struct{ pool *pgxpool.Pool }

// NewStore wraps a pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// catalogueLockID serialises catalogue imports.
const catalogueLockID = 7_271_012

// ImportResult counts what an import changed.
type ImportResult struct{ Changed, Unchanged, Retired int }

const productColumns = `sku, name, unit_price_gross, tax_rate_bps, weight_grams`

// ImportProducts makes the products on sale exactly the given ones, in one
// transaction (D68): it upserts them by SKU, puts a retired one back on sale,
// and retires the rest, which stay for their past orders. Changed counts the
// products inserted or updated.
func (s *Store) ImportProducts(ctx context.Context, products []Product) (ImportResult, error) {
	var res ImportResult
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Two concurrent imports would each retire nothing the other inserts.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, catalogueLockID); err != nil {
			return err
		}
		skus := make([]string, len(products))
		for i, p := range products {
			skus[i] = p.SKU
			tag, err := tx.Exec(ctx, `INSERT INTO products (`+productColumns+`) VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (sku) DO UPDATE SET name = excluded.name, unit_price_gross = excluded.unit_price_gross,
					tax_rate_bps = excluded.tax_rate_bps, weight_grams = excluded.weight_grams, retired_at = NULL, updated_at = now()
				WHERE (products.name, products.unit_price_gross, products.tax_rate_bps, products.weight_grams, products.retired_at)
					IS DISTINCT FROM (excluded.name, excluded.unit_price_gross, excluded.tax_rate_bps, excluded.weight_grams, NULL)`,
				p.SKU, p.Name, p.UnitPriceGross, p.TaxRateBps, p.WeightGrams)
			if err != nil {
				return fmt.Errorf("product %s: %w", p.SKU, err)
			}
			res.Changed += int(tag.RowsAffected())
		}
		res.Unchanged = len(products) - res.Changed
		tag, err := tx.Exec(ctx, `UPDATE products SET retired_at = now(), updated_at = now()
			WHERE retired_at IS NULL AND sku <> ALL($1)`, skus)
		res.Retired = int(tag.RowsAffected())
		return err
	})
	return res, err
}

// Products returns the products on sale, ordered by SKU.
func (s *Store) Products(ctx context.Context) ([]Product, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+productColumns+` FROM products WHERE retired_at IS NULL ORDER BY sku`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Product])
}

// ProductsBySKU returns the products on sale among the SKUs.
func (s *Store) ProductsBySKU(ctx context.Context, skus []string) (map[string]Product, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+productColumns+` FROM products WHERE sku = ANY($1) AND retired_at IS NULL`, skus)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Product])
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]Product, len(list))
	for _, p := range list {
		byKey[p.SKU] = p
	}
	return byKey, nil
}

// Create stores a new pending order under the idempotency key with the
// request's fingerprint (D66). It reports false when the key already has an
// order, for example from a concurrent retry; the caller then resumes that one.
func (s *Store) Create(ctx context.Context, key string, requestHash []byte, o *Order) (bool, error) {
	created := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO orders (id, idempotency_key, request_hash, status, email, first_name, last_name,
				address_line1, address_line2, postal_code, city, country,
				shipping_zone, shipping_speed, shipping_rate_source, shipping_net, shipping_tax, shipping_gross,
				net_total, tax_total, gross_total, payment_token)
			VALUES ($1, $2, $3, 'pending_payment', $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)
			ON CONFLICT (idempotency_key) DO NOTHING RETURNING created_at`,
			o.ID, key, requestHash, o.Customer.Email, o.Customer.FirstName, o.Customer.LastName,
			o.Address.Line1, o.Address.Line2, o.Address.PostalCode, o.Address.City, o.Address.Country,
			o.Priced.Shipping.Zone, o.Priced.Shipping.Speed, o.Priced.Shipping.RateSource, o.Priced.Shipping.Net, o.Priced.Shipping.Tax, o.Priced.Shipping.Gross,
			o.Priced.Totals.Net, o.Priced.Totals.Tax, o.Priced.Totals.Gross, o.PaymentToken,
		).Scan(&o.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		o.Status, created = StatusPendingPayment, true
		lines := make([][]any, len(o.Priced.Lines))
		for i, l := range o.Priced.Lines {
			lines[i] = []any{o.ID, l.Number, l.SKU, l.Name, l.Quantity, l.UnitPriceGross, l.TaxRateBps, l.Net, l.Tax, l.Gross}
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"order_lines"},
			[]string{"order_id", "line_number", "sku", "name", "quantity", "unit_price_gross", "tax_rate_bps", "net_amount", "tax_amount", "gross_amount"},
			pgx.CopyFromRows(lines)); err != nil {
			return err
		}
		taxes := make([][]any, len(o.Priced.Shipping.TaxBreakdown))
		for i, t := range o.Priced.Shipping.TaxBreakdown {
			taxes[i] = []any{o.ID, t.TaxRateBps, t.Net, t.Tax, t.Gross}
		}
		_, err = tx.CopyFrom(ctx, pgx.Identifier{"order_shipping_taxes"},
			[]string{"order_id", "tax_rate_bps", "net_amount", "tax_amount", "gross_amount"}, pgx.CopyFromRows(taxes))
		return err
	})
	return created, err
}

const orderColumns = `id::text, status, email, first_name, last_name, address_line1, address_line2, postal_code, city, country,
	shipping_zone, shipping_speed, shipping_rate_source, shipping_net, shipping_tax, shipping_gross,
	net_total, tax_total, gross_total, payment_token, coalesce(decline_code, ''), created_at`

// ByKey returns the order an idempotency key belongs to, with the fingerprint
// of the request that created it.
func (s *Store) ByKey(ctx context.Context, key string) (Order, []byte, error) {
	var hash []byte
	o, err := s.load(ctx, `SELECT `+orderColumns+`, request_hash FROM orders WHERE idempotency_key = $1`, key, &hash)
	return o, hash, err
}

// Get returns an order with its lines and shipping taxes.
func (s *Store) Get(ctx context.Context, id string) (Order, error) {
	return s.load(ctx, `SELECT `+orderColumns+` FROM orders WHERE id = $1`, id)
}

func (s *Store) load(ctx context.Context, query, arg string, extra ...any) (Order, error) {
	o := Order{Priced: Priced{Currency: currency}}
	dest := append([]any{&o.ID, &o.Status, &o.Customer.Email, &o.Customer.FirstName, &o.Customer.LastName,
		&o.Address.Line1, &o.Address.Line2, &o.Address.PostalCode, &o.Address.City, &o.Address.Country,
		&o.Priced.Shipping.Zone, &o.Priced.Shipping.Speed, &o.Priced.Shipping.RateSource, &o.Priced.Shipping.Net, &o.Priced.Shipping.Tax, &o.Priced.Shipping.Gross,
		&o.Priced.Totals.Net, &o.Priced.Totals.Tax, &o.Priced.Totals.Gross, &o.PaymentToken, &o.DeclineCode, &o.CreatedAt}, extra...)
	if err := s.pool.QueryRow(ctx, query, arg).Scan(dest...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Order{}, errNotFound
		}
		return Order{}, err
	}
	rows, err := s.pool.Query(ctx, `SELECT line_number, sku, name, quantity, unit_price_gross, tax_rate_bps,
		net_amount, tax_amount, gross_amount FROM order_lines WHERE order_id = $1 ORDER BY line_number`, o.ID)
	if err != nil {
		return Order{}, err
	}
	if o.Priced.Lines, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Line, error) {
		var l Line
		err := row.Scan(&l.Number, &l.SKU, &l.Name, &l.Quantity, &l.UnitPriceGross, &l.TaxRateBps, &l.Net, &l.Tax, &l.Gross)
		return l, err
	}); err != nil {
		return Order{}, err
	}
	rows, err = s.pool.Query(ctx, `SELECT tax_rate_bps, net_amount, tax_amount, gross_amount
		FROM order_shipping_taxes WHERE order_id = $1 ORDER BY tax_rate_bps DESC`, o.ID)
	if err != nil {
		return Order{}, err
	}
	o.Priced.Shipping.TaxBreakdown, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (TaxedAmounts, error) {
		var t TaxedAmounts
		err := row.Scan(&t.TaxRateBps, &t.Net, &t.Tax, &t.Gross)
		return t, err
	})
	return o, err
}

// MarkPaid records a pending order as paid and writes its order.placed event to
// the outbox in the same transaction (D48, D51). It reports false when the
// order was no longer pending, for example because a concurrent retry got there
// first.
func (s *Store) MarkPaid(ctx context.Context, orderID, eventID string, payload []byte) (bool, error) {
	changed := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE orders SET status = 'paid', updated_at = now()
			WHERE id = $1 AND status = 'pending_payment'`, orderID)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		changed = true
		_, err = tx.Exec(ctx, `INSERT INTO outbox (id, aggregatetype, aggregateid, type, payload)
			VALUES ($1, $2, $3, $4, $5)`, eventID, outboxTopic, orderID, TypeOrderPlaced, payload)
		return err
	})
	return changed, err
}

// MarkFailed records a pending order whose payment was declined.
func (s *Store) MarkFailed(ctx context.Context, orderID, declineCode string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE orders SET status = 'payment_failed', decline_code = $2, updated_at = now()
		WHERE id = $1 AND status = 'pending_payment'`, orderID, declineCode)
	return tag.RowsAffected() > 0, err
}

var (
	pendingOrders = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "orders_pending_payment",
		Help: "Orders waiting for their payment outcome; until the reconciler arrives, only a client retry settles them.",
	})
	oldestPending = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "orders_pending_payment_oldest_age_seconds",
		Help: "Age of the oldest order waiting for its payment outcome.",
	})
)

// RunPendingGauge measures the orders left pending every interval until ctx
// ends, so a stranded payment shows up on dashboards.
func (s *Store) RunPendingGauge(ctx context.Context, interval time.Duration, log *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		var n int
		var age float64
		err := s.pool.QueryRow(ctx, `SELECT count(*), coalesce(extract(epoch FROM now() - min(created_at)), 0)::float8
			FROM orders WHERE status = 'pending_payment'`).Scan(&n, &age)
		if err != nil && ctx.Err() == nil {
			log.WarnContext(ctx, "measure pending orders failed", "error", err)
		} else if err == nil {
			pendingOrders.Set(float64(n))
			oldestPending.Set(age)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
