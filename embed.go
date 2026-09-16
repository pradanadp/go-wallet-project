// Package wallet exposes the static assets owned by the service.
//
// The SQL migration files are embedded into the binary so a deployment is a
// single artifact (there is no migration folder to mount into the container).
package wallet

import "embed"

// MigrationsFS holds the golang-migrate files (version_name.{up|down}.sql).
//
//go:embed migrations/*.sql
var MigrationsFS embed.FS

// MigrationsDir is the folder name inside MigrationsFS.
const MigrationsDir = "migrations"
