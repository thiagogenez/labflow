// Package idempotency implements the Idempotency-Key HTTP header (IETF
// httpapi draft) on top of PostgreSQL.
//
// The first request with a key runs the handler and stores its response; a
// retry with the same key and body gets the stored response back. A retry
// while the first request still runs gets 409, the same key with a different
// body gets 422 and a request without a valid key gets 400.
//
// The handler's own writes and the stored response are not one transaction:
// if the process dies between them, the key's lease expires and a retry runs
// the handler again. Handlers must therefore also be idempotent on their
// natural key (an order ID, or the key itself via KeyFrom); this middleware
// adds the HTTP semantics on top.
package idempotency

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thiagogenez/labflow/libs/go/api"
	"github.com/thiagogenez/labflow/libs/go/postgres"
)

const (
	// Header is the request header that carries the key.
	Header = "Idempotency-Key"
	// retention is how long a key is kept (D57).
	retention = 24 * time.Hour
	// writeTimeout bounds storing an outcome after the client has gone.
	writeTimeout = 5 * time.Second
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migration creates the key table; services apply it next to their own.
var Migration = postgres.Migration{FS: mustSub(migrationFS, "migrations"), Name: "idempotency_version"}

// Store keeps keys and responses in PostgreSQL.
type Store struct {
	pool *pgxpool.Pool
	log  *slog.Logger
	// Lease is how long a running request holds its key; it must exceed the
	// longest request, after which a retry may take the key over.
	Lease time.Duration
}

// NewStore returns a store with a one-minute lease.
func NewStore(pool *pgxpool.Pool, log *slog.Logger) *Store {
	return &Store{pool: pool, log: log, Lease: time.Minute}
}

type keyContext struct{}

// claimed is what Middleware hands the handler about the request it claimed.
type claimed struct {
	key         string
	fingerprint []byte
}

// KeyFrom returns the request's decoded idempotency key inside a handler
// wrapped by Middleware.
func KeyFrom(ctx context.Context) string {
	c, _ := ctx.Value(keyContext{}).(claimed)
	return c.key
}

// FingerprintFrom returns the fingerprint Middleware compares requests by, so
// a handler that keeps its own record of the key judges "the same request"
// exactly as Middleware does.
func FingerprintFrom(ctx context.Context) []byte {
	c, _ := ctx.Value(keyContext{}).(claimed)
	return c.fingerprint
}

// Quote encodes a key as a Structured Field String, the form the draft
// requires clients to send.
func Quote(key string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(key) + `"`
}

// Middleware enforces the header on the wrapped handler.
func (s *Store) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := parseKey(r.Header.Values(Header))
		if !ok {
			api.Error(w, r, http.StatusBadRequest,
				`send exactly one Idempotency-Key header: a quoted string such as "f81d4fae", at most 255 characters`)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, api.MaxBodyBytes))
		if err != nil {
			api.Error(w, r, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body exceeds %d bytes", api.MaxBodyBytes))
			return
		}
		fp := fingerprint(r.Method, r.URL.Path, body)

		owner := rand.Text()
		stored, err := s.claim(r.Context(), key, fp, owner)
		switch {
		case errors.Is(err, errMismatch):
			api.Error(w, r, http.StatusUnprocessableEntity, "this Idempotency-Key was used with a different request")
			return
		case errors.Is(err, errInFlight):
			w.Header().Set("Retry-After", "1")
			api.Error(w, r, http.StatusConflict, "a request with this Idempotency-Key is still being processed")
			return
		case err != nil:
			s.log.ErrorContext(r.Context(), "idempotency claim failed", "error", err)
			api.Error(w, r, http.StatusServiceUnavailable, "cannot process the request right now")
			return
		case stored != nil:
			w.Header().Set("Idempotent-Replayed", "true")
			stored.write(w)
			return
		}

		r = r.WithContext(context.WithValue(r.Context(), keyContext{}, claimed{key: key, fingerprint: fp}))
		r.Body = io.NopCloser(bytes.NewReader(body))
		rec := &recorder{header: http.Header{}, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		resp := &response{status: rec.status, contentType: rec.header.Get("Content-Type"), body: rec.body.Bytes()}

		// The outcome is recorded even if the client has gone away.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), writeTimeout)
		defer cancel()
		if err := s.finish(ctx, key, owner, resp); err != nil {
			s.log.ErrorContext(r.Context(), "idempotency outcome not stored; a retry will run the handler again", "error", err)
		}
		for k, v := range rec.header {
			w.Header()[k] = v
		}
		resp.write(w)
	})
}

var (
	errMismatch  = errors.New("key used with a different request")
	errInFlight  = errors.New("key in flight")
	errLeaseLost = errors.New("the key's lease expired and another request took it over")
	bareKey      = regexp.MustCompile(`^[A-Za-z0-9._:~-]+$`)
)

// parseKey accepts one field holding a Structured Field String, as the draft
// specifies, with any parameters ignored, or a bare value of URL-safe
// characters, which many clients send.
func parseKey(values []string) (string, bool) {
	if len(values) != 1 {
		return "", false
	}
	v := strings.TrimSpace(values[0])
	key, ok := v, bareKey.MatchString(v)
	if strings.HasPrefix(v, `"`) {
		var rest string
		key, rest, ok = unquote(v)
		ok = ok && (rest == "" || (rest[0] == ';' && !strings.Contains(rest, ",")))
	}
	return key, ok && key != "" && len(key) <= 255
}

// unquote decodes the RFC 9651 sf-string that v starts with: printable ASCII
// between double quotes, where only \" and \\ are escapes. It returns the
// value and what follows the closing quote.
func unquote(v string) (string, string, bool) {
	var b strings.Builder
	for i := 1; i < len(v); i++ {
		c := v[i]
		switch {
		case c == '"':
			return b.String(), v[i+1:], true
		case c == '\\':
			i++
			if i == len(v) || (v[i] != '"' && v[i] != '\\') {
				return "", "", false
			}
			b.WriteByte(v[i])
		case c < 0x20 || c > 0x7e:
			return "", "", false
		default:
			b.WriteByte(c)
		}
	}
	return "", "", false
}

// claim takes the key for this request, or returns the stored response of a
// completed one. Concurrent first requests serialise on the primary key.
func (s *Store) claim(ctx context.Context, key string, fp []byte, owner string) (*response, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // a no-op after Commit

	tag, err := tx.Exec(ctx, `INSERT INTO idempotency_keys (key, fingerprint, owner, locked_until)
		VALUES ($1, $2, $3, now() + make_interval(secs => $4)) ON CONFLICT (key) DO NOTHING`,
		key, fp, owner, s.Lease.Seconds())
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 1 {
		return nil, tx.Commit(ctx)
	}

	var (
		storedFP  []byte
		completed bool
		expired   bool
		status    *int
		ctype     *string
		resp      response
	)
	err = tx.QueryRow(ctx, `SELECT fingerprint, completed, locked_until < now(), response_status, response_type, response_body
		FROM idempotency_keys WHERE key = $1 FOR UPDATE`, key).Scan(&storedFP, &completed, &expired, &status, &ctype, &resp.body)
	if err != nil {
		return nil, err
	}
	switch {
	case !bytes.Equal(storedFP, fp):
		return nil, errMismatch
	case completed:
		resp.status = *status
		if ctype != nil {
			resp.contentType = *ctype
		}
		return &resp, nil
	case !expired:
		return nil, errInFlight
	}
	// The previous holder died or overran its lease: take the key over.
	if _, err := tx.Exec(ctx, `UPDATE idempotency_keys SET owner = $2, locked_until = now() + make_interval(secs => $3)
		WHERE key = $1`, key, owner, s.Lease.Seconds()); err != nil {
		return nil, err
	}
	return nil, tx.Commit(ctx)
}

// finish stores a final response, or releases the key after a server error so
// the client can retry. Only the current owner may do either.
func (s *Store) finish(ctx context.Context, key, owner string, resp *response) error {
	var (
		tagRows int64
		err     error
	)
	if resp.status >= http.StatusInternalServerError {
		tag, e := s.pool.Exec(ctx, `DELETE FROM idempotency_keys WHERE key = $1 AND owner = $2 AND NOT completed`, key, owner)
		tagRows, err = tag.RowsAffected(), e
	} else {
		tag, e := s.pool.Exec(ctx, `UPDATE idempotency_keys
			SET completed = true, response_status = $3, response_type = $4, response_body = $5
			WHERE key = $1 AND owner = $2 AND NOT completed`,
			key, owner, resp.status, resp.contentType, resp.body)
		tagRows, err = tag.RowsAffected(), e
	}
	if err != nil {
		return err
	}
	if tagRows == 0 {
		return errLeaseLost
	}
	return nil
}

// RunJanitor deletes keys older than the 24-hour retention every interval
// until ctx ends.
func (s *Store) RunJanitor(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweep(ctx, interval)
		}
	}
}

func (s *Store) sweep(ctx context.Context, budget time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	tag, err := s.pool.Exec(ctx, `DELETE FROM idempotency_keys WHERE created_at < now() - make_interval(secs => $1)`,
		retention.Seconds())
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.log.WarnContext(ctx, "idempotency janitor failed", "error", err)
		}
		return
	}
	if n := tag.RowsAffected(); n > 0 {
		s.log.InfoContext(ctx, "expired idempotency keys deleted", "count", n)
	}
}

func fingerprint(method, path string, body []byte) []byte {
	h := sha256.New()
	h.Write([]byte(method + " " + path + "\n"))
	h.Write(body)
	return h.Sum(nil)
}

type response struct {
	status      int
	contentType string
	body        []byte
}

func (r *response) write(w http.ResponseWriter) {
	if r.contentType != "" {
		w.Header().Set("Content-Type", r.contentType)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(r.body)))
	w.WriteHeader(r.status)
	_, _ = w.Write(r.body) // the status is already sent; a failed write only means the client left
}

// recorder buffers the handler's response so it can be stored before it is sent.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
	wrote  bool
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(code int) {
	if !r.wrote {
		r.status, r.wrote = code, true
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.body.Write(b)
}

func mustSub(f fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		panic(err) // the embedded directory is fixed at build time
	}
	return sub
}
