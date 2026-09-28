package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

type cleanupDB struct {
	db.DBTX
	exec func(context.Context, string) (pgconn.CommandTag, error)
}

func (d cleanupDB) Exec(ctx context.Context, query string, _ ...any) (pgconn.CommandTag, error) {
	return d.exec(ctx, query)
}

func TestCleanupIntervalAndBatches(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var mu sync.Mutex
		var calls []string
		snapshot := func() []string {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(calls)
		}
		queries := db.New(cleanupDB{exec: func(ctx context.Context, query string) (pgconn.CommandTag, error) {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) != 5*time.Second {
				t.Error("batch must have its own five-second timeout")
			}
			kind := "sessions"
			if strings.Contains(query, "browser_login_requests") {
				kind = "logins"
			}
			mu.Lock()
			defer mu.Unlock()
			calls = append(calls, kind)
			if len(calls) == 1 {
				return pgconn.NewCommandTag("DELETE 1000"), nil
			}
			return pgconn.NewCommandTag("DELETE 1"), nil
		}})
		done := make(chan struct{})
		go func() { defer close(done); CleanupBrowserAuth(ctx, queries) }()
		synctest.Wait()
		time.Sleep(10*time.Minute - time.Second)
		synctest.Wait()
		if got := snapshot(); len(got) != 0 {
			t.Fatal("cleanup ran before its interval", got)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if got := snapshot(); !slices.Equal(got, []string{"logins", "logins", "sessions"}) {
			t.Fatal("full batch was not drained before moving to sessions", got)
		}
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		if got := snapshot(); !slices.Equal(got, []string{"logins", "logins", "sessions", "logins", "sessions"}) {
			t.Fatal("cleanup did not repeat", got)
		}
		cancel()
		<-done
	})
}

func TestCleanupCancellation(t *testing.T) {
	for _, duringQuery := range []bool{false, true} {
		name := "waiting"
		if duringQuery {
			name = "query"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var calls atomic.Int64
				queries := db.New(cleanupDB{exec: func(ctx context.Context, _ string) (pgconn.CommandTag, error) {
					calls.Add(1)
					<-ctx.Done()
					if !errors.Is(ctx.Err(), context.Canceled) {
						t.Error("query was not cancelled with cleanup", ctx.Err())
					}
					return pgconn.CommandTag{}, ctx.Err()
				}})
				done := make(chan struct{})
				go func() { defer close(done); CleanupBrowserAuth(ctx, queries) }()
				synctest.Wait()
				if duringQuery {
					time.Sleep(10 * time.Minute)
					synctest.Wait()
					if calls.Load() != 1 {
						t.Fatal("query did not start", calls.Load())
					}
				}
				cancel()
				<-done
				want := int64(0)
				if duringQuery {
					want = 1
				}
				if calls.Load() != want {
					t.Fatal("new query started after cancellation", calls.Load())
				}
			})
		})
	}
}

func TestCleanupTimeoutAndRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var calls atomic.Int64
		queries := db.New(cleanupDB{exec: func(ctx context.Context, _ string) (pgconn.CommandTag, error) {
			calls.Add(1)
			if calls.Load() == 1 {
				<-ctx.Done()
				if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
					t.Error("query timeout was not enforced", ctx.Err())
				}
				return pgconn.CommandTag{}, ctx.Err()
			}
			return pgconn.NewCommandTag("DELETE 0"), nil
		}})
		done := make(chan struct{})
		go func() { defer close(done); CleanupBrowserAuth(ctx, queries) }()
		synctest.Wait()
		time.Sleep(10*time.Minute + 5*time.Second)
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatal("timeout should skip to the next table", calls.Load())
		}
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		if calls.Load() != 4 {
			t.Fatal("cleanup did not retry after timeout", calls.Load())
		}
		cancel()
		<-done
	})
}

func TestCleanupErrorLogging(t *testing.T) {
	for _, kind := range []string{"database", "timeout", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			var output bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				queries := db.New(cleanupDB{exec: func(batch context.Context, query string) (pgconn.CommandTag, error) {
					if !strings.Contains(query, "browser_login_requests") {
						return pgconn.NewCommandTag("DELETE 0"), nil
					}
					switch kind {
					case "database":
						return pgconn.CommandTag{}, &pgconn.PgError{Code: "42501", Message: "private SQL and secret values"}
					case "cancelled":
						cancel()
					}
					<-batch.Done()
					return pgconn.CommandTag{}, batch.Err()
				}})
				done := make(chan struct{})
				go func() { defer close(done); CleanupBrowserAuth(ctx, queries) }()
				synctest.Wait()
				time.Sleep(10*time.Minute + 5*time.Second)
				synctest.Wait()
				cancel()
				<-done
			})
			if kind == "cancelled" {
				if output.Len() != 0 {
					t.Fatal("normal cancellation logged as a failure", output.String())
				}
				return
			}
			var record struct {
				Level     string `json:"level"`
				Message   string `json:"msg"`
				Table     string `json:"table"`
				ErrorType string `json:"error_type"`
			}
			if err := json.Unmarshal(output.Bytes(), &record); err != nil {
				t.Fatal("expected one log record", err, output.String())
			}
			if record.Level != "WARN" || record.Message != "Browser auth cleanup failed" || record.Table != "browser_login_requests" {
				t.Fatal("missing cleanup failure context", record)
			}
			want := "SQLSTATE=42501"
			if kind == "timeout" {
				want = "deadline_exceeded"
			}
			if !strings.Contains(record.ErrorType, want) || strings.Contains(output.String(), "private") || strings.Contains(output.String(), "secret") {
				t.Fatal("unsafe or missing error diagnostic", output.String())
			}
		})
	}
}
