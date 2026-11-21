// Package api holds the JSON, input and RFC 9457 problem-details helpers
// shared by every HTTP API.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"unicode"
	"unicode/utf8"

	"go.opentelemetry.io/otel/trace"
)

// MaxBodyBytes bounds every JSON request body.
const MaxBodyBytes = 1 << 20

// DecodeJSON reads one JSON object into v, rejecting unknown fields,
// trailing data and bodies larger than MaxBodyBytes. The returned error is
// safe to show to the client.
func DecodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return fmt.Errorf("request body exceeds %d bytes", MaxBodyBytes)
		}
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("invalid JSON body: unexpected data after the object")
	}
	return nil
}

// Printable reports whether s is valid UTF-8 without control characters, the
// rule for free text that reaches storage, logs or emails.
func Printable(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, c := range s {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}

// WriteJSON writes v with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the status is already sent; a failed write only means the client left
}

// WriteProblem writes an RFC 9457 problem of type about:blank with the status
// text as title. ext adds members such as a machine-readable code; the
// request's trace ID is added so a client report leads to the logs.
func WriteProblem(w http.ResponseWriter, r *http.Request, status int, detail string, ext map[string]any) {
	p := map[string]any{"type": "about:blank", "title": http.StatusText(status), "status": status}
	if detail != "" {
		p["detail"] = detail
	}
	for k, v := range ext {
		p[k] = v
	}
	if sc := trace.SpanContextFromContext(r.Context()); sc.IsValid() {
		p["trace_id"] = sc.TraceID().String()
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(p) // the status is already sent; a failed write only means the client left
}

// Error writes a problem with only a status and a detail.
func Error(w http.ResponseWriter, r *http.Request, status int, detail string) {
	WriteProblem(w, r, status, detail, nil)
}
