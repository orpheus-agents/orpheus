//go:build integration

package httpserver

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/session"
)

func TestCatalogValidationAndExistingSessions(t *testing.T) {
	server, storage := testServer(t)
	for _, path := range []string{"/api/v1/profiles", "/api/v1/templates"} {
		// requestHTTP validates successful responses against OpenAPI.
		externalRequest(t, server, "GET", path, "", "", 200)
	}
	for _, tc := range []struct{ body, code, field string }{
		{strings.Replace(externalBody, "\"template\":\"codex\"", "\"template\":\"missing\"", 1), "unknown_template", "sandbox"},
		{strings.Replace(externalBody, "\"profile\":\"default\"", "\"profile\":\"missing\"", 1), "unknown_profile", "agent"},
	} {
		raw := externalRequest(t, server, "POST", "/api/v1/sessions", tc.body, uuid.NewString(), 422)
		var response struct{ Error session.Error }
		if err := json.Unmarshal(raw, &response); err != nil {
			t.Fatal(err)
		}
		leaf := "template"
		if tc.field == "agent" {
			leaf = "profile"
		}
		want := []session.Detail{{Path: []any{"body", "configuration", tc.field, leaf}, Code: "invalid_value"}}
		if response.Error.Code != tc.code || !reflect.DeepEqual(response.Error.Details, want) {
			t.Fatalf("%s", raw)
		}
	}
	key := uuid.NewString()
	first := decodeHTTP[session.Acceptance](t, externalRequest(t, server, "POST", "/api/v1/sessions", externalBody, key, 202))
	// Simulate a restart after removing both entries from the configuration.
	delete(storage.Profiles.Profiles, "default")
	delete(storage.Profiles.Templates, "codex")
	again := decodeHTTP[session.Acceptance](t, externalRequest(t, server, "POST", "/api/v1/sessions", externalBody, key, 202))
	if again != first {
		t.Fatal("replay changed accepted run", again, first)
	}
	record, _, err := storage.Read(t.Context(), first.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Configuration.Public.Agent.Model != "fixture" || record.Configuration.Public.Sandbox.Template != "codex" {
		t.Fatal("stored execution configuration changed")
	}
	if _, err := storage.Cancel(t.Context(), first.SessionID, first.RunID); err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/sessions/" + first.SessionID.String()
	next := decodeHTTP[session.Acceptance](t, externalRequest(t, server, "POST", base+"/runs", "{\"messages\":[{\"text\":\"continue\"}]}", uuid.NewString(), 202))
	if next.SessionID != first.SessionID || next.RunID == first.RunID {
		t.Fatal(next)
	}
	externalRequest(t, server, "GET", base, "", "", 200)
}
