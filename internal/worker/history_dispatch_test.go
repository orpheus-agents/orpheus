//go:build integration

package worker

import (
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
)

func TestPreparationRetriesDoNotReadUnmaterializedHistory(t *testing.T) {
	s, remote, accepted, e := setup(t)
	remote.rejectEmptyHistory = true
	remote.initializeError = harness.ErrRejected
	for range 3 {
		tick(t, e)
		run, err := s.Run(t.Context(), accepted.SessionID, accepted.RunID)
		if err != nil || run.Status != session.Failed || run.ExecutionStartedAt != nil {
			t.Fatalf("preparation was not rejected: %+v, %v", run, err)
		}
		accepted, err = s.Accept(t.Context(), store.Admission{SessionID: accepted.SessionID, Key: uuid.New(), Messages: []session.TextMessage{{Text: "retry"}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	remote.initializeError = nil
	tick(t, e)
	if remote.starts != 1 || !slices.Equal(remote.rpcOrder, []string{"start"}) {
		t.Fatalf("history read before first dispatched turn: %v", remote.rpcOrder)
	}
}

func TestExecutionTimestampBeforeDispatchDoesNotRequireHistory(t *testing.T) {
	s, remote, _, e := setup(t)
	remote.rejectEmptyHistory = true
	_, err := s.Pool.Exec(t.Context(), `CREATE FUNCTION reject_dispatch() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.kind='start' AND NEW.status='sending' THEN RAISE EXCEPTION 'fixture'; END IF; RETURN NEW; END $$;
CREATE TRIGGER reject_dispatch BEFORE UPDATE ON operations FOR EACH ROW EXECUTE FUNCTION reject_dispatch()`)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Tick(t.Context()); err == nil {
		t.Fatal("expected dispatch persistence failure")
	}
	_, run, err := s.Read(t.Context(), e.ID)
	if err != nil || run == nil || run.ExecutionStartedAt == nil || remote.starts != 0 {
		t.Fatalf("did not reach the pre-dispatch window: %+v %v", run, err)
	}
	if _, err := s.Pool.Exec(t.Context(), "DROP TRIGGER reject_dispatch ON operations"); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	if !slices.Equal(remote.rpcOrder, []string{"start"}) {
		t.Fatalf("timestamp treated as dispatch: %v", remote.rpcOrder)
	}
}

func TestFirstTurnLostAcknowledgementReadsHistoryAfterRestart(t *testing.T) {
	s, remote, accepted, e := setup(t)
	remote.rejectEmptyHistory, remote.lostStart = true, true
	if err := e.Tick(t.Context()); !errors.Is(err, harness.ErrUncertain) {
		t.Fatal(err)
	}
	e.Disconnect()
	replacement := executor(accepted.SessionID, s, remote)
	defer replacement.Disconnect()
	tick(t, replacement)
	if remote.starts != 1 || !slices.Equal(remote.rpcOrder, []string{"start", "snapshot"}) {
		t.Fatalf("lost acknowledgement repeated dispatch: %v", remote.rpcOrder)
	}
	run, err := store.GetRun(t.Context(), s.Pool, accepted.SessionID, accepted.RunID)
	if err != nil || run.NativeTurnID == nil {
		t.Fatalf("turn not recovered: %+v %v", run, err)
	}
}
