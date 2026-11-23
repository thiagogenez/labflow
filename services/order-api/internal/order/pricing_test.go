package order

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSplitRoundsTheNetHalfUp(t *testing.T) {
	tests := []struct {
		gross   int64
		rateBps int
		want    Amounts
	}{
		{8900, 1900, Amounts{Net: 7479, Tax: 1421, Gross: 8900}},                                   // 7478.99 rounds up
		{3750, 1900, Amounts{Net: 3151, Tax: 599, Gross: 3750}},                                    // 3151.26 rounds down
		{590, 1900, Amounts{Net: 496, Tax: 94, Gross: 590}},                                        // 495.80 rounds up
		{119, 1900, Amounts{Net: 100, Tax: 19, Gross: 119}},                                        // exact
		{1, 1900, Amounts{Net: 1, Tax: 0, Gross: 1}},                                               // 0.84 rounds up
		{500, 0, Amounts{Net: 500, Tax: 0, Gross: 500}},                                            // zero-rated
		{maxAmount, 1900, Amounts{Net: 7569075003984026, Tax: 1438124250756965, Gross: maxAmount}}, // no int64 overflow
	}
	for _, tt := range tests {
		if got := split(tt.gross, tt.rateBps); got != tt.want {
			t.Errorf("split(%d, %d) = %+v, want %+v", tt.gross, tt.rateBps, got, tt.want)
		}
	}
}

// The contract's example order: a pipette, three notebooks and a book at the
// reduced rate, shipped standard to Germany; its shipping splits across both
// rates.
func TestPriceMatchesTheContractExample(t *testing.T) {
	var example struct {
		Data struct {
			Lines    []Line   `json:"lines"`
			Shipping Shipping `json:"shipping"`
			Totals   Amounts  `json:"totals"`
		} `json:"data"`
	}
	b, err := os.ReadFile("../../../../contracts/events/examples/labflow.order.placed.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &example); err != nil {
		t.Fatal(err)
	}
	products := map[string]Product{}
	var items []Item
	for _, l := range example.Data.Lines {
		products[l.SKU] = Product{SKU: l.SKU, Name: l.Name, UnitPriceGross: l.UnitPriceGross, TaxRateBps: l.TaxRateBps}
		items = append(items, Item{SKU: l.SKU, Quantity: l.Quantity})
	}
	p, err := price(items, products, "DE", SpeedStandard, testRates(t))
	if err != nil {
		t.Fatal(err)
	}
	want := example.Data.Shipping
	want.RateSource = RateSourceFlat // the example quoted the same 5.90 the flat rate charges
	if !reflect.DeepEqual(p.Lines, example.Data.Lines) || !reflect.DeepEqual(p.Shipping, want) || p.Totals != example.Data.Totals {
		t.Fatalf("priced %+v %+v %+v,\nwant %+v %+v %+v", p.Lines, p.Shipping, p.Totals, example.Data.Lines, want, example.Data.Totals)
	}
}

func TestSplitShippingFollowsTheGoodsRates(t *testing.T) {
	tests := []struct {
		name  string
		gross int64
		goods map[int]int64
		want  []TaxedAmounts
	}{
		{"one rate", 590, map[int]int64{1900: 12650},
			[]TaxedAmounts{{1900, Amounts{Net: 496, Tax: 94, Gross: 590}}}},
		{"two rates, a leftover cent to the larger remainder", 590, map[int]int64{1900: 12650, 700: 4900},
			[]TaxedAmounts{{1900, Amounts{Net: 357, Tax: 68, Gross: 425}}, {700, Amounts{Net: 154, Tax: 11, Gross: 165}}}},
		{"an even split", 1000, map[int]int64{1900: 500, 700: 500},
			[]TaxedAmounts{{1900, Amounts{Net: 420, Tax: 80, Gross: 500}}, {700, Amounts{Net: 467, Tax: 33, Gross: 500}}}},
		{"a tie goes to the higher rate; the other keeps a zero part", 1, map[int]int64{1900: 500, 700: 500},
			[]TaxedAmounts{{1900, Amounts{Net: 1, Tax: 0, Gross: 1}}, {700, Amounts{}}}},
	}
	for _, tt := range tests {
		if got := splitShipping(tt.gross, tt.goods); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: %+v, want %+v", tt.name, got, tt.want)
		}
	}
}

func TestPriceRejectsUnknownSKUsAndOverflow(t *testing.T) {
	products := map[string]Product{"BIG": {SKU: "BIG", Name: "Big", UnitPriceGross: maxAmount / 50, TaxRateBps: 1900}}
	if _, err := price([]Item{{"NOPE", 1}}, products, "DE", SpeedStandard, testRates(t)); err == nil {
		t.Fatal("an unknown SKU was priced")
	}
	if _, err := price([]Item{{"BIG", 99}}, products, "DE", SpeedStandard, testRates(t)); err == nil {
		t.Fatal("a line beyond the largest amount was priced")
	}
}

func TestZonesCoverTheEU(t *testing.T) {
	if len(zones) != 27 {
		t.Fatalf("%d countries, want the 27 EU members", len(zones))
	}
}

func TestLoadFlatRatesNeedsEveryZoneAndSpeed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rates.json")
	if err := os.WriteFile(path, []byte(`{"DE":{"standard":590,"express":1290}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFlatRates(path); err == nil {
		t.Fatal("rates without the EU zones were accepted")
	}
}

// testRates loads the repository's flat-rate file, so the tests also check it.
func testRates(t *testing.T) FlatRates {
	t.Helper()
	rates, err := LoadFlatRates("../../flat-rates.json")
	if err != nil {
		t.Fatal(err)
	}
	return rates
}
