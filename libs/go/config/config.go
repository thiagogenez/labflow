// Package config reads service configuration from environment variables.
//
// Every getter records problems instead of failing on the first one, so a
// service reports all invalid settings at startup and exits once.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// Env collects configuration values and the problems found while reading them.
type Env struct{ errs []error }

// FromEnviron reads from the process environment.
func FromEnviron() *Env { return &Env{} }

func value(name string) (string, bool) {
	v := strings.TrimSpace(os.Getenv(name))
	return v, v != ""
}

// String returns the variable, or def when it is unset or empty.
func (e *Env) String(name, def string) string {
	if v, ok := value(name); ok {
		return v
	}
	return def
}

// Required returns the variable and records an error when it is unset or empty.
func (e *Env) Required(name string) string {
	v, ok := value(name)
	if !ok {
		e.errs = append(e.errs, fmt.Errorf("%s is required", name))
	}
	return v
}

// Duration parses a positive Go duration such as "3s", or returns def when unset.
func (e *Env) Duration(name string, def time.Duration) time.Duration {
	v, ok := value(name)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		e.errs = append(e.errs, fmt.Errorf("%s must be a positive duration, got %q", name, v))
		return def
	}
	return d
}

// URL parses a required absolute http or https URL.
func (e *Env) URL(name string) *url.URL {
	v := e.Required(name)
	if v == "" {
		return nil
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		e.errs = append(e.errs, fmt.Errorf("%s must be an absolute http or https URL, got %q", name, v))
		return nil
	}
	return u
}

// Check records err when a cross-field rule fails, such as two keys that
// must differ.
func (e *Env) Check(ok bool, msg string) {
	if !ok {
		e.errs = append(e.errs, errors.New(msg))
	}
}

// Err returns every problem found so far, or nil.
func (e *Env) Err() error { return errors.Join(e.errs...) }
