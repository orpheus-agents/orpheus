//go:build integration

package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/testutil"
)

func requireTemplateFailure(t *testing.T, err error, code string) {
	t.Helper()
	failure, ok := errors.AsType[*session.APIError](err)
	want := []session.Detail{{Path: []any{"body", "configuration", "sandbox", "template"}, Code: code}}
	if !ok || failure.Status != 422 || failure.Problem.Code != "unknown_template" || !reflect.DeepEqual(failure.Problem.Details, want) {
		t.Fatalf("want unknown_template/%s, got %v", code, err)
	}
}

func TestTemplateAdmissionExactNames(t *testing.T) {
	for _, name := range []string{"codex", "team/codex", "codex:v1", "team/codex:v1", "codex:default"} {
		t.Run(name, func(t *testing.T) {
			s := fixture(t)
			if _, err := s.Pool.Exec(t.Context(), "DELETE FROM templates"); err != nil {
				t.Fatal(err)
			}
			testutil.RegisterTemplate(t, s.Pool, name)
			for _, other := range []string{"codex", "team/codex", "codex:v1", "team/codex:v1", "codex:default", strings.ToUpper(name), " " + name, name + " "} {
				if other == name {
					continue
				}
				req := request()
				req.Create.Configuration.Sandbox.Template = other
				_, err := s.Accept(t.Context(), req)
				requireTemplateFailure(t, err, "template_not_found")
			}
			req := request()
			req.Create.Configuration.Sandbox.Template = name
			a, err := s.Accept(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			record, _, err := s.Read(t.Context(), a.SessionID)
			if err != nil || record.Configuration.Public.Sandbox.Template != name {
				t.Fatal("snapshot changed template name", record, err)
			}
		})
	}
}

func TestEmptyTemplateCatalogDoesNotPersistAdmission(t *testing.T) {
	s := fixture(t)
	if _, err := s.Pool.Exec(t.Context(), "DELETE FROM templates"); err != nil {
		t.Fatal(err)
	}
	req := request()
	_, err := s.Accept(t.Context(), req)
	requireTemplateFailure(t, err, "template_not_found")
	var rows int
	if err := s.Pool.QueryRow(t.Context(), `SELECT
        (SELECT count(*) FROM sessions) + (SELECT count(*) FROM runs) +
        (SELECT count(*) FROM secrets) + (SELECT count(*) FROM idempotency_keys)`).Scan(&rows); err != nil || rows != 0 {
		t.Fatal("rejected admission persisted data", rows, err)
	}
	testutil.RegisterTemplate(t, s.Pool, "codex")
	if _, err := s.Accept(t.Context(), req); err != nil {
		t.Fatal("catalog change required restart or consumed idempotency key", err)
	}
}

// Observe a real blocked backend instead of assuming a goroutine has reached SQL
// after a fixed delay. Return its PID to follow the next lock in the chain.
func waitForTemplateBlock(t *testing.T, s *Store, blocker uint32, query string) uint32 {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var pid uint32
		err := s.Pool.QueryRow(ctx, `SELECT pid FROM pg_stat_activity
            WHERE $1::integer = ANY(pg_blocking_pids(pid)) AND query LIKE '%' || $2::text || '%'`, blocker, query).Scan(&pid)
		if err == nil {
			return pid
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("query %s did not block on backend %d", query, blocker)
		case <-ticker.C:
		}
	}
}

func TestTemplateAdmissionSerializesCatalogChanges(t *testing.T) {
	for _, mutation := range []struct{ name, query, code string }{
		{"deactivate", "UPDATE templates SET active=false WHERE name='codex'", "template_inactive"},
		{"delete", "DELETE FROM templates WHERE name='codex'", "template_not_found"},
	} {
		for _, admissionFirst := range []bool{false, true} {
			order := "mutation first"
			if admissionFirst {
				order = "admission first"
			}
			t.Run(mutation.name+"/"+order, func(t *testing.T) {
				s := fixture(t)
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				barrier, err := s.Pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = barrier.Rollback(context.Background()) }()
				query := mutation.query
				if admissionFirst {
					// Admission locks the template before inserting the session.
					query = "LOCK TABLE sessions IN SHARE MODE"
				}
				if _, err := barrier.Exec(ctx, query); err != nil {
					t.Fatal(err)
				}
				req := request()
				admitted := make(chan error, 1)
				go func() { _, err := s.Accept(ctx, req); admitted <- err }()
				blockedQuery := "GetTemplateForAdmission"
				if admissionFirst {
					blockedQuery = "CreateSession"
				}
				admissionPID := waitForTemplateBlock(t, s, barrier.Conn().PgConn().PID(), blockedQuery)
				changed := make(chan error, 1)
				if admissionFirst {
					go func() { _, err := s.Pool.Exec(ctx, mutation.query); changed <- err }()
					waitForTemplateBlock(t, s, admissionPID, mutation.query)
				}
				if err := barrier.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				err = <-admitted
				if admissionFirst {
					if err != nil {
						t.Fatal("catalog change overtook accepted session", err)
					}
					if err := <-changed; err != nil {
						t.Fatal(err)
					}
					if _, err := s.Accept(ctx, req); err != nil {
						t.Fatal("catalog change rejected replay", err)
					}
				} else {
					requireTemplateFailure(t, err, mutation.code)
				}
				req.Key = uuid.New()
				_, err = s.Accept(ctx, req)
				requireTemplateFailure(t, err, mutation.code)
			})
		}
	}
}

func TestTemplateAdmissionUsesSharedRowLock(t *testing.T) {
	s := fixture(t)
	first, err := s.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Rollback(context.Background()) }()
	if err := validateTemplateForAdmission(t.Context(), first, "codex"); err != nil {
		t.Fatal(err)
	}
	second, err := s.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Rollback(context.Background()) }()
	if _, err := second.Exec(t.Context(), "SET LOCAL lock_timeout = '1s'"); err != nil {
		t.Fatal(err)
	}
	if err := validateTemplateForAdmission(t.Context(), second, "codex"); err != nil {
		t.Fatal("admission row locks conflict", err)
	}
}
