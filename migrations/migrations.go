// Package migrations embeds the registry schema migrations (applied in file name order).
package migrations

import "embed"

// FS holds NNNN_name.sql files.
//
//go:embed *.sql
var FS embed.FS
