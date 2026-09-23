package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/skillum-ai/orpheus/internal/harness"
	"github.com/skillum-ai/orpheus/internal/session"
	"github.com/skillum-ai/orpheus/internal/store/db"
)

func BudgetExhausted(record SessionRecord) bool {
	return record.TotalTokens >= record.Configuration.Public.Limits.MaxSessionTokens
}

// EnforceTokenBudget runs under the session lock, including immediately before
// dispatch. It preserves an earlier cancellation and a confirmed agent result.
func EnforceTokenBudget(ctx context.Context, tx pgx.Tx, record *SessionRecord, run *RunRecord) error {
	if !BudgetExhausted(*record) || run.Status.Terminal() || run.AgentStatus != nil || run.Status == session.Finalizing || run.CancelRequestedAt != nil {
		return nil
	}
	run.CancelRequestedAt = new(time.Now().UTC())
	run.StopReason = new("token_limit")
	run.Status = session.Cancelling
	return PublishRun(ctx, tx, record, run)
}

// ApplyUsage follows Reconcile in the same transaction: a completed turn wins
// over a newly observed cap. Reports are replayable after a failed commit.
func ApplyUsage(ctx context.Context, tx pgx.Tx, record *SessionRecord, reports []harness.UsageReport) error {
	if record.ThreadID == nil || len(reports) == 0 {
		return nil
	}
	ids := make([]string, 0, len(reports))
	for _, report := range reports {
		if report.ContextID == *record.ThreadID {
			ids = append(ids, report.TurnID)
		}
	}
	runs, err := runRecords(db.New(tx).RunsByNativeIDs(ctx, db.RunsByNativeIDsParams{SessionID: record.ID, TurnIds: ids}))
	if err != nil {
		return err
	}
	byTurn := make(map[string]*RunRecord, len(runs))
	for i := range runs {
		byTurn[*runs[i].NativeTurnID] = &runs[i]
	}
	changed := make(map[string]bool, len(runs))
	for _, report := range reports {
		run := byTurn[report.TurnID]
		usage := report.Total
		if report.ContextID != *record.ThreadID || run == nil || usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.TotalTokens < 0 {
			continue
		}
		before := run.Usage
		// Each aggregate only grows to the reported maximum. Run totals cannot
		// overflow: their sum is bounded by the session's int64 counters.
		for _, c := range []struct {
			reported    int64
			saved, used *int64
		}{
			{usage.InputTokens, &record.InputTokens, &run.Usage.InputTokens},
			{usage.OutputTokens, &record.OutputTokens, &run.Usage.OutputTokens},
			{usage.TotalTokens, &record.TotalTokens, &run.Usage.TotalTokens},
		} {
			delta := max(c.reported-*c.saved, 0)
			*c.saved += delta
			*c.used += delta
		}
		changed[report.TurnID] = changed[report.TurnID] || run.Usage != before
	}
	for i := range runs {
		if changed[*runs[i].NativeTurnID] {
			if err := PublishRun(ctx, tx, record, &runs[i]); err != nil {
				return err
			}
		}
	}
	active, err := ActiveRun(ctx, tx, record.ID)
	if err != nil || active == nil {
		return err
	}
	return EnforceTokenBudget(ctx, tx, record, active)
}
