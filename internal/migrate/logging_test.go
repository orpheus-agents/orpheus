//go:build integration

package migrate_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/orpheus-agents/orpheus/internal/migrate"
	"github.com/pressly/goose/v3"
)

func captureMigrationLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	var output bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	return &output
}

func migrationLogs(t *testing.T, output *bytes.Buffer) []map[string]any {
	t.Helper()
	if strings.Contains(output.String(), "private-token") || strings.Contains(output.String(), "SELECT") {
		t.Fatal("migration source or error text leaked", output.String())
	}
	var records []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(output.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err, line)
		}
		records = append(records, record)
	}
	return records
}

func writeMigration(t *testing.T, directory, name, up, down string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, name), []byte("-- +goose Up\n"+up+"\n-- +goose Down\n"+down+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationResultLogs(t *testing.T) {
	_, cfg, directory := migrationFixture(t, markerMigration)
	writeMigration(t, directory, "00002_private-token.sql", "", "")
	dsn := migrationDSN(t, cfg, "logging")
	output := captureMigrationLogs(t)
	for _, tt := range []struct {
		command  string
		versions []int
		current  int
	}{
		{"up", []int{1, 2}, 2},
		{"up", nil, 2},
		{"down", []int{2}, 1},
		{"up", []int{2}, 2},
		{"reset", []int{2, 1}, 0},
		{"reset", nil, 0},
	} {
		output.Reset()
		if err := migrate.Run(t.Context(), dsn, tt.command, directory); err != nil {
			t.Fatal(err)
		}
		records := migrationLogs(t, output)
		var versions []int
		for _, record := range records {
			if record["msg"] != "Migration completed" {
				continue
			}
			version := int(record["version"].(float64))
			versions = append(versions, version)
			direction := tt.command
			if direction == "reset" {
				direction = "down"
			}
			if record["direction"] != direction || record["empty"] != (version == 2) || record["level"] != "INFO" {
				t.Fatal(record)
			}
			if duration, ok := record["duration_seconds"].(float64); !ok || duration < 0 {
				t.Fatal("missing duration", record)
			}
		}
		last := records[len(records)-1]
		for _, record := range []map[string]any{records[0], last} {
			if record["command"] != tt.command || record["direction"] != nil {
				t.Fatal("operation should identify the command, not direction", record)
			}
		}
		if !slices.Equal(versions, tt.versions) || last["msg"] != "Database migration completed" || last["current_version"] != float64(tt.current) {
			t.Fatal(tt.command, records)
		}
	}
}

type cancelAfterMigrationHandler struct {
	slog.Handler
	cancel context.CancelFunc
}

func (h cancelAfterMigrationHandler) Handle(ctx context.Context, record slog.Record) error {
	err := h.Handler.Handle(ctx, record)
	if record.Message == "Migration completed" {
		h.cancel()
	}
	return err
}

func TestVersionReadFailureDoesNotFailCompletedMigration(t *testing.T) {
	database, cfg, directory := migrationFixture(t, markerMigration)
	output := captureMigrationLogs(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Cancel after Goose has applied the migration and released its lock, but
	// before the informational version query. No timing or production hooks.
	slog.SetDefault(slog.New(cancelAfterMigrationHandler{Handler: slog.Default().Handler(), cancel: cancel}))
	if err := migrate.Run(ctx, migrationDSN(t, cfg, "logging"), "up", directory); err != nil {
		t.Fatal("version query changed a successful migration into failure", err)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("test did not cancel the version query")
	}
	var count int
	if err := database.QueryRowContext(t.Context(), "SELECT count(*) FROM migration_marker").Scan(&count); err != nil || count != 1 {
		t.Fatal("migration was not committed", count, err)
	}
	records := migrationLogs(t, output)
	warned := false
	for _, record := range records {
		if record["current_version"] != nil || record["level"] == "ERROR" {
			t.Fatal("unavailable version must not produce a value or error", record)
		}
		if record["msg"] == "Database migration version unavailable" {
			warned = record["level"] == "WARN" && record["command"] == "up" && strings.Contains(record["error_type"].(string), "cancelled")
		}
	}
	last := records[len(records)-1]
	if !warned || last["msg"] != "Database migration completed" || last["level"] != "INFO" || last["command"] != "up" {
		t.Fatal("missing warning or successful completion", records)
	}
}

func TestPartialMigrationLogs(t *testing.T) {
	const badSQL = "SELECT 'private-token'::integer;"
	for _, command := range []string{"up", "down", "reset"} {
		t.Run(command, func(t *testing.T) {
			_, cfg, directory := migrationFixture(t, markerMigration)
			up, down := "SELECT 1;", badSQL
			if command == "up" {
				up, down = badSQL, "SELECT 1;"
			}
			writeMigration(t, directory, "00002_private-token.sql", up, down)
			if command == "reset" {
				writeMigration(t, directory, "00003_ok.sql", "SELECT 1;", "SELECT 1;")
			}
			dsn := migrationDSN(t, cfg, "logging")
			if command != "up" {
				if err := migrate.Run(t.Context(), dsn, "up", directory); err != nil {
					t.Fatal(err)
				}
			}
			output := captureMigrationLogs(t)
			err := migrate.Run(t.Context(), dsn, command, directory)
			partial, ok := errors.AsType[*goose.PartialError](err)
			if !ok {
				t.Fatal("missing partial error", err)
			}
			records := migrationLogs(t, output)
			if len(records) != len(partial.Applied)+2 {
				t.Fatal("missing applied results or unexpected completion", records)
			}
			for i, applied := range partial.Applied {
				if records[i+1]["msg"] != "Migration completed" || records[i+1]["version"] != float64(applied.Source.Version) {
					t.Fatal(records)
				}
			}
			last := records[len(records)-1]
			direction := command
			if direction == "reset" {
				direction = "down"
			}
			if last["msg"] != "Migration failed" || last["version"] != float64(2) || last["direction"] != direction || last["level"] != "ERROR" {
				t.Fatal(last)
			}
			if !strings.Contains(last["error_type"].(string), "SQLSTATE=22P02") {
				t.Fatal("missing safe SQL diagnostic", last)
			}
		})
	}
}
