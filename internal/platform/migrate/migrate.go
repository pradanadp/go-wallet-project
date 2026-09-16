// Package migrate runs the SQL migrations (golang-migrate) that are embedded in the binary.
//
// golang-migrate is used (not ORM auto-migrate) because the DDL is explicit, can be
// reviewed like code, has a rollback path (.down.sql), and records the migration
// version in the schema_migrations table so it is safe to run repeatedly.
package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	migratelib "github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	// database/sql driver for pgx: only migrations need it (the request path keeps
	// using native pgx). Migrations run once at startup/deploy, so the database/sql
	// layer overhead is irrelevant here.
	_ "github.com/jackc/pgx/v5/stdlib"

	wallet "github.com/example/wallet-service"
)

// Command is a migration command supported by the CLI.
type Command string

const (
	CommandUp      Command = "up"
	CommandDown    Command = "down"
	CommandVersion Command = "version"
)

// Run executes a migration command against the database at dsn.
func Run(ctx context.Context, command Command, dsn string, log *slog.Logger) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("opening the migration connection: %w", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("migration connection failed: %w", err)
	}

	source, err := iofs.New(wallet.MigrationsFS, wallet.MigrationsDir)
	if err != nil {
		return fmt.Errorf("reading the migration files: %w", err)
	}

	driver, err := migratepgx.WithInstance(db, &migratepgx.Config{})
	if err != nil {
		return fmt.Errorf("preparing the migration driver: %w", err)
	}

	migrator, err := migratelib.NewWithInstance("iofs", source, "pgx5", driver)
	if err != nil {
		return fmt.Errorf("creating the migrator: %w", err)
	}
	defer func() {
		// Close also closes the source and the driver.
		if srcErr, dbErr := migrator.Close(); srcErr != nil || dbErr != nil {
			log.Warn("closing the migrator", slog.Any("source_error", srcErr), slog.Any("database_error", dbErr))
		}
	}()

	switch command {
	case CommandUp:
		err = migrator.Up()
	case CommandDown:
		err = migrator.Down()
	case CommandVersion:
		return logVersion(migrator, log)
	default:
		return fmt.Errorf("unknown migration command %q (choose up, down, or version)", command)
	}

	if errors.Is(err, migratelib.ErrNoChange) {
		log.Info("no new migrations to apply")
		return nil
	}
	if err != nil {
		return fmt.Errorf("running migration %s: %w", command, err)
	}

	log.Info("migrations finished", slog.String("command", string(command)))
	return nil
}

func logVersion(migrator *migratelib.Migrate, log *slog.Logger) error {
	version, dirty, err := migrator.Version()
	if errors.Is(err, migratelib.ErrNilVersion) {
		log.Info("no migration has been applied yet")
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the migration version: %w", err)
	}
	log.Info("current migration version", slog.Int("version", int(version)), slog.Bool("dirty", dirty))
	if dirty {
		return errors.New("the database schema is dirty: fix it manually before continuing")
	}
	return nil
}
