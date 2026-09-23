//go:build integration

package store

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/session"
)

func TestRunEnvironmentAdmissionAndReplay(t *testing.T) {
	s := fixture(t)
	s.Settings.HarnessEnvAllowlist = []string{"MISSING_A", "MISSING_B"}
	req := request()
	req.Create.Env = map[string]string{"TASK_ID": "hidden-run-secret", "TOKEN": "run-override", "EMPTY": ""}
	req.Create.EnvFrom = []string{"MISSING_B", "MISSING_A"}
	a, err := s.Accept(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	replay := request()
	replay.Key = req.Key
	replay.Create.Env = map[string]string{"EMPTY": "", "TOKEN": "run-override", "TASK_ID": "hidden-run-secret"}
	replay.Create.EnvFrom = []string{"MISSING_A", "MISSING_B"}
	again, err := s.Accept(t.Context(), replay)
	if err != nil || again != a {
		t.Fatal(again, err)
	}
	s.Settings.HarnessEnvAllowlist = nil
	again, err = s.Accept(t.Context(), replay)
	if err != nil || again != a {
		t.Fatal("replay rejected after allowlist changed", again, err)
	}
	s.Settings.HarnessEnvAllowlist = []string{"MISSING_A", "MISSING_B"}
	run, err := s.Run(t.Context(), a.SessionID, a.RunID)
	if err != nil || !slices.Equal(run.EnvNames, []string{"EMPTY", "TASK_ID", "TOKEN"}) || !slices.Equal(run.EnvFrom, []string{"MISSING_A", "MISSING_B"}) {
		t.Fatal(run, err)
	}
	var ciphertext *string
	if err := s.Pool.QueryRow(t.Context(), "SELECT env_ciphertext FROM runs WHERE id=$1", a.RunID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if ciphertext == nil || strings.Contains(*ciphertext, "hidden-run-secret") || strings.Contains(*ciphertext, "run-override") {
		t.Fatal("run environment was not encrypted")
	}
	decrypted, err := s.Cipher.DecryptRun(a.SessionID, a.RunID, ciphertext)
	if err != nil || !maps.Equal(decrypted, req.Create.Env) {
		t.Fatal(decrypted, err)
	}
	// The harness still gets the session environment, even for overridden names.
	sessionEnv, err := config.Environment(s.Cipher, a.SessionID, mustSession(t, s, a.SessionID).EnvCiphertext, mustSession(t, s, a.SessionID).Configuration.Public, s.Settings.HarnessEnvAllowlist)
	if err != nil || sessionEnv["TOKEN"] != "private" {
		t.Fatal(sessionEnv, err)
	}
	if _, ok := sessionEnv["TASK_ID"]; ok {
		t.Fatal("run environment leaked to the harness")
	}
	events, err := s.Events(t.Context(), a.SessionID, "0", 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events.Items {
		if strings.Contains(string(event.Data), "hidden-run-secret") || strings.Contains(string(event.Data), "run-override") || strings.Contains(string(event.Data), "private") {
			t.Fatal("secret leaked in event", string(event.Data))
		}
	}
	replay.Create.Env["TASK_ID"] = "changed-secret"
	_, err = s.Accept(t.Context(), replay)
	requireCode(t, err, "idempotency_conflict")
	replay.Create.Env["TASK_ID"] = "hidden-run-secret"
	replay.Create.EnvFrom = []string{"MISSING_A"}
	_, err = s.Accept(t.Context(), replay)
	requireCode(t, err, "idempotency_conflict")
	if _, err := s.Cancel(t.Context(), a.SessionID, a.RunID); err != nil {
		t.Fatal(err)
	}
	next, err := s.Accept(t.Context(), Admission{SessionID: a.SessionID, Key: uuid.New(), Text: "next"})
	if err != nil {
		t.Fatal(err)
	}
	nextRun, err := s.Run(t.Context(), a.SessionID, next.RunID)
	if err != nil || len(nextRun.EnvNames) != 0 || len(nextRun.EnvFrom) != 0 || nextRun.EnvNames == nil || nextRun.EnvFrom == nil {
		t.Fatal(nextRun, err)
	}
	if nextRun.Status != session.Accepted {
		t.Fatal(nextRun.Status)
	}
}

func mustSession(t *testing.T, s *Store, id uuid.UUID) SessionRecord {
	t.Helper()
	r, err := GetSession(t.Context(), s.Pool, id, false)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
