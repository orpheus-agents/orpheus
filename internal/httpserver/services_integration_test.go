//go:build integration

package httpserver

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/session"
)

func TestServiceRequests(t *testing.T) {
	server, s := testServer(t)
	s.Profiles.Services = map[string]config.Service{"gitlab": {Name: "GitLab", Description: "Repositories", EnvFrom: []string{"GITLAB_TOKEN"}}}
	for _, tc := range []struct {
		body string
		path []any
	}{
		{strings.Replace(validBody, `"messages":`, `"services":["missing"],"messages":`, 1), []any{"body", "services", float64(0)}},
		{strings.Replace(validBody, `"sandbox":{`, `"sandbox":{"services":["missing"],`, 1), []any{"body", "configuration", "sandbox", "services", float64(0)}},
	} {
		raw := externalRequest(t, server, "POST", "/api/v1/sessions", tc.body, uuid.NewString(), 422)
		assertServiceProblem(t, raw, "unknown_service", tc.path)
	}
	for _, tc := range []struct {
		body string
		path []any
	}{
		{strings.Replace(validBody, `"messages":`, `"env":{"GITLAB_TOKEN":"literal"},"services":["gitlab"],"messages":`, 1), []any{"body", "services", float64(0)}},
		{strings.Replace(validBody, `"sandbox":{`, `"sandbox":{"env":{"GITLAB_TOKEN":"literal"},"services":["gitlab"],`, 1), []any{"body", "configuration", "sandbox", "services", float64(0)}},
	} {
		raw := externalRequest(t, server, "POST", "/api/v1/sessions", tc.body, uuid.NewString(), 422)
		assertServiceProblem(t, raw, "validation_error", tc.path)
	}
	for _, value := range []string{`null`, `"gitlab"`, `["gitlab","gitlab"]`, `[""]`, `["BAD"]`, `["missing"]`} {
		for _, body := range []string{
			strings.Replace(validBody, `"messages":`, `"services":`+value+`,"messages":`, 1),
			strings.Replace(validBody, `"sandbox":{`, `"sandbox":{"services":`+value+`,`, 1),
		} {
			externalRequest(t, server, "POST", "/api/v1/sessions", body, uuid.NewString(), 422)
		}
	}
	var count int
	if err := s.Pool.QueryRow(t.Context(), "SELECT count(*) FROM sessions").Scan(&count); err != nil || count != 0 {
		t.Fatal("rejected admission persisted", count, err)
	}
	body := strings.Replace(validBody, `"sandbox":{`, `"sandbox":{"services":["gitlab"],`, 1)
	body = strings.Replace(body, `"messages":`, `"services":["gitlab"],"messages":`, 1)
	a := decodeHTTP[session.Acceptance](t, externalRequest(t, server, "POST", "/api/v1/sessions", body, uuid.NewString(), 202))
	base := "/api/v1/sessions/" + a.SessionID.String()
	raw := externalRequest(t, server, "POST", base+"/runs", `{"messages":[{"text":"next"}],"services":["missing"]}`, uuid.NewString(), 422)
	assertServiceProblem(t, raw, "unknown_service", []any{"body", "services", float64(0)})
	raw = externalRequest(t, server, "POST", base+"/runs", `{"messages":[{"text":"next"}],"env":{"GITLAB_TOKEN":"literal"},"services":["gitlab"]}`, uuid.NewString(), 422)
	assertServiceProblem(t, raw, "validation_error", []any{"body", "services", float64(0)})
	sess := decodeHTTP[session.Session](t, externalRequest(t, server, "GET", base, "", "", 200))
	if len(sess.Configuration.Sandbox.Services) != 1 || sess.Configuration.Sandbox.Services[0].Code != "gitlab" {
		t.Fatal(sess.Configuration)
	}
	run := decodeHTTP[session.Run](t, externalRequest(t, server, "GET", base+"/runs/"+a.RunID.String(), "", "", 200))
	if len(run.Services) != 1 || run.Services[0].Code != "gitlab" {
		t.Fatal(run)
	}
	for _, value := range []string{`null`, `["gitlab","gitlab"]`, `["missing"]`} {
		externalRequest(t, server, "POST", base+"/runs", `{"messages":[{"text":"next"}],"services":`+value+`}`, uuid.NewString(), 422)
	}
	externalRequest(t, server, "POST", base+"/runs/"+a.RunID.String()+"/messages", `{"messages":[{"text":"clarify"}],"services":[]}`, uuid.NewString(), 422)
	if _, err := s.Cancel(t.Context(), a.SessionID, a.RunID); err != nil {
		t.Fatal(err)
	}
	next := decodeHTTP[session.Acceptance](t, externalRequest(t, server, "POST", base+"/runs", `{"messages":[{"text":"next"}],"services":["gitlab"]}`, uuid.NewString(), 202))
	run = decodeHTTP[session.Run](t, externalRequest(t, server, "GET", base+"/runs/"+next.RunID.String(), "", "", 200))
	if len(run.Services) != 1 || run.Services[0].Code != "gitlab" {
		t.Fatal(run)
	}
}

func assertServiceProblem(t *testing.T, raw []byte, code string, path []any) {
	t.Helper()
	var body struct{ Error session.Error }
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != code || len(body.Error.Details) != 1 || !reflect.DeepEqual(body.Error.Details[0].Path, path) {
		t.Fatalf("expected %s at %v, got %s", code, path, raw)
	}
}
