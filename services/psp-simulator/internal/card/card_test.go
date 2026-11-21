package card

import (
	"errors"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

func TestParse(t *testing.T) {
	tests := []struct {
		name, number, cvc string
		month, year       int
		wantBrand         Brand
		wantOutcome       Outcome
		wantCode          string
	}{
		{"visa approved", "4111 1111 1111 1111", "123", 12, 2029, Visa, Approve, ""},
		{"visa decline bin", "4000 0095 0000 0008", "123", 12, 2029, Visa, Decline, ""},
		{"mastercard 2-series", "2221 0000 0000 0009", "123", 1, 2027, Mastercard, Approve, ""},
		{"amex pending", "3700 0030 0000 009", "1234", 6, 2028, Amex, Pending, ""},
		{"luhn failure", "4111 1111 1111 1112", "123", 12, 2029, "", "", "incorrect_number"},
		{"wrong length", "4111 1111 1111", "123", 12, 2029, "", "", "incorrect_number"},
		{"letters", "4111-1111-1111-111a", "123", 12, 2029, "", "", "incorrect_number"},
		{"unsupported brand", "6011 1111 1111 1117", "123", 12, 2029, "", "", "unsupported_brand"},
		{"bad month", "4111 1111 1111 1111", "123", 13, 2029, "", "", "invalid_expiry_month"},
		{"expired last month", "4111 1111 1111 1111", "123", 10, 2026, "", "", "expired_card"},
		{"valid through this month", "4111 1111 1111 1111", "123", 11, 2026, Visa, Approve, ""},
		{"amex needs 4-digit cvc", "3700 0030 0000 009", "123", 6, 2028, "", "", "invalid_cvc"},
		{"19-digit visa", "4000 0000 0000 0000 006", "123", 12, 2029, Visa, Approve, ""},
		{"expiry too far ahead", "4111 1111 1111 1111", "123", 12, 40000, "", "", "invalid_expiry_year"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Parse(tt.number, tt.month, tt.year, tt.cvc, now)
			if tt.wantCode != "" {
				var ce *Error
				if !errors.As(err, &ce) || ce.Code != tt.wantCode {
					t.Fatalf("error %v, want code %q", err, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.Brand != tt.wantBrand || c.Outcome != tt.wantOutcome || len(c.Last4) != 4 {
				t.Fatalf("got %+v, want brand %s outcome %s", c, tt.wantBrand, tt.wantOutcome)
			}
		})
	}
}

func TestGenerateRoundTrips(t *testing.T) {
	for b := range testPrefix {
		for o := range outcomes {
			for range 20 {
				tc, err := Generate(b, o, now)
				if err != nil {
					t.Fatal(err)
				}
				c, err := Parse(tc.Number, tc.ExpMonth, tc.ExpYear, tc.CVC, now)
				if err != nil || c.Brand != b || c.Outcome != o {
					t.Fatalf("Generate(%s, %s) = %+v, parsed as %+v, %v", b, o, tc, c, err)
				}
			}
		}
	}
}
