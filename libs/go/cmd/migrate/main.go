// Command migrate runs the schema migrations of a service whose own image
// holds no Go, from that service's migrations image (D57). Every directory in
// /migrations is one set of goose SQL files with its own version table,
// goose.<directory>, applied in name order with what `/service migrate` uses
// in the Go services: the schema owner's MIGRATIONS_DATABASE_URL, its
// password from MIGRATIONS_DATABASE_PASSWORD_FILE, an advisory lock and
// MIGRATION_TIMEOUT. The image names its service in SERVICE, for the logs.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"syscall"

	"github.com/thiagogenez/labflow/libs/go/postgres"
)

const root = "/migrations"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := run(ctx, os.Getenv("SERVICE"), root); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, service, root string) error {
	if service == "" {
		return errors.New("SERVICE is required")
	}
	migrations, err := sets(root)
	if err != nil {
		return err
	}
	return postgres.MigrateFromEnv(ctx, service, migrations...)
}

// setName is an identifier PostgreSQL takes unquoted in goose.<name>.
var setName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// sets returns one migration set per directory in root, in name order, and
// refuses anything else there, so a misplaced file fails the job instead of
// being skipped.
func sets(root string) ([]postgres.Migration, error) {
	entries, err := os.ReadDir(root) // sorted by name
	if err != nil {
		return nil, err
	}
	fsys := os.DirFS(root)
	migrations := make([]postgres.Migration, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || !setName.MatchString(e.Name()) {
			return nil, fmt.Errorf("%s/%s: want only directories named like a PostgreSQL identifier (a-z, 0-9, _)", root, e.Name())
		}
		migrations = append(migrations, postgres.NewMigration(fsys, e.Name(), e.Name()))
	}
	if len(migrations) == 0 {
		return nil, fmt.Errorf("%s holds no migration set", root)
	}
	return migrations, nil
}
