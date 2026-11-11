//go:build integration

package worker

import (
	"errors"
	"testing"

	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/session"
)

func TestSecretSourcesReachHarnessAndHooks(t *testing.T) {
	_, box, _, e := setupHooks(t, true, false, map[string]string{"TOKEN": "run-value", "TASK_ID": "42"})
	tickUntil(t, e, func() bool { return box.starts == 1 })
	if box.env["TOKEN"] != "session-value" || len(box.hookEnv) != 2 || box.hookEnv[0]["TOKEN"] != "session-value" || box.hookEnv[1]["TOKEN"] != "run-value" || box.hookEnv[1]["TASK_ID"] != "42" {
		t.Fatal("incorrect environment scope")
	}
	if _, ok := box.env["TASK_ID"]; ok {
		t.Fatal("run env leaked to harness")
	}
	complete(box.remote)
	tickUntil(t, e, func() bool { return len(box.hookEnv) == 3 })
	if box.hookEnv[2]["TOKEN"] != "run-value" {
		t.Fatal("after_run lost environment")
	}
}

func TestCorruptSecretPreventsExecution(t *testing.T) {
	for _, table := range []string{"sessions", "runs"} {
		t.Run(table, func(t *testing.T) {
			s, box, a, e := setupHooks(t, true, false, map[string]string{"TOKEN": "run-value"})
			id := a.SessionID
			if table == "runs" {
				id = a.RunID
			}
			if _, err := s.Pool.Exec(t.Context(), "UPDATE secrets SET ciphertext=set_byte(ciphertext,0,get_byte(ciphertext,0)#1) WHERE id=(SELECT env_secret_id FROM "+table+" WHERE id=$1)", id); err != nil {
				t.Fatal(err)
			}
			tickUntil(t, e, func() bool {
				run, err := s.Run(t.Context(), a.SessionID, a.RunID)
				return err == nil && run.Status.Terminal()
			})
			run, err := s.Run(t.Context(), a.SessionID, a.RunID)
			if err != nil || run.Status != session.Failed || box.starts != 0 || box.launches != 0 {
				t.Fatal("corrupt secret allowed execution", err)
			}
		})
	}
}

func TestSecretReadFailureRetriesHarness(t *testing.T) {
	s, box, a, e := setupWithEnvironment(t, session.SandboxInput{Template: "codex", Env: map[string]string{"TOKEN": "private"}}, nil)
	if _, err := s.Pool.Exec(t.Context(), "ALTER TABLE secrets RENAME TO secrets_off"); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if err := e.Tick(t.Context()); !errors.Is(err, errStoreAccess) {
			t.Fatal("expected retryable store error", err)
		}
	}
	run, err := s.Run(t.Context(), a.SessionID, a.RunID)
	if err != nil || run.Status.Terminal() || box.starts != 0 || box.launches != 0 {
		t.Fatal("transient read failure ended run", err)
	}
	if _, err := s.Pool.Exec(t.Context(), "ALTER TABLE secrets_off RENAME TO secrets"); err != nil {
		t.Fatal(err)
	}
	tickUntil(t, e, func() bool { return box.starts == 1 })
	if box.env["TOKEN"] != "private" {
		t.Fatal("recovery lost environment")
	}
}

func TestSecretReadFailureRetriesHookWithoutConsumingAttempts(t *testing.T) {
	for _, retry := range []bool{false, true} {
		name := "initial"
		if retry {
			name = "retry"
		}
		t.Run(name, func(t *testing.T) {
			s, box, a, e := setupHooks(t, false, false, map[string]string{"TOKEN": "run-private"})
			tickUntil(t, e, func() bool { return len(box.hookEnv) == 1 })
			box.completeHook(box.hookEnv[0]["ORPHEUS_HOOK_OPERATION_ID"], box.invoked[0])
			box.autoResult = true
			wantAttempts := 0
			if retry {
				box.lostHookStart = true
				if err := e.Tick(t.Context()); !errors.Is(err, harness.ErrUncertain) {
					t.Fatal("expected uncertain start", err)
				}
				wantAttempts = 1
			}
			if _, err := s.Pool.Exec(t.Context(), "ALTER TABLE secrets RENAME TO secrets_off"); err != nil {
				t.Fatal(err)
			}
			for range 5 {
				if err := e.Tick(t.Context()); !errors.Is(err, errStoreAccess) {
					t.Fatal("expected retryable store error", err)
				}
			}
			run, err := s.Run(t.Context(), a.SessionID, a.RunID)
			if err != nil || run.Status.Terminal() || box.starts != 0 {
				t.Fatal("transient failure ended run", err)
			}
			h, err := s.Hook(t.Context(), a.RunID, "before_run")
			if err != nil || h == nil || h.StartAttempts != wantAttempts || h.Status == "failed" {
				t.Fatal("DB read consumed hook attempt", h, err)
			}
			if _, err := s.Pool.Exec(t.Context(), "ALTER TABLE secrets_off RENAME TO secrets"); err != nil {
				t.Fatal(err)
			}
			tickUntil(t, e, func() bool { return box.starts == 1 })
			if box.hookEnv[1]["TOKEN"] != "run-private" {
				t.Fatal("hook recovery lost environment")
			}
		})
	}
}
