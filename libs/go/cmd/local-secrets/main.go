// Command local-secrets generates the secrets of the local Docker Compose
// stack in .secrets/ (D40): a random value per file, created when missing or
// replaced with -rotate, and RabbitMQ's definitions with the users' passwords
// filled in. Whenever the set differs from the one it last recorded, it writes
// a new .secrets/generation, which Compose puts in a label, so the next `up`
// recreates every service that reads a secret.
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/thiagogenez/labflow/libs/go/rabbitmq"
)

func main() {
	root := flag.String("root", ".", "repository root")
	rotate := flag.Bool("rotate", false, "replace every secret")
	flag.Parse()
	if err := run(*root, *rotate); err != nil {
		fmt.Fprintln(os.Stderr, "local-secrets:", err)
		os.Exit(1)
	}
}

// password returns 52 random letters and digits.
func password() string { return rand.Text() + rand.Text() }

func tokenKey() string {
	key := make([]byte, 32)
	_, _ = rand.Read(key) // never fails
	return base64.StdEncoding.EncodeToString(key)
}

// secrets lists every file but RabbitMQ's, which follow the template's users.
var secrets = []struct {
	name     string
	generate func() string
}{
	{"postgres-superuser", password},
	{"psp-owner", password}, {"psp-app", password},
	{"payment-owner", password}, {"payment-app", password},
	{"orders-owner", password}, {"orders-app", password},
	{"psp-secret-key", func() string { return "sk_test_" + password() }},
	{"order-token-key", tokenKey},
}

func run(root string, rotate bool) error {
	dir := filepath.Join(root, ".secrets")
	template, err := os.ReadFile(filepath.Join(root, "compose", "rabbitmq", "definitions.template.json"))
	if err != nil {
		return err
	}
	users, err := rabbitmq.DefinitionUsers(template)
	if err != nil {
		return err
	}
	// The directory keeps other users of this machine out; the files inside
	// stay readable by the containers' own users, which on Linux differ.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	unlock, err := lock(dir)
	if err != nil {
		return err
	}
	defer unlock()

	// Every value is settled before any file is written, so a refused one
	// leaves the previous set whole.
	files := map[string][]byte{}
	value := func(name string, generate func() string) (string, error) {
		if !rotate {
			old, err := read(dir, name)
			if err == nil {
				files[name] = old
				return strings.TrimSuffix(string(old), "\n"), nil // as every reader does
			}
			if !errors.Is(err, fs.ErrNotExist) {
				return "", err // never replace a secret that exists but cannot be read
			}
		}
		v := generate()
		files[name] = []byte(v)
		return v, nil
	}
	for _, s := range secrets {
		if _, err := value(s.name, s.generate); err != nil {
			return err
		}
	}
	passwords := map[string]string{}
	for _, user := range users {
		if passwords[user], err = value("rabbitmq-"+user, password); err != nil {
			return err
		}
	}
	if files["rabbitmq-definitions.json"], err = rabbitmq.RenderDefinitions(template, passwords); err != nil {
		return fmt.Errorf("%w (in %s)", err, dir)
	}

	// A new generation whenever the set differs from the one last recorded,
	// however it changed: here, by hand, or by a run that failed halfway.
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	digest := sha256.New()
	for _, name := range names {
		_, _ = fmt.Fprintf(digest, "%s %x\n", name, sha256.Sum256(files[name])) // writing to a hash never fails
	}
	manifest := []byte(fmt.Sprintf("%x\n", digest.Sum(nil)))
	if old, err := read(dir, ".manifest"); err != nil || !bytes.Equal(old, manifest) {
		files["generation"] = []byte(rand.Text())
		files[".manifest"] = manifest
		names = append(names, "generation", ".manifest") // last, once the set is in place
	} else if _, err := read(dir, "generation"); err != nil {
		files["generation"] = []byte(rand.Text())
		names = append(names, "generation")
	}
	for _, name := range names {
		old, err := read(dir, name)
		if err == nil && bytes.Equal(old, files[name]) {
			if err := os.Chmod(filepath.Join(dir, name), 0o644); err != nil {
				return err
			}
			continue
		}
		if err := write(dir, name, files[name]); err != nil {
			return err
		}
	}
	return nil
}

// read returns a managed file's contents, refusing anything but a regular
// file, such as a link that would make the generator read or change a file
// outside the directory.
func read(dir, name string) ([]byte, error) {
	path := filepath.Join(dir, name)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file", path)
	}
	return os.ReadFile(path)
}

// lock serialises runs, so two rotations cannot interleave their files.
func lock(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() { _ = f.Close() }, nil // closing releases the lock
}

// write replaces a file in one step, so no reader sees it half written.
func write(dir, name string, content []byte) error {
	f, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }() // a no-op once renamed
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(0o644); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, name))
}
