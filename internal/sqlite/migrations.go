package sqlite

import "embed"

// embeddedMigrations is the application's only executable DDL authority.
//
//go:embed migrations/*.sql
var embeddedMigrations embed.FS
