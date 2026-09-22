//go:build integration

package httpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skillum-ai/orpheus/internal/session"
	"github.com/skillum-ai/orpheus/internal/store"
)

func TestSandboxAccessHTTP(t *testing.T) {
	const agentBoxKey = "private-agentbox-key-must-not-appear-in-api"
	t.Setenv("AGENTBOX_API_KEY", agentBoxKey)
	server, s := testServer(t)
	a := decodeHTTP[session.Acceptance](t, externalRequest(t, server, "POST", "/api/v1/sessions", validBody, uuid.NewString(), 202))
	path := "/api/v1/sessions/" + a.SessionID.String()
	initial := decodeHTTP[session.Session](t, externalRequest(t, server, "GET", path, "", "", 200))
	if initial.Sandbox.ID != nil || initial.Sandbox.Workspace != nil {
		t.Fatal(initial.Sandbox)
	}
	if err := s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, record *store.SessionRecord) error {
		record.SandboxID = new("agentbox-123")
		record.Workspace = new("/home/user/workspace")
		return store.Emit(t.Context(), tx, record, "sandbox.updated", record.Sandbox())
	}); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{path, "/api/v1/sessions", path + "/events"} {
		raw := externalRequest(t, server, "GET", endpoint, "", "", 200)
		if !strings.Contains(string(raw), `"id":"agentbox-123"`) || !strings.Contains(string(raw), `"workspace":"/home/user/workspace"`) {
			t.Fatalf("%s: sandbox address missing: %s", endpoint, raw)
		}
		if strings.Contains(string(raw), agentBoxKey) {
			t.Fatalf("%s: credentials exposed: %s", endpoint, raw)
		}
	}
	events := decodeHTTP[session.EventPage](t, externalRequest(t, server, "GET", path+"/events", "", "", 200))
	var found bool
	for _, event := range events.Items {
		if event.Type != "sandbox.updated" {
			continue
		}
		var state session.SandboxState
		if err := json.Unmarshal(event.Data, &state); err != nil {
			t.Fatal(err)
		}
		if state.ID != nil && state.Workspace != nil {
			found = *state.ID == "agentbox-123" && *state.Workspace == "/home/user/workspace"
		}
	}
	if !found {
		t.Fatal("sandbox event missing", events)
	}
}
