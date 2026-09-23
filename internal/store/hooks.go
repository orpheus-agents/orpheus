package store

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skillum-ai/orpheus/internal/session"
	"github.com/skillum-ai/orpheus/internal/store/db"
)

type HookExecution = db.HookExecution

func (s *Store) Hook(ctx context.Context, runID uuid.UUID, name string) (*HookExecution, error) {
	h, err := db.New(s.Pool).GetHookExecution(ctx, db.GetHookExecutionParams{RunID: runID, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &h, err
}

func FailUnfinishedHooks(ctx context.Context, tx pgx.Tx, runID uuid.UUID, activeName string, problem *session.Error) error {
	hooks, err := db.New(tx).HookExecutions(ctx, runID)
	if err != nil {
		return err
	}
	for _, h := range hooks {
		if h.Status != "pending" && h.Status != "running" {
			continue
		}
		h.FinishedAt = new(time.Now().UTC())
		if h.Name == activeName && h.Status == "running" {
			h.Status = "failed"
			h.Error = problem
			h.OutputCompleteness = "unavailable"
		} else {
			h.Status = "skipped"
		}
		if err := db.New(tx).SaveHookExecution(ctx, db.SaveHookExecutionParams{
			ID: h.ID, Status: h.Status, StartedAt: h.StartedAt, DeadlineAt: h.DeadlineAt,
			CancelAttemptedAt: h.CancelAttemptedAt, FinishedAt: h.FinishedAt,
			ExitCode: h.ExitCode, Signal: h.Signal, Output: h.Output,
			OutputCompleteness: h.OutputCompleteness, TruncationReason: h.TruncationReason,
			Error: h.Error,
		}); err != nil {
			return err
		}
	}
	return nil
}

// ChangeHook commits a hook transition and its run.updated projection together.
func (s *Store) ChangeHook(ctx context.Context, sid, rid uuid.UUID, name string, change func(*HookExecution, *RunRecord) error) error {
	return s.Mutate(ctx, sid, false, func(tx pgx.Tx, record *SessionRecord) error {
		h, err := db.New(tx).GetHookExecution(ctx, db.GetHookExecutionParams{RunID: rid, Name: name})
		if err != nil {
			return err
		}
		r, err := GetRun(ctx, tx, sid, rid)
		if err != nil {
			return err
		}
		beforeHook, beforeRun := h, r
		if err := change(&h, &r); err != nil {
			return err
		}
		if reflect.DeepEqual(beforeHook, h) && reflect.DeepEqual(beforeRun, r) {
			return nil
		}
		for _, field := range []**time.Time{&h.StartedAt, &h.DeadlineAt, &h.CancelAttemptedAt, &h.FinishedAt} {
			if *field != nil {
				*field = new((*field).UTC().Truncate(time.Microsecond))
			}
		}
		if err := db.New(tx).SaveHookExecution(ctx, db.SaveHookExecutionParams{
			ID: h.ID, Status: h.Status, StartedAt: h.StartedAt, DeadlineAt: h.DeadlineAt,
			CancelAttemptedAt: h.CancelAttemptedAt,
			FinishedAt:        h.FinishedAt, ExitCode: h.ExitCode, Signal: h.Signal,
			Output: h.Output, OutputCompleteness: h.OutputCompleteness,
			TruncationReason: h.TruncationReason, Error: h.Error,
		}); err != nil {
			return err
		}
		return PublishRun(ctx, tx, record, &r)
	})
}
