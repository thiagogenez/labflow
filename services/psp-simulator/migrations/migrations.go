// Package migrations embeds the provider's schema.
package migrations

import (
	"embed"

	"github.com/thiagogenez/labflow/libs/go/postgres"
)

//go:embed *.sql
var fsys embed.FS

// Migration creates the provider's tables.
var Migration = postgres.Migration{FS: fsys, Name: "db_version"}
