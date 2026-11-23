package order

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// LoadCatalogue reads and checks a catalogue file: a JSON array of products
// with unique SKUs (D68).
func LoadCatalogue(path string) ([]Product, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var products []Product
	if err := dec.Decode(&products); err != nil {
		return nil, fmt.Errorf("catalogue %s: %w", path, err)
	}
	// Anything after the array would otherwise be ignored, and the import
	// would retire every product the cut-off array misses.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("catalogue %s: data after the product array", path)
	}
	if len(products) == 0 {
		return nil, fmt.Errorf("catalogue %s: no products; an empty file would retire the whole catalogue", path)
	}
	seen := make(map[string]bool, len(products))
	for _, p := range products {
		switch {
		case !validSKU(p.SKU):
			return nil, fmt.Errorf("catalogue %s: invalid SKU %q", path, p.SKU)
		case seen[p.SKU]:
			return nil, fmt.Errorf("catalogue %s: duplicate SKU %s", path, p.SKU)
		case !text(p.Name, 1, 200):
			return nil, fmt.Errorf("catalogue %s: %s needs a name of 1 to 200 characters", path, p.SKU)
		case p.UnitPriceGross <= 0 || p.UnitPriceGross > maxAmount:
			return nil, fmt.Errorf("catalogue %s: %s needs a positive price in minor units", path, p.SKU)
		case !vatRates[p.TaxRateBps]:
			return nil, fmt.Errorf("catalogue %s: %s needs tax_rate_bps 1900 or 700, German standard or reduced VAT", path, p.SKU)
		case p.WeightGrams <= 0:
			return nil, fmt.Errorf("catalogue %s: %s needs a positive weight_grams", path, p.SKU)
		}
		seen[p.SKU] = true
	}
	return products, nil
}

// Import makes the catalogue file the products on sale.
func Import(ctx context.Context, store *Store, path string) (ImportResult, error) {
	products, err := LoadCatalogue(path)
	if err != nil {
		return ImportResult{}, err
	}
	return store.ImportProducts(ctx, products)
}
