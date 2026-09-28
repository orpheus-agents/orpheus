//go:build integration

package store

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/session"
)

func bindUsageRun(t *testing.T, s *Store, a session.Acceptance, turn string) {
	t.Helper()
	if err := s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, r *SessionRecord) error {
		r.ThreadID = new("thread")
		run, err := GetRun(t.Context(), tx, a.SessionID, a.RunID)
		if err != nil {
			return err
		}
		run.NativeTurnID = &turn
		run.Status = session.Running
		run.ExecutionStartedAt = new(time.Now().UTC())
		return SaveRun(t.Context(), tx, &run)
	}); err != nil {
		t.Fatal(err)
	}
}
func reportUsage(t *testing.T, s *Store, sid uuid.UUID, reports ...harness.UsageReport) {
	t.Helper()
	if err := s.Mutate(t.Context(), sid, false, func(tx pgx.Tx, r *SessionRecord) error { return ApplyUsage(t.Context(), tx, r, reports) }); err != nil {
		t.Fatal(err)
	}
}
func usageReport(turn string, input, output int64) harness.UsageReport {
	return harness.UsageReport{ContextID: "thread", TurnID: turn, Total: session.Usage{InputTokens: input, OutputTokens: output, TotalTokens: input + output}}
}
func breakdownReport(turn string, input, cached, output, reasoning int64) harness.UsageReport {
	report := usageReport(turn, input, output)
	report.CachedInputTokens = &cached
	report.ReasoningOutputTokens = &reasoning
	return report
}

func TestUsageBreakdownAcrossRunsAndReplay(t *testing.T) {
	s := fixture(t)
	a, err := s.Accept(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	bindUsageRun(t, s, a, "first")
	first := breakdownReport("first", 90, 60, 10, 4)
	reportUsage(t, s, a.SessionID, first)
	record, _, err := s.Read(t.Context(), a.SessionID)
	if err != nil || record.Usage() != (session.Usage{InputTokens: 90, CachedInputTokens: 60, OutputTokens: 10, ReasoningOutputTokens: 4, TotalTokens: 100}) {
		t.Fatal(record.Usage(), err)
	}
	before := record.NextEventSequence
	reportUsage(t, s, a.SessionID, first, breakdownReport("first", 80, 50, 9, 3))
	record, _, err = s.Read(t.Context(), a.SessionID)
	if err != nil || record.NextEventSequence != before {
		t.Fatal(record.NextEventSequence, err)
	}
	// A corrected breakdown is published even when the main counters stay put.
	reportUsage(t, s, a.SessionID, breakdownReport("first", 90, 61, 10, 5))
	record, _, err = s.Read(t.Context(), a.SessionID)
	if err != nil || record.NextEventSequence != before+1 || record.CachedInputTokens != 61 || record.ReasoningOutputTokens != 5 {
		t.Fatal(record, err)
	}
	if err := s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, r *SessionRecord) error {
		run, err := GetRun(t.Context(), tx, a.SessionID, a.RunID)
		if err != nil {
			return err
		}
		return AgentFinished(t.Context(), tx, r, &run, session.Completed, nil, nil)
	}); err != nil {
		t.Fatal(err)
	}
	b, err := s.Accept(t.Context(), Admission{SessionID: a.SessionID, Key: uuid.New(), Messages: []session.TextMessage{{Text: "next"}}})
	if err != nil {
		t.Fatal(err)
	}
	bindUsageRun(t, s, b, "second")
	reportUsage(t, s, a.SessionID, breakdownReport("second", 120, 75, 20, 8))
	second, err := s.Run(t.Context(), a.SessionID, b.RunID)
	want := session.Usage{InputTokens: 30, CachedInputTokens: 14, OutputTokens: 10, ReasoningOutputTokens: 3, TotalTokens: 40}
	if err != nil || second.Usage != want {
		t.Fatal(second.Usage, err)
	}
	record, _, err = s.Read(t.Context(), a.SessionID)
	if err != nil || record.Usage() != (session.Usage{InputTokens: 120, CachedInputTokens: 75, OutputTokens: 20, ReasoningOutputTokens: 8, TotalTokens: 140}) {
		t.Fatal(record.Usage(), err)
	}
}

func TestUsageBreakdownStartsAfterLegacyBaseline(t *testing.T) {
	s := fixture(t)
	a, err := s.Accept(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(t.Context(), `UPDATE sessions SET cached_input_native_total=NULL, reasoning_output_native_total=NULL WHERE id=$1`, a.SessionID); err != nil {
		t.Fatal(err)
	}
	bindUsageRun(t, s, a, "legacy")
	reportUsage(t, s, a.SessionID, breakdownReport("legacy", 100, 70, 10, 4))
	record, _, err := s.Read(t.Context(), a.SessionID)
	run, runErr := s.Run(t.Context(), a.SessionID, a.RunID)
	if err != nil || runErr != nil || record.CachedInputNativeTotal == nil || *record.CachedInputNativeTotal != 70 || record.ReasoningOutputNativeTotal == nil || *record.ReasoningOutputNativeTotal != 4 || record.CachedInputTokens != 0 || record.ReasoningOutputTokens != 0 || run.Usage.CachedInputTokens != 0 || run.Usage.ReasoningOutputTokens != 0 {
		t.Fatal(record, run, err, runErr)
	}
	reportUsage(t, s, a.SessionID, breakdownReport("legacy", 120, 80, 15, 6))
	partial := usageReport("legacy", 130, 20)
	partial.ReasoningOutputTokens = new(int64(7))
	reportUsage(t, s, a.SessionID, partial)
	reportUsage(t, s, a.SessionID, breakdownReport("legacy", 140, 90, 25, 8))
	record, _, err = s.Read(t.Context(), a.SessionID)
	run, runErr = s.Run(t.Context(), a.SessionID, a.RunID)
	if err != nil || runErr != nil || record.CachedInputTokens != 20 || record.ReasoningOutputTokens != 4 || run.Usage.CachedInputTokens != 20 || run.Usage.ReasoningOutputTokens != 4 {
		t.Fatal(record, run, err, runErr)
	}
}
func TestUsageAcrossRunsAndReplay(t *testing.T) {
	s := fixture(t)
	req := request()
	req.Create.Configuration.Limits.MaxSessionTokens = 100
	a, err := s.Accept(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	bindUsageRun(t, s, a, "first")
	first := usageReport("first", 30, 10)
	reportUsage(t, s, a.SessionID, first)
	before, _, err := s.Read(t.Context(), a.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	other := first
	other.ContextID = "unrelated"
	reportUsage(t, s, a.SessionID, first, usageReport("first", 20, 5), usageReport("unknown", 500, 10), other)
	after, _, err := s.Read(t.Context(), a.SessionID)
	if err != nil || after.NextEventSequence != before.NextEventSequence || after.Usage() != first.Total {
		t.Fatal(after.Usage(), err)
	}
	if err := s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, r *SessionRecord) error {
		run, err := GetRun(t.Context(), tx, a.SessionID, a.RunID)
		if err != nil {
			return err
		}
		return AgentFinished(t.Context(), tx, r, &run, session.Failed, nil, nil)
	}); err != nil {
		t.Fatal(err)
	}
	nextReq := Admission{SessionID: a.SessionID, Key: uuid.New(), Messages: []session.TextMessage{{Text: "next"}}}
	b, err := s.Accept(t.Context(), nextReq)
	if err != nil {
		t.Fatal(err)
	}
	bindUsageRun(t, s, b, "second")
	reportUsage(t, s, a.SessionID, usageReport("second", 50, 20))
	want := session.Usage{InputTokens: 20, OutputTokens: 10, TotalTokens: 30}
	second, err := s.Run(t.Context(), a.SessionID, b.RunID)
	if err != nil || second.Usage != want {
		t.Fatal(second, err)
	}
	reportUsage(t, s, a.SessionID, usageReport("second", 70, 30))
	second, err = s.Run(t.Context(), a.SessionID, b.RunID)
	if err != nil || second.Status != session.Cancelling || second.StopReason == nil || *second.StopReason != "token_limit" {
		t.Fatal(second, err)
	}
	record, _, err := s.Read(t.Context(), a.SessionID)
	if err != nil || record.TotalTokens != 100 {
		t.Fatal(record, err)
	}
	_, err = s.Accept(t.Context(), Admission{SessionID: a.SessionID, RunID: b.RunID, Key: uuid.New(), Messages: []session.TextMessage{{Text: "steer"}}})
	requireCode(t, err, "token_limit_exceeded")
	_, err = s.Accept(t.Context(), Admission{SessionID: a.SessionID, Key: uuid.New(), Messages: []session.TextMessage{{Text: "next"}}})
	requireCode(t, err, "token_limit_exceeded")
	if got, err := s.Accept(t.Context(), req); err != nil || got != a {
		t.Fatal(got, err)
	}
	if got, err := s.Accept(t.Context(), nextReq); err != nil || got != b {
		t.Fatal(got, err)
	}
	firstRun, err := s.Run(t.Context(), a.SessionID, a.RunID)
	if err != nil || firstRun.Usage != first.Total || firstRun.Status != session.Failed {
		t.Fatal(firstRun, err)
	}
}
func TestUsageRollbackAndCompletion(t *testing.T) {
	for _, hooks := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "finalizing"}[hooks], func(t *testing.T) {
			s := fixture(t)
			req := request()
			req.Create.Configuration.Limits.MaxSessionTokens = 10
			if hooks {
				req.Create.Configuration.Hooks = &session.HooksInput{AfterRun: new("#!/bin/sh\ntrue\n")}
			}
			a, err := s.Accept(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			bindUsageRun(t, s, a, "turn")
			snap := harness.Snapshot{Turns: []harness.Turn{{NativeID: "turn", Status: session.Completed}}, Usage: []harness.UsageReport{usageReport("turn", 8, 2)}}
			apply := func(tx pgx.Tx, r *SessionRecord) error {
				if err := Reconcile(t.Context(), tx, r, snap); err != nil {
					return err
				}
				return ApplyUsage(t.Context(), tx, r, snap.Usage)
			}
			rollback := errors.New("rollback")
			err = s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, r *SessionRecord) error {
				if err := apply(tx, r); err != nil {
					return err
				}
				return rollback
			})
			if !errors.Is(err, rollback) {
				t.Fatal(err)
			}
			record, run, err := s.Read(t.Context(), a.SessionID)
			if err != nil || record.TotalTokens != 0 || run == nil || run.Status != session.Running {
				t.Fatal(record, run, err)
			}
			if err := s.Mutate(t.Context(), a.SessionID, false, apply); err != nil {
				t.Fatal(err)
			}
			view, err := s.Run(t.Context(), a.SessionID, a.RunID)
			want := session.Completed
			if hooks {
				want = session.Finalizing
			}
			if err != nil || view.Status != want || view.CancelRequestedAt != nil || view.Usage.TotalTokens != 10 || view.AgentStatus == nil || *view.AgentStatus != session.Completed {
				t.Fatal(view, err)
			}
			reportUsage(t, s, a.SessionID, snap.Usage...)
			view, err = s.Run(t.Context(), a.SessionID, a.RunID)
			if err != nil || view.Status != want || view.CancelRequestedAt != nil {
				t.Fatal(view, err)
			}
		})
	}
}
func TestUsagePreservesCancellationAndInt64(t *testing.T) {
	s := fixture(t)
	a, err := s.Accept(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	bindUsageRun(t, s, a, "turn")
	if _, err := s.Cancel(t.Context(), a.SessionID, a.RunID); err != nil {
		t.Fatal(err)
	}
	reportUsage(t, s, a.SessionID, usageReport("turn", math.MaxInt64-1, 1))
	reportUsage(t, s, a.SessionID, usageReport("turn", math.MaxInt64-1, 1))
	view, err := s.Run(t.Context(), a.SessionID, a.RunID)
	if err != nil || view.Usage.TotalTokens != math.MaxInt64 || view.StopReason == nil || *view.StopReason != "user_request" {
		t.Fatal(view, err)
	}
}
