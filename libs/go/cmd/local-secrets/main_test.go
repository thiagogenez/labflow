package main

import (
	"crypto/tls"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// newRoot copies the real definitions template, with an extra user whose name
// has a digit, into a scratch repository root.
func newRoot(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	template, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "compose", "rabbitmq", "definitions.template.json"))
	if err != nil {
		t.Fatal(err)
	}
	var d map[string]any
	if err := json.Unmarshal(template, &d); err != nil {
		t.Fatal(err)
	}
	d["users"] = append(d["users"].([]any), map[string]any{"name": "worker2", "password": "@worker2@", "tags": []any{}})
	root := t.TempDir()
	writeTemplate(t, root, d)
	return root
}

func writeTemplate(t *testing.T, root string, d any) {
	t.Helper()
	out, _ := json.Marshal(d)
	path := filepath.Join(root, "compose", "rabbitmq", "definitions.template.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

// snapshot reads every file in .secrets.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(root, ".secrets", "*"))
	got := map[string]string{}
	for _, f := range files {
		if filepath.Base(f) == ".lock" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			got[filepath.Base(f)] = "<unreadable>"
			continue
		}
		got[filepath.Base(f)] = string(b)
	}
	return got
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestGenerate(t *testing.T) {
	root := newRoot(t)
	if err := run(root, false); err != nil {
		t.Fatal(err)
	}
	first := snapshot(t, root)
	if mode(t, filepath.Join(root, ".secrets")) != 0o700 || mode(t, filepath.Join(root, ".secrets", "orders-app")) != 0o644 {
		t.Error(".secrets must be 0700 and its files 0644")
	}
	var defs struct {
		Users []struct{ Name, Password string }
	}
	if err := json.Unmarshal([]byte(first["rabbitmq-definitions.json"]), &defs); err != nil {
		t.Fatal(err)
	}
	for _, u := range defs.Users {
		if u.Password != first["rabbitmq-"+u.Name] {
			t.Errorf("user %s did not get its own password", u.Name)
		}
	}
	if len(first["orders-app"]) < 32 || first["generation"] == "" {
		t.Errorf("missing or short secrets: %d files", len(first))
	}

	// Run again: nothing changes, and a mode containers cannot read is fixed.
	if err := os.Chmod(filepath.Join(root, ".secrets", "orders-app"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(root, false); err != nil {
		t.Fatal(err)
	}
	if again := snapshot(t, root); !equal(again, first) {
		t.Error("a second run changed the secrets")
	}
	if mode(t, filepath.Join(root, ".secrets", "orders-app")) != 0o644 {
		t.Error("a kept secret kept a mode containers cannot read")
	}

	// -rotate replaces every value and the generation.
	if err := run(root, true); err != nil {
		t.Fatal(err)
	}
	rotated := snapshot(t, root)
	for name, old := range first {
		if rotated[name] == old {
			t.Errorf("-rotate kept %s", name)
		}
	}
}

// A template change renders again under a new generation, so `make up`
// recreates RabbitMQ with it.
func TestTemplateChangeIsApplied(t *testing.T) {
	root := newRoot(t)
	if err := run(root, false); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, root)
	writeTemplate(t, root, map[string]any{"users": []any{
		map[string]any{"name": "admin", "password": "@admin@"},
		map[string]any{"name": "auditor", "password": "@auditor@"},
	}})
	if err := run(root, false); err != nil {
		t.Fatal(err)
	}
	after := snapshot(t, root)
	var defs struct{ Users []struct{ Name string } }
	if err := json.Unmarshal([]byte(after["rabbitmq-definitions.json"]), &defs); err != nil {
		t.Fatal(err)
	}
	if after["generation"] == before["generation"] || len(defs.Users) != 2 || defs.Users[1].Name != "auditor" {
		t.Error("a template change was not rendered under a new generation")
	}
	if after["rabbitmq-admin"] != before["rabbitmq-admin"] {
		t.Error("a template change replaced an existing password")
	}
}

// A refused template or password leaves every previous file as it was.
func TestFailureKeepsThePreviousSet(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "private")
	for _, tc := range []struct {
		name    string
		rotate  bool
		breakIt func(t *testing.T, root string)
	}{
		{"an unreadable secret", false, func(t *testing.T, root string) {
			if err := os.Chmod(filepath.Join(root, ".secrets", "order-token-key"), 0o000); err != nil {
				t.Fatal(err)
			}
		}},
		{"a link to a file outside", false, func(t *testing.T, root string) {
			if err := os.WriteFile(outside, []byte(strings.Repeat("p", 40)), 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, ".secrets", "orders-app")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, path); err != nil {
				t.Fatal(err)
			}
		}},
		{"a committed password", true, func(t *testing.T, root string) {
			writeTemplate(t, root, map[string]any{"users": []any{map[string]any{"name": "admin", "password": "hunter2"}}})
		}},
		{"a missing secret beside a password JSON would need to escape", false, func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, ".secrets", "orders-app")); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, ".secrets", "rabbitmq-admin"), []byte(`a"b`+strings.Repeat("c", 40)), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"an unreadable TLS key", false, func(t *testing.T, root string) {
			if err := os.Chmod(filepath.Join(root, ".secrets", "mailpit-tls.key"), 0o000); err != nil {
				t.Fatal(err)
			}
		}},
		{"a Mailpit password that would add a user", false, func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, ".secrets", "mailpit-ui"), []byte(strings.Repeat("p", 40)+"\nroot:x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"a password JSON would need to escape", false, func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, ".secrets", "rabbitmq-admin"), []byte(`a"b`+strings.Repeat("c", 40)), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newRoot(t)
			if err := run(root, false); err != nil {
				t.Fatal(err)
			}
			tc.breakIt(t, root)
			before := snapshot(t, root)
			if err := run(root, tc.rotate); err == nil {
				t.Fatal("run succeeded")
			}
			if after := snapshot(t, root); !equal(after, before) {
				t.Error("a failed run changed the secrets")
			}
			if info, err := os.Stat(outside); err == nil && info.Mode().Perm() != 0o600 {
				t.Error("a file outside .secrets changed mode")
			}
		})
	}
}

func equal(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// A secret replaced by hand, or a deleted marker, gets a new generation, so
// `make up` recreates the services that read it.
func TestManualChangeGetsANewGeneration(t *testing.T) {
	root := newRoot(t)
	if err := run(root, false); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, root)
	if err := os.WriteFile(filepath.Join(root, ".secrets", "order-token-key"), []byte(tokenKey()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(root, false); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(t, root); after["generation"] == before["generation"] {
		t.Error("a hand-replaced secret kept the generation")
	}
	if err := os.Remove(filepath.Join(root, ".secrets", "generation")); err != nil {
		t.Fatal(err)
	}
	if err := run(root, false); err != nil {
		t.Fatal(err)
	}
	if snapshot(t, root)["generation"] == "" {
		t.Error("a deleted generation was not written again")
	}
}

// Mailpit gets a password file per service from the passwords, and a
// certificate for its name that is kept until 30 days before it expires.
func TestMailpitFiles(t *testing.T) {
	root := newRoot(t)
	if err := run(root, false); err != nil {
		t.Fatal(err)
	}
	got := snapshot(t, root)
	if got["mailpit-smtp-users"] != "email-worker:"+got["mailpit-email-worker"]+"\n" ||
		got["mailpit-ui-users"] != "admin:"+got["mailpit-ui"]+"\n" {
		t.Error("a Mailpit password file does not hold its user's password")
	}
	pair, err := tls.X509KeyPair([]byte(got["mailpit-tls.crt"]), []byte(got["mailpit-tls.key"]))
	if err != nil {
		t.Fatal(err)
	}
	if left := time.Until(pair.Leaf.NotAfter); pair.Leaf.VerifyHostname("mailpit") != nil || left < 364*24*time.Hour {
		t.Errorf("certificate for %v, %s left; want mailpit, a year", pair.Leaf.DNSNames, left)
	}

	if err := run(root, false); err != nil {
		t.Fatal(err)
	}
	if snapshot(t, root)["mailpit-tls.crt"] != got["mailpit-tls.crt"] {
		t.Error("a second run replaced a certificate with a year left")
	}

	// A certificate with 29 days left is replaced, with its key.
	key, cert, err := newTLSPair(time.Now().Add(29*24*time.Hour - tlsLifetime))
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{"mailpit-tls.key": key, "mailpit-tls.crt": cert} {
		if err := os.WriteFile(filepath.Join(root, ".secrets", name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := run(root, false); err != nil {
		t.Fatal(err)
	}
	renewed := snapshot(t, root)
	if renewed["mailpit-tls.crt"] == string(cert) || renewed["mailpit-tls.key"] == string(key) {
		t.Error("a certificate with 29 days left was kept")
	}
	if renewed["generation"] == got["generation"] {
		t.Error("a renewed certificate kept the generation, so Compose would not recreate Mailpit")
	}
}
