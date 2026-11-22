package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thiagogenez/labflow/libs/go/api"
)

func TestDecodeJSON(t *testing.T) {
	tests := []struct {
		name, body, wantErr string
	}{
		{"valid", `{"a":1}`, ""},
		{"unknown field", `{"a":1,"b":2}`, "unknown field"},
		{"trailing data", `{"a":1}{"a":2}`, "unexpected data"},
		{"not json", `a=1`, "invalid JSON"},
		{"too large", `{"a":` + strings.Repeat("1", api.MaxBodyBytes) + `}`, "exceeds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var v struct{ A int }
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.body))
			err := api.DecodeJSON(httptest.NewRecorder(), r, &v)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("error %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestWriteProblem(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/payments", nil)
	api.WriteProblem(w, r, http.StatusPaymentRequired, "declined", map[string]any{"code": "card_declined"})

	if w.Code != http.StatusPaymentRequired || w.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("status %d, content type %q", w.Code, w.Header().Get("Content-Type"))
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"type": "about:blank", "title": "Payment Required", "status": float64(402),
		"detail": "declined", "code": "card_declined"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if _, ok := got["instance"]; ok {
		t.Error("instance must be omitted: a route path does not identify one occurrence")
	}
}

func TestPrintable(t *testing.T) {
	for in, want := range map[string]bool{"tok_ABC": true, "Ada Lovelace": true, "Müller": true,
		"": true, "a\x00b": false, "line\nbreak": false, "tab\t": false, "\xff": false} {
		if got := api.Printable(in); got != want {
			t.Errorf("Printable(%q) = %v, want %v", in, got, want)
		}
	}
}
