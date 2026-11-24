package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thiagogenez/labflow/libs/go/config"
)

func TestEnv(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		read    func(*config.Env) any
		want    any
		wantErr string
	}{
		{"string default", nil, func(e *config.Env) any { return e.String("A", "x") }, "x", ""},
		{"string trimmed", map[string]string{"A": " y "}, func(e *config.Env) any { return e.String("A", "x") }, "y", ""},
		{"required missing", nil, func(e *config.Env) any { return e.Required("A") }, "", "A is required"},
		{"required blank", map[string]string{"A": "  "}, func(e *config.Env) any { return e.Required("A") }, "", "A is required"},
		{"duration default", nil, func(e *config.Env) any { return e.Duration("D", time.Second) }, time.Second, ""},
		{"duration parsed", map[string]string{"D": "3s"}, func(e *config.Env) any { return e.Duration("D", time.Second) }, 3 * time.Second, ""},
		{"duration invalid", map[string]string{"D": "soon"}, func(e *config.Env) any { return e.Duration("D", time.Second) }, time.Second, "D must be a positive duration"},
		{"duration negative", map[string]string{"D": "-1s"}, func(e *config.Env) any { return e.Duration("D", time.Second) }, time.Second, "D must be a positive duration"},
		{"url valid", map[string]string{"U": "http://psp:8080"}, func(e *config.Env) any { return e.URL("U").String() }, "http://psp:8080", ""},
		{"url relative", map[string]string{"U": "psp:8080"}, func(e *config.Env) any { return e.URL("U") == nil }, true, "U must be an absolute http or https URL"},
		{"url with credentials", map[string]string{"U": "http://user:s3cret@psp:8080"}, func(e *config.Env) any { return e.URL("U") == nil }, true, "U must not carry credentials"},
		{"check fails", nil, func(e *config.Env) any { e.Check(false, "keys must differ"); return nil }, nil, "keys must differ"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, k := range []string{"A", "D", "U"} {
				t.Setenv(k, tt.env[k])
			}
			e := config.FromEnviron()
			if got := tt.read(e); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
			err := e.Err()
			if tt.wantErr == "" && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Errorf("error %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestEnvReportsEveryProblem(t *testing.T) {
	t.Setenv("A", "")
	t.Setenv("B", "")
	t.Setenv("D", "x")
	e := config.FromEnviron()
	e.Required("A")
	e.Required("B")
	e.Duration("D", time.Second)
	err := e.Err()
	for _, want := range []string{"A is required", "B is required", "D must be a positive duration"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error %v does not mention %q", err, want)
		}
	}
}

func TestSecret(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	good := write("good", " s3cret value \n")
	tests := []struct {
		name    string
		value   *string // nil leaves S unset
		file    string
		want    string
		wantErr string
	}{
		{name: "from the file, less its newline", file: good, want: " s3cret value "},
		{name: "a carriage return is the secret's", file: write("crlf", "s3cret\r\n"), want: "s3cret\r"},
		{name: "value set directly", value: ptr("s3cret value"), file: good, wantErr: "S must not be set; put the secret in a file and set S_FILE"},
		{name: "value set to nothing", value: ptr(""), file: good, wantErr: "S must not be set"},
		{name: "no file named", wantErr: "S_FILE is required"},
		{name: "missing file", file: filepath.Join(dir, "absent"), wantErr: "S_FILE"},
		{name: "empty file", file: write("empty", "\n"), wantErr: "is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("S", "")
			if tt.value == nil {
				_ = os.Unsetenv("S") // t.Setenv restores it afterwards
			} else {
				t.Setenv("S", *tt.value)
			}
			t.Setenv("S_FILE", tt.file)
			e := config.FromEnviron()
			if got := e.Secret("S"); got != tt.want {
				t.Errorf("Secret = %q, want %q", got, tt.want)
			}
			err := e.Err()
			if tt.wantErr == "" && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Errorf("error %v, want %q", err, tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "s3cret") {
				t.Errorf("error repeats the secret: %v", err)
			}
		})
	}
}

func ptr(s string) *string { return &s }
