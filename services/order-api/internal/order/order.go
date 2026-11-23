// Package order runs checkout: it prices orders from the catalogue, charges
// them through payment-api and records paid orders with their order.placed
// event in the outbox.
package order

import (
	"fmt"
	"regexp"
	"time"
)

// Order statuses.
const (
	StatusPendingPayment = "pending_payment"
	StatusPaid           = "paid"
	StatusPaymentFailed  = "payment_failed"
)

const (
	// RateSourceFlat marks shipping priced from the flat-rate table (D14).
	RateSourceFlat = "flat_rate"
	// currency is the only one the shop sells in (D54).
	currency = "EUR"
)

// skuPattern is the event contract's SKU format.
var skuPattern = regexp.MustCompile(`^[A-Z0-9]+(-[A-Z0-9]+)*$`)

func validSKU(s string) bool { return len(s) <= 32 && skuPattern.MatchString(s) }

// Product is a catalogue entry.
type Product struct {
	SKU            string `json:"sku"`
	Name           string `json:"name"`
	UnitPriceGross int64  `json:"unit_price_gross"`
	TaxRateBps     int    `json:"tax_rate_bps"`
	WeightGrams    int    `json:"weight_grams"`
}

// Customer is who pays and receives the emails.
type Customer struct {
	Email     string `json:"email"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

// Address is where the order ships; it never leaves order-api (D66).
type Address struct {
	Line1      string `json:"line1"`
	Line2      string `json:"line2"`
	PostalCode string `json:"postal_code"`
	City       string `json:"city"`
	Country    string `json:"country"`
}

// Line is one priced product of an order.
type Line struct {
	Number         int    `json:"line_number"`
	SKU            string `json:"sku"`
	Name           string `json:"name"`
	Quantity       int    `json:"quantity"`
	UnitPriceGross int64  `json:"unit_price_gross"`
	TaxRateBps     int    `json:"tax_rate_bps"`
	Amounts
}

// Shipping is the priced delivery of an order. Its VAT follows the goods'
// rates (D54), so it carries one part per rate.
type Shipping struct {
	Zone       string `json:"zone"`
	Speed      string `json:"speed"`
	RateSource string `json:"rate_source"`
	Amounts
	TaxBreakdown []TaxedAmounts `json:"tax_breakdown"`
}

// Priced is the priced part of an order, as quotes, orders and the
// order.placed event all show it.
type Priced struct {
	Currency string   `json:"currency"`
	Lines    []Line   `json:"lines"`
	Shipping Shipping `json:"shipping"`
	Totals   Amounts  `json:"totals"`
}

// Order is a priced order and its payment state.
type Order struct {
	ID           string
	Status       string
	Customer     Customer
	Address      Address
	Priced       Priced
	PaymentToken string
	DeclineCode  string
	CreatedAt    time.Time
}

// Item is a requested quantity of a product.
type Item struct {
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
}

// price prices the requested items for a destination from the catalogue and
// the flat rates. Amounts are rounded per line, and totals are the sums of
// the lines and shipping (D54).
func price(items []Item, products map[string]Product, country, speed string, rates FlatRates) (Priced, error) {
	p := Priced{Currency: currency}
	var err error
	goodsByRate := make(map[int]int64)
	for i, it := range items {
		prod, ok := products[it.SKU]
		if !ok {
			return Priced{}, fmt.Errorf("%w: %s", errUnknownSKU, it.SKU)
		}
		gross := prod.UnitPriceGross * int64(it.Quantity)
		if gross > maxAmount {
			return Priced{}, errTooLarge
		}
		line := Line{Number: i + 1, SKU: prod.SKU, Name: prod.Name, Quantity: it.Quantity,
			UnitPriceGross: prod.UnitPriceGross, TaxRateBps: prod.TaxRateBps, Amounts: split(gross, prod.TaxRateBps)}
		p.Lines = append(p.Lines, line)
		goodsByRate[line.TaxRateBps] += line.Gross
		if p.Totals, err = add(p.Totals, line.Amounts); err != nil {
			return Priced{}, err
		}
	}
	zone := zones[country]
	p.Shipping = Shipping{Zone: zone, Speed: speed, RateSource: RateSourceFlat,
		TaxBreakdown: splitShipping(rates[zone][speed], goodsByRate)}
	for _, part := range p.Shipping.TaxBreakdown {
		p.Shipping.Net += part.Net
		p.Shipping.Tax += part.Tax
		p.Shipping.Gross += part.Gross
	}
	if p.Totals, err = add(p.Totals, p.Shipping.Amounts); err != nil {
		return Priced{}, err
	}
	return p, nil
}
