package store

import (
	"context"
	"log/slog"
	"time"

	"github.com/orpheus-agents/orpheus/internal/diagnostic"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

// CleanupBrowserAuth runs under worker ownership, regardless of browser auth mode.
// Each batch has a bounded size and a separate query timeout.
func CleanupBrowserAuth(ctx context.Context, queries *db.Queries) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Minute):
			for _, task := range []struct {
				table string
				clean func(context.Context) (int64, error)
			}{
				{"browser_login_requests", queries.CleanupBrowserLogins},
				{"browser_sessions", queries.CleanupBrowserSessions},
			} {
				for ctx.Err() == nil {
					batch, cancel := context.WithTimeout(ctx, 5*time.Second)
					n, err := task.clean(batch)
					cancel()
					if err != nil {
						if ctx.Err() == nil {
							slog.WarnContext(ctx, "Browser auth cleanup failed", "table", task.table, "error_type", diagnostic.Describe(err))
						}
						break
					}
					if n < 1000 {
						break
					}
				}
			}
		}
	}
}
