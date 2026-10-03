//go:build integration

package worker

import (
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
	s.Profiles.Services = map[string]config.Service{
		"session": {Name: "Session", Description: "Agent service", EnvFrom: []string{"SESSION_SERVICE_ENV"}},
		"run":     {Name: "Run", Description: "Hook service", EnvFrom: []string{"RUN_SERVICE_ENV"}},
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
	env, err := e.hookEnvironment(record, run, "before_remove")
	if err != nil || env["RUN_SERVICE_ENV"] != "" || env["SESSION_SERVICE_ENV"] != "session-value" {
		t.Fatal(env, err)
	}
}
