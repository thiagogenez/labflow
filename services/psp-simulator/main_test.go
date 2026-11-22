package main

import (
	"strings"
	"testing"
)

func TestServeRejectsEqualKeys(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://unused")
	t.Setenv("PUBLISHABLE_KEY", "same")
	t.Setenv("SECRET_KEY", "same")
	err := serve(t.Context())
	if err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("serve with equal keys: %v, want a configuration error", err)
	}
}

func TestServeRejectsSlowDelayBeyondChargeLimit(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://unused")
	t.Setenv("PUBLISHABLE_KEY", "pk")
	t.Setenv("SECRET_KEY", "sk")
	t.Setenv("SLOW_DELAY", "11s")
	err := serve(t.Context())
	if err == nil || !strings.Contains(err.Error(), "SLOW_DELAY") {
		t.Fatalf("serve with SLOW_DELAY=11s: %v, want a configuration error", err)
	}
}
