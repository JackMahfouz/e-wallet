// Package migrations embeds the SQL migrations so the server can apply them at startup.
package migrations

import "embed"

// FS holds the goose migration files, at its root.
//
//go:embed *.sql
var FS embed.FS
