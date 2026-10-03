package config

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/secret"
	"github.com/orpheus-agents/orpheus/internal/session"
)

const serviceProfiles = `[templates.codex]
[profiles.default]
harness = "codex"
model = "model"
[profiles.default.auth]
mode = "api_key"
api_key_env = "OPENAI_API_KEY"
`

func TestServiceConfiguration(t *testing.T) {
	const service = "[services.gitlab]\nname = ' GitLab '\ndescription = ' Repositories '\nenv_from = ['TOKEN_B', 'TOKEN_A']\n"
	p, err := ReadProfiles(strings.NewReader(serviceProfiles + service))
	if err != nil || p.Services["gitlab"].Name != "GitLab" || p.Services["gitlab"].Description != "Repositories" || !slices.Equal(p.Services["gitlab"].EnvFrom, []string{"TOKEN_A", "TOKEN_B"}) {
		t.Fatal(p.Services, err)
	}
	for _, invalid := range []string{
		strings.Replace(service, "services.gitlab", "services.Bad", 1),
		strings.Replace(service, "services.gitlab", "services."+strings.Repeat("a", 65), 1),
		strings.Replace(service, "name = ' GitLab '", "name = ' '", 1),
		strings.Replace(service, "description = ' Repositories '\n", "", 1),
		strings.Replace(service, "['TOKEN_B', 'TOKEN_A']", "[]", 1),
		strings.Replace(service, "'TOKEN_B'", "'TOKEN_A'", 1),
		strings.Replace(service, "'TOKEN_B'", "'AGENTBOX_API_KEY'", 1),
		strings.Replace(service, "'TOKEN_B'", "'BAD-NAME'", 1),
		service + "extra = true\n",
	} {
		if _, err := ReadProfiles(strings.NewReader(serviceProfiles + invalid)); err == nil {
			t.Fatalf("accepted invalid service: %s", invalid)
		}
	}
	if _, err := ReadProfiles(strings.NewReader(serviceProfiles + service + strings.Replace(service, "services.gitlab", "services.other", 1))); err != nil {
		t.Fatal("overlapping services rejected", err)
	}
	if p, err := ReadProfiles(strings.NewReader(serviceProfiles)); err != nil || len(p.Services) != 0 {
		t.Fatal(p, err)
	}
}

func TestResolveServices(t *testing.T) {
	p := Profiles{Services: map[string]Service{
		"a": {Name: "A", Description: "First", EnvFrom: []string{"SHARED", "TOKEN_A"}},
		"b": {Name: "B", Description: "Second", EnvFrom: []string{"TOKEN_B", "SHARED"}},
	}}
	refs, services, err := ResolveServices(nil, []string{"SHARED", "EXTRA"}, []string{"b", "a"}, p)
	if err != nil || !slices.Equal(refs, []string{"EXTRA", "SHARED", "TOKEN_A", "TOKEN_B"}) || len(services) != 2 || services[0].Code != "a" || services[1].Code != "b" {
		t.Fatal(refs, services, err)
	}
	if got := p.EnvironmentAllowlist([]string{"EXTRA", "SHARED"}); !slices.Equal(got, refs) {
		t.Fatal(got)
	}
	p.Services["a"].EnvFrom[0] = "CHANGED"
	if !slices.Equal(services[0].EnvFrom, []string{"SHARED", "TOKEN_A"}) {
		t.Fatal("snapshot aliases configuration", services)
	}
	for _, tc := range []struct {
		codes []string
		from  []string
		env   map[string]string
		code  string
	}{
		{codes: []string{"missing"}, code: "unknown_service"},
		{codes: []string{"a", "a"}, code: "validation_error"},
		{codes: []string{"BAD"}, code: "validation_error"},
		{codes: []string{"a"}, from: []string{"X", "X"}, code: "validation_error"},
		{codes: []string{"a"}, env: map[string]string{"TOKEN_A": "literal"}, code: "validation_error"},
	} {
		_, _, err := ResolveServices(tc.env, tc.from, tc.codes, p, "configuration", "sandbox")
		problem, ok := errors.AsType[*session.APIError](err)
		if !ok || problem.Problem.Code != tc.code || len(problem.Problem.Details) == 0 {
			t.Fatal(tc, err)
		}
		if tc.code == "unknown_service" && !reflect.DeepEqual(problem.Problem.Details[0].Path, []any{"body", "configuration", "sandbox", "services", 0}) {
			t.Fatal(problem.Problem)
		}
	}
	refs, services, err = ResolveServices(nil, nil, nil, p)
	if err != nil || refs == nil || services == nil || len(refs) != 0 || len(services) != 0 {
		t.Fatal(refs, services, err)
	}
}

func TestServiceEnvironmentUsesFrozenReferencesAndCurrentAllowlist(t *testing.T) {
	p, err := ReadProfiles(strings.NewReader(serviceProfiles + "[services.gitlab]\nname='GitLab'\ndescription='Repositories'\nenv_from=['SERVICE_TOKEN']"))
	if err != nil {
		t.Fatal(err)
	}
	in := session.ConfigurationInput{Agent: session.AgentInput{Profile: "default"}, Sandbox: session.SandboxInput{Template: "codex", Services: []string{"gitlab"}}, Limits: session.Limits{RunTimeoutSeconds: 3600}}
	cfg, err := Resolve(in, p, nil, DefaultMaxSessionTokens)
	if err != nil || !slices.Equal(cfg.Public.Sandbox.EnvFrom, []string{"SERVICE_TOKEN"}) || len(cfg.Public.Sandbox.Services) != 1 {
		t.Fatal(cfg, err)
	}
	cipher, err := secret.New("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	t.Setenv("SERVICE_TOKEN", "secret-value")
	t.Setenv("ADDED_TOKEN", "must-not-appear")
	p.Services["gitlab"] = Service{Name: "Renamed", Description: "New", EnvFrom: []string{"SERVICE_TOKEN", "ADDED_TOKEN"}}
	env, err := Environment(cipher, id, nil, cfg.Public, p.EnvironmentAllowlist(nil))
	if err != nil || len(env) != 1 || env["SERVICE_TOKEN"] != "secret-value" {
		t.Fatal(env, err)
	}
	delete(p.Services, "gitlab")
	if _, err := Environment(cipher, id, nil, cfg.Public, p.EnvironmentAllowlist(nil)); err == nil {
		t.Fatal("revoked reference accepted")
	}
	env, err = MergedEnvironment(cipher, id, nil, cfg.Public, nil, map[string]string{"SERVICE_TOKEN": "run-value"}, nil)
	if err != nil || env["SERVICE_TOKEN"] != "run-value" {
		t.Fatal("overridden reference was resolved", env, err)
	}
	missing := session.Configuration{Sandbox: session.SandboxConfiguration{Template: "codex", EnvFrom: []string{"ORPHEUS_TEST_MISSING_SERVICE_ENV"}}}
	if _, err := Environment(cipher, id, nil, missing, []string{"ORPHEUS_TEST_MISSING_SERVICE_ENV"}); err == nil {
		t.Fatal("missing reference accepted")
	}
	// The allowlist includes catalog names, but direct references still need selection.
	in.Sandbox.Services = nil
	in.Sandbox.EnvFrom = []string{"SERVICE_TOKEN"}
	p.Services["gitlab"] = Service{EnvFrom: []string{"SERVICE_TOKEN"}}
	if _, err := Resolve(in, p, nil, DefaultMaxSessionTokens); err != nil {
		t.Fatal("direct catalog reference rejected", err)
	}
}
