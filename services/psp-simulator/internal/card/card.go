// Package card validates card details as a payment provider does before it
// issues a token, and generates valid test card numbers.
//
// The simulated outcome of a charge comes from fictitious test BINs: a brand
// prefix (400000 Visa, 510000 Mastercard, 370000 American Express) followed by
// two digits that choose the outcome. Any other valid number is approved.
package card

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Brand is a card network.
type Brand string

// Supported brands.
const (
	Visa       Brand = "visa"
	Mastercard Brand = "mastercard"
	Amex       Brand = "amex"
)

// Outcome is what the simulator does with a charge on the card.
type Outcome string

// Simulated outcomes.
const (
	Approve        Outcome = "approve"
	Decline        Outcome = "decline"         // refused at once: insufficient funds
	Slow           Outcome = "slow"            // approved after a delay, to exercise client timeouts
	Pending        Outcome = "pending"         // pending, approved later
	PendingDecline Outcome = "pending_decline" // pending, refused later
)

var (
	testPrefix = map[Brand]string{Visa: "400000", Mastercard: "510000", Amex: "370000"}
	outcomes   = map[Outcome]string{Approve: "00", Decline: "95", Slow: "10", Pending: "30", PendingDecline: "31"}
	// Accepted lengths per brand; the first one is what Generate produces.
	lengths   = map[Brand][]int{Visa: {16, 13, 19}, Mastercard: {16}, Amex: {15}}
	cvcLength = map[Brand]int{Visa: 3, Mastercard: 3, Amex: 4}
)

// maxValidityYears bounds how far in the future an expiry date may be.
const maxValidityYears = 20

// Card is what the provider keeps: the brand and last four digits, never the
// full number, the expiry or the CVC.
type Card struct {
	Brand   Brand
	Last4   string
	Outcome Outcome
}

// TestCard holds the details a browser would send to tokenize a card.
type TestCard struct {
	Number   string `json:"number"`
	ExpMonth int    `json:"exp_month"`
	ExpYear  int    `json:"exp_year"`
	CVC      string `json:"cvc"`
}

// Error is a validation failure with a machine-readable code.
type Error struct{ Code, Message string }

func (e *Error) Error() string { return e.Message }

// Parse validates the details and returns the card to keep.
func Parse(number string, expMonth, expYear int, cvc string, now time.Time) (Card, error) {
	digits := strings.NewReplacer(" ", "", "-", "").Replace(number)
	brand, ok := detectBrand(digits)
	switch {
	case !isDigits(digits):
		return Card{}, &Error{"incorrect_number", "the card number must contain only digits"}
	case !ok:
		return Card{}, &Error{"unsupported_brand", "only Visa, Mastercard and American Express are accepted"}
	case !slices.Contains(lengths[brand], len(digits)) || !luhnValid(digits):
		return Card{}, &Error{"incorrect_number", "the card number is incorrect"}
	case expMonth < 1 || expMonth > 12:
		return Card{}, &Error{"invalid_expiry_month", "the expiry month must be between 1 and 12"}
	case expYear > now.Year()+maxValidityYears:
		return Card{}, &Error{"invalid_expiry_year", fmt.Sprintf("the expiry year must be at most %d years ahead", maxValidityYears)}
	case !now.Before(time.Date(expYear, time.Month(expMonth)+1, 1, 0, 0, 0, 0, time.UTC)):
		return Card{}, &Error{"expired_card", "the card has expired"}
	case len(cvc) != cvcLength[brand] || !isDigits(cvc):
		return Card{}, &Error{"invalid_cvc", fmt.Sprintf("the security code must have %d digits", cvcLength[brand])}
	}
	return Card{Brand: brand, Last4: digits[len(digits)-4:], Outcome: outcomeOf(brand, digits)}, nil
}

// Generate returns a valid test card of the brand that triggers the outcome,
// expiring three years after now.
func Generate(b Brand, o Outcome, now time.Time) (TestCard, error) {
	prefix, ok := testPrefix[b]
	if !ok {
		return TestCard{}, fmt.Errorf("unknown brand %q", b)
	}
	code, ok := outcomes[o]
	if !ok {
		return TestCard{}, fmt.Errorf("unknown outcome %q", o)
	}
	var sb strings.Builder
	sb.WriteString(prefix + code)
	for sb.Len() < lengths[b][0]-1 {
		sb.WriteByte(byte('0' + rand.IntN(10)))
	}
	partial := sb.String()
	return TestCard{
		Number:   partial + strconv.Itoa(checkDigit(partial)),
		ExpMonth: 12,
		ExpYear:  now.Year() + 3,
		CVC:      strings.Repeat("1", cvcLength[b]),
	}, nil
}

func detectBrand(d string) (Brand, bool) {
	if len(d) < 4 {
		return "", false
	}
	two, _ := strconv.Atoi(d[:2])
	four, _ := strconv.Atoi(d[:4])
	switch {
	case d[0] == '4':
		return Visa, true
	case (two >= 51 && two <= 55) || (four >= 2221 && four <= 2720):
		return Mastercard, true
	case two == 34 || two == 37:
		return Amex, true
	}
	return "", false
}

func outcomeOf(b Brand, d string) Outcome {
	prefix := testPrefix[b]
	if strings.HasPrefix(d, prefix) {
		for o, code := range outcomes {
			if strings.HasPrefix(d[len(prefix):], code) {
				return o
			}
		}
	}
	return Approve
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// luhnValid reports whether the last digit is the Luhn check digit of the rest.
func luhnValid(d string) bool {
	return checkDigit(d[:len(d)-1]) == int(d[len(d)-1]-'0')
}

// checkDigit computes the Luhn check digit for a number without it.
func checkDigit(partial string) int {
	sum := 0
	for i := len(partial) - 1; i >= 0; i-- {
		n := int(partial[i] - '0')
		if (len(partial)-1-i)%2 == 0 {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
	}
	return (10 - sum%10) % 10
}
