package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

func (s *Store) Operation(ctx context.Context, sid uuid.UUID, kind string, rid, mid *uuid.UUID, parameters any) (Operation, error) {
	var out Operation
	err := s.Mutate(ctx, sid, false, func(tx pgx.Tx, _ *SessionRecord) error {
		v, err := db.New(tx).FindOperation(ctx, db.FindOperationParams{SessionID: sid, Kind: kind, RunID: rid, MessageID: mid})
		if err == nil && v.Status != "failed" {
			out = v
			return nil
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if parameters == nil {
			parameters = struct{}{}
		}
		raw, err := json.Marshal(parameters)
		if err != nil {
			return err
		}
		out, err = db.New(tx).CreateOperation(ctx, db.CreateOperationParams{ID: uuid.New(), SessionID: sid, RunID: rid, MessageID: mid, Kind: kind, Parameters: raw})
		return err
	})
	return out, err
}
func SaveOperation(ctx context.Context, tx db.DBTX, o *Operation) error {
	return db.New(tx).SaveOperation(ctx, db.SaveOperationParams{ID: o.ID, Status: o.Status, Result: o.Result, AttemptedAt: o.AttemptedAt})
}
func (s *Store) SetOperation(ctx context.Context, sid, id uuid.UUID, status string, result any) error {
	return s.Mutate(ctx, sid, false, func(tx pgx.Tx, _ *SessionRecord) error {
		o, err := db.New(tx).GetSessionOperation(ctx, db.GetSessionOperationParams{ID: id, SessionID: sid})
		if err != nil {
			return err
		}
		o.Status = status
		if status == "sending" && o.AttemptedAt == nil {
			o.AttemptedAt = new(time.Now().UTC())
		}
		if result != nil {
			raw, err := json.Marshal(result)
			if err != nil {
				return err
			}
			o.Result = raw
		}
		return SaveOperation(ctx, tx, &o)
	})
}
func (s *Store) GetOperation(ctx context.Context, id uuid.UUID) (*Operation, error) {
	v, err := db.New(s.Pool).GetOperation(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &v, err
}
func (s *Store) LatestAttempt(ctx context.Context, sid uuid.UUID, kind string) (*Operation, error) {
	v, err := db.New(s.Pool).LatestAttempt(ctx, db.LatestAttemptParams{SessionID: sid, Kind: kind})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &v, err
}
func (s *Store) LastTerminalRun(ctx context.Context, sid uuid.UUID) (RunRecord, error) {
	return runRecord(db.New(s.Pool).LastTerminalRun(ctx, sid))
}
func (s *Store) AuthFailedBefore(ctx context.Context, sid uuid.UUID, number int, includeCurrent bool) (bool, error) {
	if includeCurrent {
		number++
	}
	r, err := runRecord(db.New(s.Pool).PreviousExecutedRun(ctx, db.PreviousExecutedRunParams{SessionID: sid, Number: number}))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return r.Error != nil && r.Error.Code == "authentication_failed", nil
}
