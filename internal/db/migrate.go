// Package db owns the schema (embedded migrations) and small transaction helpers.
package db

import (
	"embed"
	"errors"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers the pgx5:// driver
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// Migrations are embedded so the API binary can migrate a database without any
// files on disk; the same binary runs as the Compose/Kubernetes migration job.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

func newMigrate(databaseURL string) (*migrate.Migrate, error) {
	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return nil, err
	}
	// golang-migrate picks the driver by URL scheme; pgx v5 registers as "pgx5".
	url := databaseURL
	for _, p := range []string{"postgres://", "postgresql://"} {
		if strings.HasPrefix(url, p) {
			url = "pgx5://" + strings.TrimPrefix(url, p)
		}
	}
	return migrate.NewWithSourceInstance("iofs", src, url)
}

// Migrate applies all pending up migrations. It is a no-op when the schema is current.
func Migrate(databaseURL string) error {
	m, err := newMigrate(databaseURL)
	if err != nil {
		return err
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}

// MigrateDown reverts every migration. Used by tests to prove down migrations work.
func MigrateDown(databaseURL string) error {
	m, err := newMigrate(databaseURL)
	if err != nil {
		return err
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}
