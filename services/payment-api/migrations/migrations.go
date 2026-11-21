// Package migrations embeds the payment service's schema.
package migrations

import (
	"embed"

	"github.com/thiagogenez/labflow/libs/go/postgres"
)

//go:embed *.sql
var fsys embed.FS

// Migration creates the payment tables.
var Migration = postgres.Migration{FS: fsys, Name: "db_version"}
