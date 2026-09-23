package config

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skillum-ai/orpheus/internal/secret"
	"github.com/skillum-ai/orpheus/internal/session"
)

func TestResolveHooks(t *testing.T) {
	profiles := Profiles{Profiles: map[string]Profile{"default": {Harness: "codex", Model: new("model"), Auth: Auth{Mode: "api_key", APIKeyEnv: "KEY"}}}}
	input := session.ConfigurationInput{Agent: session.AgentInput{Profile: "default"}, Sandbox: session.SandboxInput{Template: "template"}, Limits: session.Limits{RunTimeoutSeconds: 3600}}
	got, err := Resolve(input, profiles, nil)
	if err != nil || got.Public.Hooks.TimeoutSeconds != 300 {
		t.Fatal(got, err)
	}
	input.Hooks = &session.HooksInput{AfterCreate: new("#!/bin/sh\nprintf ready\n"), BeforeRemove: new("#!/usr/bin/env python3\npass\n"), TimeoutSeconds: new(45)}
	got, err = Resolve(input, profiles, nil)
	if err != nil || got.Public.Hooks.TimeoutSeconds != 45 || got.Public.Hooks.AfterCreate == nil || got.Public.Hooks.BeforeRemove == nil {
		t.Fatal(got, err)
	}
	for _, script := range []string{"", "echo without shebang", "#!\n", "#!/bin/sh\x00", strings.Repeat("a", 65537)} {
		input.Hooks.AfterCreate = &script
		if _, err := Resolve(input, profiles, nil); err == nil {
			t.Fatalf("accepted invalid hook %q", script[:min(len(script), 30)])
		}
	}
	input.Hooks.AfterCreate = new("#!/bin/sh\n")
	input.Hooks.TimeoutSeconds = new(0)
	if _, err := Resolve(input, profiles, nil); err == nil {
		t.Fatal("accepted zero hook timeout")
	}
}

func TestMergedEnvironmentDoesNotResolveOverriddenSources(t *testing.T) {
	cipher, err := secret.New(base64.URLEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	cfg := session.Configuration{Sandbox: session.SandboxConfiguration{Template: "template", EnvFrom: []string{"SESSION_TOKEN"}}}
	env, err := MergedEnvironment(cipher, id, nil, cfg, nil, map[string]string{"SESSION_TOKEN": "from-run"}, nil)
	if err != nil || env["SESSION_TOKEN"] != "from-run" {
		t.Fatalf("overridden session reference was resolved: %v, %v", env, err)
	}

	sealed, err := cipher.Encrypt(id, map[string]string{"RUN_TOKEN": "from-session"})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("RUN_TOKEN", "from-run-reference")
	cfg.Sandbox.EnvFrom = nil
	env, err = MergedEnvironment(cipher, id, sealed, cfg, []string{"RUN_TOKEN"}, nil, []string{"RUN_TOKEN"})
	if err != nil || env["RUN_TOKEN"] != "from-run-reference" {
		t.Fatalf("run reference did not override session value: %v, %v", env, err)
	}
}
