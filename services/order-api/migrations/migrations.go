// Package migrations embeds the order service's schema.
package migrations

import (
	"embed"

	"github.com/thiagogenez/labflow/libs/go/idempotency"
	"github.com/thiagogenez/labflow/libs/go/outbox"
	"github.com/thiagogenez/labflow/libs/go/postgres"
)

//go:embed *.sql
var fsys embed.FS

// Migration creates the catalogue and order tables, and the outbox table,
// which this service created before libs/go/outbox existed.
var Migration = postgres.Migration{FS: fsys, Name: "db_version"}

// All is every migration set of the service, in the order they apply: the
// outbox library's comes after Migration, which already created its table.
var All = []postgres.Migration{idempotency.Migration, Migration, outbox.Migration}
