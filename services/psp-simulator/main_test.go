package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// configure sets a valid configuration whose secret key comes from a file
// holding secretKey.
func configure(t *testing.T, publishableKey, secretKey string) {
	t.Helper()
	dir := t.TempDir()
	for name, value := range map[string]string{"db": "pw", "sk": secretKey} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("DATABASE_URL", "postgres://psp_app@unused/psp")
	t.Setenv("DATABASE_PASSWORD_FILE", filepath.Join(dir, "db"))
	t.Setenv("PUBLISHABLE_KEY", publishableKey)
	t.Setenv("SECRET_KEY_FILE", filepath.Join(dir, "sk"))
}

func TestServeRejectsEqualKeys(t *testing.T) {
	configure(t, "same", "same")
	err := serve(t.Context())
	if err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("serve with equal keys: %v, want a configuration error", err)
	}
}

func TestServeRejectsSlowDelayBeyondChargeLimit(t *testing.T) {
	configure(t, "pk", "sk")
	t.Setenv("SLOW_DELAY", "11s")
	err := serve(t.Context())
	if err == nil || !strings.Contains(err.Error(), "SLOW_DELAY") {
		t.Fatalf("serve with SLOW_DELAY=11s: %v, want a configuration error", err)
	}
}
