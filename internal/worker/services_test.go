//go:build integration

package worker

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
)

func TestServiceEnvironmentScope(t *testing.T) {
	s, box, initial, e := setupHooks(t, true, false)
	if _, err := s.Cancel(t.Context(), initial.SessionID, initial.RunID); err != nil {
		t.Fatal(err)
	}
	s.Deployment.Services = map[string]config.Service{
		"session": {Name: "Session", Description: "Agent service", Credentials: map[string]config.DirectCredential{"session_service_env": {Env: "SESSION_SERVICE_ENV"}}},
		"run":     {Name: "Run", Description: "Hook service", Credentials: map[string]config.DirectCredential{"run_service_env": {Env: "RUN_SERVICE_ENV"}}},
	}
	t.Setenv("SESSION_SERVICE_ENV", "session-value")
	t.Setenv("RUN_SERVICE_ENV", "run-value")
	a, err := s.Accept(t.Context(), store.Admission{Key: uuid.New(), Create: &session.CreateSession{
		Configuration: session.ConfigurationInput{Agent: session.AgentInput{Profile: "p"}, Sandbox: session.SandboxInput{Template: "codex", Services: []string{"session"}}, Limits: session.Limits{RunTimeoutSeconds: 3600}, Hooks: &session.HooksInput{AfterCreate: new("#!/bin/sh\n# after_create\n"), BeforeRun: new("#!/bin/sh\n# before_run\n"), AfterRun: new("#!/bin/sh\n# after_run\n"), BeforeRemove: new("#!/bin/sh\n# before_remove\n")}},
		Messages:      []session.TextMessage{{Text: "task"}}, Services: []string{"run"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	e.ID = a.SessionID
	tickUntil(t, e, func() bool { return box.starts == 1 })
	if box.env["SESSION_SERVICE_ENV"] != "session-value" || box.env["RUN_SERVICE_ENV"] != "" {
		t.Fatal("harness services", box.env)
	}
	if len(box.hookEnv) != 2 || box.hookEnv[0]["SESSION_SERVICE_ENV"] != "session-value" || box.hookEnv[0]["RUN_SERVICE_ENV"] != "" || box.hookEnv[1]["RUN_SERVICE_ENV"] != "run-value" {
		t.Fatal("hook services", box.hookEnv)
	}
	complete(box.remote)
	tickUntil(t, e, func() bool { return len(box.hookEnv) == 3 })
	if box.hookEnv[2]["RUN_SERVICE_ENV"] != "run-value" {
		t.Fatal("final hook services", box.hookEnv)
	}
	// before_remove is reserved but not activated by current lifecycle cleanup.
	// Its environment builder must still exclude run-only services.
	record, err := store.GetSession(t.Context(), s.Pool, a.SessionID, false)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.GetRun(t.Context(), s.Pool, a.SessionID, a.RunID)
	if err != nil {
		t.Fatal(err)
	}
	env, err := e.hookEnvironment(t.Context(), record, run, "before_remove")
	if err != nil || env["RUN_SERVICE_ENV"] != "" || env["SESSION_SERVICE_ENV"] != "session-value" {
		t.Fatal(env, err)
	}
}

func TestHarnessEnvironmentHistoryAcrossLaunches(t *testing.T) {
	s, r, a, e := setupWithEnvironment(t, session.SandboxInput{Template: "codex", Env: map[string]string{"SESSION": "private"}}, nil)
	first, err := s.Run(t.Context(), a.SessionID, a.RunID)
	if err != nil || first.HarnessEnvNames != nil {
		t.Fatal(first, err)
	}
	runs := []uuid.UUID{a.RunID}
	var expected [][]string
	for n := range 3 {
		if n > 0 {
			complete(r)
			tick(t, e)
			tick(t, e)
			e.Disconnect()
			next, err := s.Accept(t.Context(), store.Admission{SessionID: a.SessionID, Key: uuid.New(), Messages: []session.TextMessage{{Text: "continue"}}})
			if err != nil {
				t.Fatal(err)
			}
			runs = append(runs, next.RunID)
		}
		if n == 2 {
			r.processes = nil
			s.Settings.SandboxProxyURL = ""
		}
		tick(t, e)
		expected = append(expected, slices.Sorted(maps.Keys(r.env)))
	}
	if r.launches != 2 || !slices.Equal(expected[0], expected[1]) || slices.Equal(expected[0], expected[2]) {
		t.Fatal(r.launches, expected)
	}
	page, err := s.ListRuns(t.Context(), a.SessionID, 10, "", store.ListFilter{})
	if err != nil || len(page.Items) != 3 {
		t.Fatal(page, err)
	}
	all, err := s.ListAllRuns(t.Context(), 10, "", store.ListFilter{})
	if err != nil || len(all.Items) != 3 {
		t.Fatal(all, err)
	}
	events, err := s.Events(t.Context(), a.SessionID, "0", 200)
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range runs {
		check := func(view session.Run) {
			t.Helper()
			if view.ID != id || !slices.Equal(view.HarnessEnvNames, expected[i]) {
				t.Fatal(view.ID, view.HarnessEnvNames, expected[i])
			}
		}
		current, err := s.Run(t.Context(), a.SessionID, id)
		if err != nil {
			t.Fatal(err)
		}
		check(current)
		idx := slices.IndexFunc(page.Items, func(v session.Run) bool { return v.ID == id })
		if idx < 0 {
			t.Fatal(id)
		}
		check(page.Items[idx])
		idx = slices.IndexFunc(all.Items, func(v session.Run) bool { return v.ID == id })
		if idx < 0 {
			t.Fatal(id)
		}
		check(all.Items[idx])
		var last session.Run
		sawUnbound := false
		for _, event := range events.Items {
			if event.Type == "run.updated" {
				var v session.Run
				if err := json.Unmarshal(event.Data, &v); err != nil {
					t.Fatal(err)
				}
				if v.ID == id {
					if v.HarnessEnvNames == nil {
						sawUnbound = true
					}
					last = v
				}
			}
		}
		if !sawUnbound {
			t.Fatal("missing original unbound event", id)
		}
		check(last)
	}
}
