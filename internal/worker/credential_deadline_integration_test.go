//go:build integration

package worker

import (
	"strings"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus/internal/servicecredentials"
	"github.com/orpheus-agents/orpheus/internal/session"
)

func deadlineTokenFiles(box *remote) *int {
	removed := new(0)
	box.credentialCommands = func(command string) ([]byte, error) {
		if strings.Contains(command, "mktemp") {
			dir := ".kord"
			if strings.Contains(command, ".second") {
				dir = ".second"
			}
			return []byte("/home/template/" + dir + "/.run.fixture\n"), nil
		}
		if strings.Contains(command, "rm -f --") && strings.Contains(command, `"$d/run.token"`) {
			*removed++
		}
		return []byte("/home/template/workspace"), nil
	}
	return removed
}

func deadlineProviderConfig() servicecredentials.Config {
	return servicecredentials.Config{Endpoint: "https://provider.invalid", SecretEnv: "WORKER_PROVIDER_KEY", ContextFields: []string{"subject", "scope_context", "context_observed_at"}, TokenFile: "~/.kord/run.token"}
}

func TestCredentialDeadlinePersistsAfterPreparationRenewalAndRestart(t *testing.T) {
	s, box, a, e := setup(t)
	deadlineTokenFiles(box)
	now := time.Now().UTC().Truncate(time.Microsecond)
	// The invocation was issued before preparation. Its remaining absolute
	// lifetime, not a new twenty minutes from dispatch, constrains execution.
	grant := servicecredentials.Grant{CredentialID: "first", AccessToken: "fixture-first", ScopeKey: "first", ExpiresAt: now.Add(4 * time.Minute), RunExpiresAt: now.Add(15 * time.Minute)}
	state := workerCredentials(t, s, a, grant, deadlineProviderConfig())
	second := state.Entries["kord"]
	second.Config.TokenFile = "~/.second/run.token"
	second.Grant.CredentialID = "second"
	second.Grant.AccessToken = "fixture-second"
	second.Grant.RunExpiresAt = now.Add(10 * time.Minute)
	state.Entries["second"] = second
	if err := s.SaveServiceCredentials(t.Context(), a.SessionID, a.RunID, state); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	run, err := s.Run(t.Context(), a.SessionID, a.RunID)
	want := second.Grant.RunExpiresAt.Add(-s.Settings.CancelGrace).Add(-s.Settings.WorkerPoll)
	if err != nil || run.DeadlineAt == nil || !run.DeadlineAt.Equal(want) || box.starts != 1 {
		t.Fatal("provider deadline not persisted before dispatch", run, box.starts, err)
	}
	for code, entry := range state.Entries {
		entry.Grant.ExpiresAt = now.Add(5 * time.Minute)
		state.Entries[code] = entry
	}
	if err := s.SaveServiceCredentials(t.Context(), a.SessionID, a.RunID, state); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	e.Disconnect()
	e = executor(a.SessionID, s, box)
	t.Cleanup(e.Disconnect)
	// A changed worker setting must not move a deadline already dispatched.
	s.Settings.CancelGrace = time.Millisecond
	tick(t, e)
	run, err = s.Run(t.Context(), a.SessionID, a.RunID)
	if err != nil || run.DeadlineAt == nil || !run.DeadlineAt.Equal(want) || box.starts != 1 {
		t.Fatal("renewal or restart stretched execution", run, box.starts, err)
	}
}

func TestCredentialStopWindowPreventsDispatchAndCleansSandbox(t *testing.T) {
	for _, multiple := range []bool{true, false} {
		t.Run(map[bool]string{true: "reusable", false: "single_run"}[multiple], func(t *testing.T) {
			s, box, a, e := setupSession(t, multiple, session.SandboxInput{Template: "codex"}, nil)
			removed := deadlineTokenFiles(box)
			absolute := time.Now().UTC().Add(10 * time.Second)
			workerCredentials(t, s, a, servicecredentials.Grant{CredentialID: "fixture", AccessToken: "fixture", ScopeKey: "fixture", ExpiresAt: absolute, RunExpiresAt: absolute}, deadlineProviderConfig())
			tickUntil(t, e, func() bool {
				record, _, err := s.Read(t.Context(), a.SessionID)
				return err == nil && !record.SlotReserved
			})
			run, err := s.Run(t.Context(), a.SessionID, a.RunID)
			if err != nil || run.Status != session.Cancelled || run.StopReason == nil || *run.StopReason != "run_timeout" || run.ExecutionStartedAt != nil || box.starts != 0 || *removed == 0 {
				t.Fatal("exhausted stop window dispatched or leaked token", run, box.starts, *removed, err)
			}
			view, err := s.Session(t.Context(), a.SessionID)
			want := "deleted"
			if multiple {
				want = "paused"
			}
			if err != nil || view.Sandbox.State != want {
				t.Fatal("stop window did not clean up", view, err)
			}
		})
	}
}

func TestCredentialDeadlineUsesNormalCancellationAndCleanup(t *testing.T) {
	for _, multiple := range []bool{true, false} {
		for _, forced := range []bool{false, true} {
			name := map[bool]string{true: "reusable", false: "single_run"}[multiple] + "/" + map[bool]string{true: "forced", false: "graceful"}[forced]
			t.Run(name, func(t *testing.T) {
				s, box, a, e := setupSession(t, multiple, session.SandboxInput{Template: "codex"}, nil)
				removed := deadlineTokenFiles(box)
				now := time.Now().UTC().Truncate(time.Microsecond)
				absolute := now.Add(15 * time.Minute)
				workerCredentials(t, s, a, servicecredentials.Grant{CredentialID: "fixture", AccessToken: "fixture", ScopeKey: "fixture", ExpiresAt: now.Add(4 * time.Minute), RunExpiresAt: absolute}, deadlineProviderConfig())
				tick(t, e)
				run, err := s.Run(t.Context(), a.SessionID, a.RunID)
				if err != nil || run.DeadlineAt == nil || !run.DeadlineAt.Before(absolute) {
					t.Fatal("no cancellation window", run, err)
				}
				// Move only the test run's persisted deadline, avoiding a real wait.
				if _, err := s.Pool.Exec(t.Context(), "UPDATE runs SET deadline_at=now()-interval '1 second' WHERE id=$1", a.RunID); err != nil {
					t.Fatal(err)
				}
				tick(t, e)
				run, err = s.Run(t.Context(), a.SessionID, a.RunID)
				if err != nil || run.Status != session.Cancelling || run.StopReason == nil || *run.StopReason != "run_timeout" || box.cancels != 1 || run.CancelRequestedAt == nil || !run.CancelRequestedAt.Before(absolute) {
					t.Fatal("did not cancel before provider deadline", run, box.cancels, err)
				}
				if forced {
					if _, err := s.Pool.Exec(t.Context(), "UPDATE runs SET cancel_attempted_at=now()-interval '1 minute' WHERE id=$1", a.RunID); err != nil {
						t.Fatal(err)
					}
				} else {
					box.turns[0].Status = session.Cancelled
				}
				tickUntil(t, e, func() bool {
					record, _, err := s.Read(t.Context(), a.SessionID)
					return err == nil && !record.SlotReserved
				})
				run, err = s.Run(t.Context(), a.SessionID, a.RunID)
				method := map[bool]string{true: "forced", false: "graceful"}[forced]
				if err != nil || run.Status != session.Cancelled || run.Error != nil || run.StopMethod == nil || *run.StopMethod != method || *removed == 0 {
					t.Fatal("credential deadline broke cancellation", run, *removed, err)
				}
				view, err := s.Session(t.Context(), a.SessionID)
				want := map[bool]string{true: "paused", false: "deleted"}[multiple]
				if err != nil || view.Sandbox.State != want {
					t.Fatal("credential deadline left unavailable sandbox", view, err)
				}
			})
		}
	}
}
