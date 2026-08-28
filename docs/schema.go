// Package schema exposes the authoritative SQLite schema to the application.
package schema

import _ "embed"

// SQLite is the single schema used by production startup and integration tests.
//
//go:embed sqlite-schema.sql
var SQLite string
