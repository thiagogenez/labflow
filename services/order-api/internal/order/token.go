package order

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
)

// minTokenKeyBytes is the hash output length; a shorter key weakens
// HMAC-SHA-256 (RFC 2104, section 3).
const minTokenKeyBytes = 32

// Tokens issues and checks order access tokens: the HMAC-SHA-256 of the order
// ID, so a token is re-derived instead of stored (D56).
type Tokens struct{ key []byte }

// NewTokens returns a token issuer for a key of at least 32 bytes.
func NewTokens(key []byte) (*Tokens, error) {
	if len(key) < minTokenKeyBytes {
		return nil, errors.New("the order token key must be at least 32 bytes")
	}
	return &Tokens{key: key}, nil
}

// Issue returns the order's access token.
func (t *Tokens) Issue(orderID string) string {
	return base64.RawURLEncoding.EncodeToString(t.mac(orderID))
}

// Valid reports, in constant time, whether token belongs to the order.
func (t *Tokens) Valid(orderID, token string) bool {
	got, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil && hmac.Equal(got, t.mac(orderID))
}

func (t *Tokens) mac(orderID string) []byte {
	m := hmac.New(sha256.New, t.key)
	m.Write([]byte(orderID))
	return m.Sum(nil)
}
