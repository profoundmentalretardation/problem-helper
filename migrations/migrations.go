// Package migrations embeds the SQL migration files so internal/store can
// apply them without depending on the process's working directory.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
