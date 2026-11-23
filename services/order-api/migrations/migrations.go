// Package migrations embeds the order service's schema.
package migrations

import (
	"embed"

	"github.com/thiagogenez/labflow/libs/go/postgres"
)

//go:embed *.sql
var fsys embed.FS

// Migration creates the catalogue, order and outbox tables.
var Migration = postgres.Migration{FS: fsys, Name: "db_version"}
