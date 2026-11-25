package main

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/thiagogenez/labflow/libs/go/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSetsRefuseAnythingButSetDirectories(t *testing.T) {
	for _, tc := range []struct {
		name    string
		files   []string
		wantErr string
	}{
		{"empty", nil, "holds no migration set"},
		{"a file beside the sets", []string{"email/00001_a.sql", "00002_b.sql"}, "00002_b.sql: want only directories"},
		{"a set name with a hyphen", []string{"email-worker/00001_a.sql"}, "email-worker: want only directories"},
		{"a set name in capitals", []string{"Email/00001_a.sql"}, "Email: want only directories"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for _, f := range tc.files {
				write(t, filepath.Join(root, f), "-- +goose Up\nSELECT 1;\n")
			}
			if _, err := sets(root); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("sets: %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestRunNeedsItsService(t *testing.T) {
	if err := run(t.Context(), "", t.TempDir()); err == nil || err.Error() != "SERVICE is required" {
		t.Fatalf("run without SERVICE: %v, want SERVICE is required", err)
	}
}

// TestRunAppliesEverySetInNameOrder runs the command's whole path: the
// connection without its password, the password from its file, and two sets,
// the second of which needs the first.
func TestRunAppliesEverySetInNameOrder(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a_first", "00001_create.sql"), "-- +goose Up\nCREATE TABLE first (id int PRIMARY KEY);\n")
	write(t, filepath.Join(root, "b_second", "00001_create.sql"), "-- +goose Up\nCREATE TABLE second (id int PRIMARY KEY REFERENCES first);\n")

	dsn := pgtest.NewDSN(t)
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := u.User.Password()
	u.User = url.User(u.User.Username())
	passwordFile := filepath.Join(t.TempDir(), "password")
	write(t, passwordFile, password)
	t.Setenv("MIGRATIONS_DATABASE_URL", u.String())
	t.Setenv("MIGRATIONS_DATABASE_PASSWORD_FILE", passwordFile)

	if err := run(t.Context(), "test", root); err != nil {
		t.Fatalf("run: %v", err)
	}
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(t.Context()) }()
	var versions int
	err = conn.QueryRow(t.Context(), "SELECT (SELECT max(version_id) FROM goose.a_first) + (SELECT max(version_id) FROM goose.b_second)").Scan(&versions)
	if err != nil || versions != 2 {
		t.Fatalf("version tables: %d, %v; want version 1 in goose.a_first and goose.b_second", versions, err)
	}
}
