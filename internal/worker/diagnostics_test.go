//go:build integration

package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/diagnostic"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/harness/codex"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
)

func failureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var log bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&log, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &log
}

func rpcFailure() error {
	return diagnostic.With(&codex.RPCError{Code: -32000, NativeMessage: "private-token"}, diagnostic.Fields{
		ID: uuid.New(), Kind: "rpc_rejected", RPCMethod: "account/read", RPCID: uuid.NewString(), RPCCode: new(-32000), NativeMessage: new("backend refused [REDACTED]"),
	})
}

func TestFailureLogMatchesPersistedRunAndEvent(t *testing.T) {
	for _, where := range []string{"initialize", "start", "steer"} {
		t.Run(where, func(t *testing.T) {
			s, remote, accepted, e := setup(t)
			log := failureLog(t)
			cause := rpcFailure()
			fields := diagnostic.Capture(cause)
			switch where {
			case "initialize":
				remote.initializeError = cause
			case "start":
				remote.startError = cause
			case "steer":
				tick(t, e)
				_, err := s.Accept(t.Context(), store.Admission{SessionID: accepted.SessionID, RunID: accepted.RunID, Key: uuid.New(), Messages: []session.TextMessage{{Text: "more"}}})
				if err != nil {
					t.Fatal(err)
				}
				remote.steerError = cause
			}
			tick(t, e)
			if strings.Count(log.String(), `"event":"execution.failure"`) != 1 || strings.Contains(log.String(), "private-token") {
				t.Fatalf("duplicated or unsafe diagnostic: %s", log)
			}
			var event map[string]any
			if err := json.Unmarshal(log.Bytes(), &event); err != nil {
				t.Fatal(err)
			}
			wantPhase := "execution"
			if where == "initialize" {
				wantPhase = "preparation"
			}
			if event["diagnostic_id"] != fields.ID.String() || event["run_id"] != accepted.RunID.String() || event["phase"] != wantPhase {
				t.Fatal("wrong binding", event)
			}
			run, err := s.Run(t.Context(), accepted.SessionID, accepted.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if where == "steer" {
				messages, err := store.Messages(t.Context(), s.Pool, accepted.RunID)
				if err != nil {
					t.Fatal(err)
				}
				problem := messages[len(messages)-1].Error
				if run.Status != session.Running || problem == nil || problem.DiagnosticID == nil || *problem.DiagnosticID != fields.ID {
					t.Fatal("steer did not preserve ID")
				}
			} else if run.Error == nil || run.Error.DiagnosticID == nil || *run.Error.DiagnosticID != fields.ID {
				t.Fatal("run did not preserve ID", run)
			}
			page, err := s.Events(t.Context(), accepted.SessionID, "0", 100)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(page)
			if err != nil || !bytes.Contains(raw, []byte(fields.ID.String())) || bytes.Contains(raw, []byte("private-token")) || bytes.Contains(raw, []byte("backend refused")) {
				t.Fatal("unsafe or missing public event")
			}
		})
	}
}

func TestExecutionFailureKeepsPriorityAndOriginalRun(t *testing.T) {
	s, remote, accepted, e := setup(t)
	log := failureLog(t)
	remote.initializeError = harness.FailureCause("context_lost", "Context is unavailable.", rpcFailure())
	err := e.Tick(t.Context())
	f, ok := errors.AsType[*harness.ExecutionError](err)
	if !ok || f.Code != "context_lost" {
		t.Fatal("nested rejection changed classification", err)
	}
	if _, err := s.Cancel(t.Context(), accepted.SessionID, accepted.RunID); err != nil {
		t.Fatal(err)
	}
	remote.initializeError = nil
	tick(t, e)
	next, err := s.Accept(t.Context(), store.Admission{SessionID: accepted.SessionID, Key: uuid.New(), Messages: []session.TextMessage{{Text: "next"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.failure(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	run, err := s.Run(t.Context(), next.SessionID, next.RunID)
	if err != nil || run.Status != session.Accepted || run.Error != nil {
		t.Fatal("stale failure changed next run", run, err)
	}
	old, err := s.Run(t.Context(), accepted.SessionID, accepted.RunID)
	if err != nil || old.Status != session.Cancelled || old.Error != nil {
		t.Fatal("failure rewrote cancellation", old, err)
	}
	if strings.Contains(log.String(), next.RunID.String()) {
		t.Fatal("stale failure attributed to next run")
	}
}

func TestOperationSaveFailureRetainsDiagnosticWithoutRejection(t *testing.T) {
	s, remote, accepted, e := setup(t)
	remote.startError = rpcFailure()
	_, err := s.Pool.Exec(t.Context(), `CREATE FUNCTION reject_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.kind='start' AND NEW.status='failed' THEN RAISE EXCEPTION 'fixture'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_failure BEFORE UPDATE ON operations FOR EACH ROW EXECUTE FUNCTION reject_failure()`)
	if err != nil {
		t.Fatal(err)
	}
	err = e.Tick(t.Context())
	if err == nil || errors.Is(err, harness.ErrRejected) {
		t.Fatal("database failure became a known rejection", err)
	}
	fields := diagnostic.Capture(err)
	if fields.ID != diagnostic.Capture(remote.startError).ID || fields.OperationID == nil || fields.RunID == nil || *fields.RunID != accepted.RunID {
		t.Fatal("lost original diagnostic", fields)
	}
	run, err := s.Run(t.Context(), accepted.SessionID, accepted.RunID)
	if err != nil || run.Status.Terminal() {
		t.Fatal("persistence failure terminated run", run, err)
	}
}

func TestFailureIsLoggedEvenWhenTerminalSaveFails(t *testing.T) {
	for _, code := range []string{"harness_failed", "sandbox_lost"} {
		t.Run(code, func(t *testing.T) {
			s, remote, accepted, e := setup(t)
			log := failureLog(t)
			if code == "sandbox_lost" {
				tick(t, e)
				remote.state = "lost"
				remote.snapshotError = harness.ErrNotFound
			} else {
				remote.initializeError = rpcFailure()
			}
			_, err := s.Pool.Exec(t.Context(), `CREATE FUNCTION reject_terminal() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.status='failed' THEN RAISE EXCEPTION 'fixture'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_terminal BEFORE UPDATE ON runs FOR EACH ROW EXECUTE FUNCTION reject_terminal()`)
			if err != nil {
				t.Fatal(err)
			}
			err = e.Tick(t.Context())
			f, ok := errors.AsType[*harness.ExecutionError](err)
			if !ok || f.Code != code || strings.Contains(log.String(), `"event":"execution.failure"`) {
				t.Fatal("Tick must return the failure for central handling", err, log)
			}
			err = e.failure(t.Context(), f)
			if err == nil || diagnostic.Capture(err).ID != diagnostic.Capture(f).ID || strings.Count(log.String(), `"event":"execution.failure"`) != 1 {
				t.Fatal("lost or duplicated diagnostic on rollback", err, log)
			}
			run, err := s.Run(t.Context(), accepted.SessionID, accepted.RunID)
			if err != nil || run.Status.Terminal() {
				t.Fatal("failed transaction persisted a terminal run", run, err)
			}
		})
	}
}
