// Package migrations embeds the SQL migrations into the binary, so the
// container image needs no extra files and the schema always matches the code.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
