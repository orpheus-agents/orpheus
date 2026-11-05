package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/diagnostic"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/store"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

type workerTask struct {
	done          <-chan struct{}
	cleanupResult *string // Written before done closes, read only after it closes.
}

type sandboxCleanupPass struct {
	nextAt                                     time.Time
	running                                    bool
	pending                                    []uuid.UUID
	active, selected, deleted, skipped, failed int
}

func (p *sandboxCleanupPass) finished(result string) {
	p.active--
	switch result {
	case "deleted":
		p.deleted++
	case "skipped":
		p.skipped++
	default:
		p.failed++
	}
}

// Called by the owner loop only; ordinary and cleanup jobs share its task map.
// A fixed batch avoids an unbounded sweep, and no goroutine waits on a permit.
func (p *sandboxCleanupPass) advance(ctx context.Context, s *store.Store, platform harness.Platform, tasks map[uuid.UUID]workerTask, wg *sync.WaitGroup, now time.Time) error {
	if !p.running {
		if now.Before(p.nextAt) {
			return nil
		}
		cutoff := now.Add(-time.Duration(s.Settings.SandboxPausedRetentionDays) * 24 * time.Hour)
		ids, err := db.New(s.Pool).SandboxCleanupCandidates(ctx, db.SandboxCleanupCandidatesParams{Cutoff: cutoff, NowAt: now, BatchSize: s.Settings.SandboxCleanupBatchSize})
		if err != nil {
			p.nextAt = now.Add(time.Minute)
			return err
		}
		p.nextAt = now.Add(sandboxCleanupInterval)
		p.running, p.pending, p.selected = true, ids, len(ids)
		p.deleted, p.skipped, p.failed = 0, 0, 0
	}
	for len(p.pending) > 0 && p.active < sandboxCleanupConcurrency && ctx.Err() == nil {
		id := p.pending[0]
		p.pending = p.pending[1:]
		if _, busy := tasks[id]; busy {
			p.skipped++
			continue
		}
		done := make(chan struct{})
		result := new("failed")
		tasks[id] = workerTask{done: done, cleanupResult: result}
		p.active++
		retryAt := p.nextAt
		wg.Go(func() {
			defer close(done)
			e := NewExecutor(id, s, platform)
			var err error
			*result, err = e.cleanupRetainedSandbox(ctx, retryAt)
			if err != nil && ctx.Err() == nil {
				slog.WarnContext(ctx, "Paused sandbox deletion failed", "session_id", id, "error_type", diagnostic.Describe(err))
			}
		})
	}
	if len(p.pending) == 0 && p.active == 0 {
		slog.InfoContext(ctx, "Paused sandbox cleanup completed", "selected", p.selected, "deleted", p.deleted, "skipped", p.skipped, "failed", p.failed)
		p.running = false
		// A slow pass does not accumulate ticks and immediately drain another batch.
		if !now.Before(p.nextAt) {
			p.nextAt = now.Add(sandboxCleanupInterval)
		}
	}
	return nil
}
