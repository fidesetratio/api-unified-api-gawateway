// Package migrations embeds the numbered SQL migration files.
package migrations

import "embed"

// FS holds every *.sql migration, applied in filename order.
//
//go:embed *.sql
var FS embed.FS
