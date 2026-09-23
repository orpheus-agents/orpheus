//go:build integration

package httpserver

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/session"
)

func TestRunEnvironmentHTTP(t *testing.T) {
	server, s := testServer(t)
	s.Settings.HarnessEnvAllowlist = []string{"FROM_A", "FROM_B"}
	body := strings.Replace(validBody, `"message":`, `"env":{"TASK_ID":"hidden-run-secret"},"env_from":["FROM_B","FROM_A"],"message":`, 1)
	key := uuid.NewString()
	a := decodeHTTP[session.Acceptance](t, externalRequest(t, server, "POST", "/api/v1/sessions", body, key, 202))
	base := "/api/v1/sessions/" + a.SessionID.String()
	path := base + "/runs/" + a.RunID.String()
	run := decodeHTTP[session.Run](t, externalRequest(t, server, "GET", path, "", "", 200))
	if !slices.Equal(run.EnvNames, []string{"TASK_ID"}) || !slices.Equal(run.EnvFrom, []string{"FROM_A", "FROM_B"}) {
		t.Fatal(run)
	}
	for _, endpoint := range []string{path, base + "/runs", base + "/events", "/api/v1/runs"} {
		raw := externalRequest(t, server, "GET", endpoint, "", "", 200)
		if strings.Contains(string(raw), "hidden-run-secret") || !strings.Contains(string(raw), `"env_names"`) {
			t.Fatal(endpoint, string(raw))
		}
	}
	reordered := strings.Replace(body, `["FROM_B","FROM_A"]`, `["FROM_A","FROM_B"]`, 1)
	again := decodeHTTP[session.Acceptance](t, externalRequest(t, server, "POST", "/api/v1/sessions", reordered, key, 202))
	if again != a {
		t.Fatal(again)
	}
	externalRequest(t, server, "POST", "/api/v1/sessions", strings.Replace(body, "hidden-run-secret", "changed-secret", 1), key, 409)
	for _, invalid := range []string{
		strings.Replace(body, `"env":{"TASK_ID":"hidden-run-secret"}`, `"env":null`, 1),
		strings.Replace(body, `"TASK_ID"`, `"ORPHEUS_RUN_ID"`, 1),
		strings.Replace(body, `["FROM_B","FROM_A"]`, `["FROM_B","FROM_B"]`, 1),
		strings.Replace(body, `["FROM_B","FROM_A"]`, `["NOT_ALLOWED"]`, 1),
	} {
		externalRequest(t, server, "POST", "/api/v1/sessions", invalid, uuid.NewString(), 422)
	}
	if _, err := s.Cancel(t.Context(), a.SessionID, a.RunID); err != nil {
		t.Fatal(err)
	}
	nextBody := `{"message":{"text":"next"},"env":{"TASK_ID":"next-secret"},"env_from":["FROM_A"]}`
	nextKey := uuid.NewString()
	next := decodeHTTP[session.Acceptance](t, externalRequest(t, server, "POST", base+"/runs", nextBody, nextKey, 202))
	nextRun := decodeHTTP[session.Run](t, externalRequest(t, server, "GET", base+"/runs/"+next.RunID.String(), "", "", 200))
	if !slices.Equal(nextRun.EnvNames, []string{"TASK_ID"}) || !slices.Equal(nextRun.EnvFrom, []string{"FROM_A"}) {
		t.Fatal(nextRun)
	}
	externalRequest(t, server, "POST", base+"/runs", nextBody, nextKey, 202)
	externalRequest(t, server, "POST", base+"/runs", strings.Replace(nextBody, "next-secret", "changed-secret", 1), nextKey, 409)
	steerBody := `{"message":{"text":"clarify"},"env":{"TASK_ID":"no"}}`
	externalRequest(t, server, "POST", base+"/runs/"+next.RunID.String()+"/messages", steerBody, uuid.NewString(), 422)
}
