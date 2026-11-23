package order

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestTokens(t *testing.T) {
	if _, err := NewTokens(make([]byte, 31)); err == nil {
		t.Fatal("a 31-byte key was accepted")
	}
	tokens, err := NewTokens([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	const id = "01a1f874-3be8-7fa4-95f5-868d7d69c709"
	token := tokens.Issue(id)
	tests := []struct {
		name, id, token string
		want            bool
	}{
		{"own token", id, token, true},
		{"another order", "01a1f874-3be8-7fa4-95f5-868d7d69c70a", token, false},
		{"altered token", id, token[:len(token)-1] + "A", false},
		{"not base64", id, "!!", false},
		{"empty", id, "", false},
	}
	for _, tt := range tests {
		if got := tokens.Valid(tt.id, tt.token); got != tt.want {
			t.Errorf("%s: Valid = %v, want %v", tt.name, got, tt.want)
		}
	}
	other, _ := NewTokens([]byte(strings.Repeat("x", 32)))
	if other.Valid(id, token) {
		t.Fatal("a token survived a key rotation")
	}
}

// The shared vector pins the encoding email-worker must reproduce (D56).
func TestTokenMatchesTheSharedVector(t *testing.T) {
	var v struct {
		KeyBase64 string `json:"key_base64"`
		OrderID   string `json:"order_id"`
		Expected  string `json:"expected"`
	}
	b, err := os.ReadFile("../../../../contracts/order-access-token.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	key, err := base64.StdEncoding.DecodeString(v.KeyBase64)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := NewTokens(key)
	if err != nil {
		t.Fatal(err)
	}
	if got := tokens.Issue(v.OrderID); got != v.Expected {
		t.Fatalf("token %s, want the shared vector's %s", got, v.Expected)
	}
}
