// Package sqliteassets exposes the immutable SQLite schema migrations embedded
// in the RKC binary. Runtime code must still verify the checked-in manifest and
// every migration digest before executing any SQL.
package sqliteassets

import "embed"

// Migrations contains the signed-by-source migration manifest and SQL payloads.
//
//go:embed migrations/manifest.json migrations/*.sql
var Migrations embed.FS
