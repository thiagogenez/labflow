package order

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"slices"
)

// maxAmount is the largest integer every JSON parser reads exactly (RFC 7493).
const maxAmount = 1<<53 - 1

// vatRates are the German VAT rates a product may carry, in basis points:
// standard and reduced (D54).
var vatRates = map[int]bool{1900: true, 700: true}

// Shipping speeds.
const (
	SpeedStandard = "standard"
	SpeedExpress  = "express"
)

// zones maps each EU country to its shipping zone (D67).
var zones = map[string]string{
	"DE": "DE",
	"AT": "EU-1", "BE": "EU-1", "CZ": "EU-1", "DK": "EU-1", "FR": "EU-1", "LU": "EU-1", "NL": "EU-1", "PL": "EU-1",
	"ES": "EU-2", "FI": "EU-2", "HU": "EU-2", "IE": "EU-2", "IT": "EU-2", "PT": "EU-2", "SE": "EU-2", "SI": "EU-2", "SK": "EU-2",
	"BG": "EU-3", "CY": "EU-3", "EE": "EU-3", "GR": "EU-3", "HR": "EU-3", "LT": "EU-3", "LV": "EU-3", "MT": "EU-3", "RO": "EU-3",
}

// FlatRates holds the gross flat shipping rate per zone and speed (D67).
type FlatRates map[string]map[string]int64

// LoadFlatRates reads the rates file and checks it prices every zone and speed.
func LoadFlatRates(path string) (FlatRates, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rates FlatRates
	if err := json.Unmarshal(b, &rates); err != nil {
		return nil, fmt.Errorf("flat rates %s: %w", path, err)
	}
	for _, zone := range zones {
		for _, speed := range []string{SpeedStandard, SpeedExpress} {
			if r := rates[zone][speed]; r <= 0 || r > maxAmount {
				return nil, fmt.Errorf("flat rates %s: %s %s must be a positive amount in minor units", path, zone, speed)
			}
		}
	}
	return rates, nil
}

// Amounts is a gross amount split into net and VAT.
type Amounts struct {
	Net   int64 `json:"net_amount"`
	Tax   int64 `json:"tax_amount"`
	Gross int64 `json:"gross_amount"`
}

// split takes the VAT out of a gross amount, rounding the net half up (D54).
func split(gross int64, rateBps int) Amounts {
	// net = gross * 10000 / (10000 + rate), rounded; big.Int keeps every
	// product exact up to maxAmount.
	num := new(big.Int).Mul(big.NewInt(gross), big.NewInt(2*10000))
	den := big.NewInt(int64(2 * (10000 + rateBps)))
	num.Add(num, big.NewInt(int64(10000+rateBps)))
	net := num.Quo(num, den).Int64()
	return Amounts{Net: net, Tax: gross - net, Gross: gross}
}

var errTooLarge = errors.New("the order total exceeds the largest supported amount")

// add sums amounts, failing past maxAmount.
func add(a, b Amounts) (Amounts, error) {
	sum := Amounts{Net: a.Net + b.Net, Tax: a.Tax + b.Tax, Gross: a.Gross + b.Gross}
	if sum.Gross > maxAmount {
		return Amounts{}, errTooLarge
	}
	return sum, nil
}

// TaxedAmounts is the part of an amount taxed at one rate.
type TaxedAmounts struct {
	TaxRateBps int `json:"tax_rate_bps"`
	Amounts
}

// splitShipping taxes shipping at its goods' rates (D54): the gross is
// split across the rates in proportion to the goods' gross amounts, the
// largest remainders taking the leftover cents (higher rate first on a tie),
// and each part is taxed at its rate. Every goods rate gets a part, a zero one
// included, ordered by descending rate.
func splitShipping(gross int64, goodsByRate map[int]int64) []TaxedAmounts {
	rates := make([]int, 0, len(goodsByRate))
	var goods int64
	for r, g := range goodsByRate {
		rates = append(rates, r)
		goods += g
	}
	slices.SortFunc(rates, func(a, b int) int { return b - a })
	parts := make([]int64, len(rates))
	rems := make([]*big.Int, len(rates))
	var allocated int64
	for i, r := range rates {
		q, m := new(big.Int).QuoRem(new(big.Int).Mul(big.NewInt(gross), big.NewInt(goodsByRate[r])), big.NewInt(goods), new(big.Int))
		parts[i], rems[i] = q.Int64(), m
		allocated += parts[i]
	}
	order := make([]int, len(rates))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return rems[b].Cmp(rems[a]) })
	for i := int64(0); i < gross-allocated; i++ {
		parts[order[i]]++
	}
	out := make([]TaxedAmounts, len(rates))
	for i, r := range rates {
		out[i] = TaxedAmounts{TaxRateBps: r, Amounts: split(parts[i], r)}
	}
	return out
}
