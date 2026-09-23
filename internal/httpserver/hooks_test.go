//go:build integration

package httpserver

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skillum-ai/orpheus/internal/session"
)

func TestHooksHTTPContract(t *testing.T) {
	server, _ := testServer(t)
	body := `{"configuration":{"agent":{"profile":"default"},"sandbox":{"template":"codex"},"hooks":{"after_create":"#!/bin/sh\nprintf ready\n","before_run":"#!/bin/sh\nprintf before\n","after_run":"#!/bin/sh\nprintf after\n","before_remove":"#!/bin/sh\nprintf remove\n"}},"message":{"text":"task"}}`
	key := uuid.NewString()
	a := decodeHTTP[session.Acceptance](t, externalRequest(t, server, "POST", "/api/v1/sessions", body, key, 202))
	base := "/api/v1/sessions/" + a.SessionID.String()
	view := decodeHTTP[session.Session](t, externalRequest(t, server, "GET", base, "", "", 200))
	if view.Configuration.Hooks.TimeoutSeconds != 300 || view.Configuration.Hooks.AfterCreate == nil || !strings.Contains(*view.Configuration.Hooks.AfterCreate, "printf ready") || view.Configuration.Hooks.BeforeRemove == nil {
		t.Fatal(view.Configuration.Hooks)
	}
	run := decodeHTTP[session.Run](t, externalRequest(t, server, "GET", base+"/runs/"+a.RunID.String(), "", "", 200))
	if len(run.Hooks) != 3 || run.Hooks[0].Name != "after_create" || run.Hooks[0].Status != "pending" || run.Hooks[2].Name != "after_run" {
		t.Fatal(run.Hooks)
	}
	externalRequest(t, server, "POST", "/api/v1/sessions", strings.Replace(body, "printf ready", "printf changed", 1), key, 409)
	for _, bad := range []string{
		strings.Replace(body, `"#!/bin/sh\nprintf ready\n"`, `"echo no shebang"`, 1),
		strings.Replace(body, `"#!/bin/sh\nprintf ready\n"`, `null`, 1),
		strings.Replace(body, `"after_create":`, `"unknown":"x","after_create":`, 1),
		strings.Replace(body, `"after_run":`, `"timeout_seconds":0,"after_run":`, 1),
	} {
		if bad == body {
			continue
		}
		externalRequest(t, server, "POST", "/api/v1/sessions", bad, uuid.NewString(), 422)
	}
	externalRequest(t, server, "POST", base+"/runs/"+a.RunID.String()+"/cancel", "", "", 200)
	run = decodeHTTP[session.Run](t, externalRequest(t, server, "GET", base+"/runs/"+a.RunID.String(), "", "", 200))
	if run.Status != session.Cancelled || len(run.Hooks) != 3 || run.Hooks[0].Status != "skipped" || run.Hooks[2].Status != "skipped" {
		t.Fatal(run)
	}
}
