// Package migrate applies the Goose SQL files shipped with the application.
package migrate

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/orpheus-agents/orpheus/internal/diagnostic"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

func Provider(db *sql.DB, directory string, opts ...goose.ProviderOption) (*goose.Provider, error) {
	return goose.NewProvider(goose.DialectPostgres, db, os.DirFS(directory), opts...)
}
func Run(ctx context.Context, url, command, directory string) error {
	db, err := sql.Open("pgx", url)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return err
	}
	p, err := Provider(db, directory, goose.WithSessionLocker(sessionLocker{locker}))
	if err != nil {
		return err
	}
	if command != "down" && command != "reset" {
		command = "up"
	}
	slog.InfoContext(ctx, "Database migration started", "command", command)
	var results []*goose.MigrationResult
	switch command {
	case "down":
		var result *goose.MigrationResult
		result, err = p.Down(ctx)
		if result != nil {
			results = append(results, result)
		}
	case "reset":
		results, err = p.DownTo(ctx, 0)
	default:
		results, err = p.Up(ctx)
	}
	partial, failed := errors.AsType[*goose.PartialError](err)
	if failed {
		results = partial.Applied
	}
	for _, result := range results {
		logMigrationResult(ctx, result)
	}
	if failed {
		logMigrationResult(ctx, partial.Failed)
	}
	if err != nil {
		return err
	}
	// GetVersions observes the current version without acquiring the migration lock again.
	version, _, err := p.GetVersions(ctx)
	attrs := []any{"command", command}
	if err != nil {
		slog.WarnContext(ctx, "Database migration version unavailable", "command", command, "error_type", diagnostic.Describe(err))
	} else {
		attrs = append(attrs, "current_version", version)
	}
	slog.InfoContext(ctx, "Database migration completed", attrs...)
	return nil
}

func logMigrationResult(ctx context.Context, result *goose.MigrationResult) {
	attrs := []any{"version", result.Source.Version, "direction", result.Direction,
		"duration_seconds", result.Duration.Seconds(), "empty", result.Empty}
	if result.Error != nil {
		slog.ErrorContext(ctx, "Migration failed", append(attrs, "error_type", diagnostic.Describe(result.Error))...)
		return
	}
	slog.InfoContext(ctx, "Migration completed", attrs...)
}
