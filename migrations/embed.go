// Package migrations embeds the goose SQL migrations. Files are plain SQL,
// forward-only, named NNNN_<module>_<desc>.sql, and applied by
// `exchange migrate up` (or the compose `migrate` service) with the
// ex_migrate role. Login roles themselves are created outside migrations
// (infra/postgres/initdb/01-roles.sh).
package migrations

import "embed"

// FS contains every *.sql migration.
//
//go:embed *.sql
var FS embed.FS
