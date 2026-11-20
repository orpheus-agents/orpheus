package config

import (
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/orpheus-agents/orpheus/internal/session"
)

const directService = `[services.registry]
name = " Registry "
description = " Image registry "
delivery = "direct"
endpoint = "https://registry.example:5443/path/v1"
credentials.password = { env = "WORKER_TOKEN", as = "REGISTRY_TOKEN" }
credentials.username = { env = "REGISTRY_USER" }
environment.REGISTRY = { from = "endpoint.host" }
environment.URL = { from = "endpoint" }
environment.REGION = "eu"
environment.EMPTY = ""
`

func TestServiceConfiguration(t *testing.T) {
	p, err := ReadDeployment(strings.NewReader(directService))
	if err != nil {
		t.Fatal(err)
	}
	s := p.Services["registry"]
	if s.Name != "Registry" || s.Description != "Image registry" || !slices.Equal(s.EnvNames(), []string{"EMPTY", "REGION", "REGISTRY", "REGISTRY_TOKEN", "REGISTRY_USER", "URL"}) {
		t.Fatal(s)
	}
	if !slices.Equal(p.EnvironmentAllowlist([]string{"MANUAL", "REGISTRY_USER"}), []string{"MANUAL", "REGISTRY_USER", "WORKER_TOKEN"}) {
		t.Fatal("unexpected allowlist")
	}
	for _, tc := range [][2]string{
		{"services.registry", "services.Bad"}, {"services.registry", "services." + strings.Repeat("a", 65)},
		{" Registry ", " "}, {" Image registry ", " "}, {"direct", "broker"},
		{"https://registry.example:5443/path/v1", "relative/path"}, {"https://registry.example:5443/path/v1", "https://:5443"},
		{":5443/path/v1", ":70000"}, {":5443/path/v1", ":x"}, {"registry.example", "user@registry.example"},
		{"path/v1", "path?"}, {"path/v1", "path#"}, {"https://", "ftp://"},
		{"WORKER_TOKEN", "AGENTBOX_API_KEY"}, {"REGISTRY_TOKEN", "BAD-NAME"}, {"REGISTRY_TOKEN", ""},
		{"env = \"REGISTRY_USER\"", "source = \"REGISTRY_USER\""}, {"endpoint.host", "auth.key"},
		{"from = \"endpoint\"", "from = \"endpoint\", extra = true"}, {"REGION = \"eu\"", "REGION = 2"},
		{"REGION = \"eu\"", "REGION = \"\\u0000\""},
		{"environment.EMPTY", "environment.ORPHEUS_RUN_ID"}, {"delivery =", "legacy ="},
	} {
		if _, err := ReadDeployment(strings.NewReader(strings.ReplaceAll(directService, tc[0], tc[1]))); err == nil {
			t.Fatalf("accepted %q -> %q", tc[0], tc[1])
		}
	}
	for _, suffix := range []string{"env_from = ['TOKEN']\n", "environment.URL = 'duplicate'\n"} {
		if _, err := ReadDeployment(strings.NewReader(directService + suffix)); err == nil {
			t.Fatal(suffix)
		}
	}
	if _, err := ReadDeployment(strings.NewReader(strings.Split(directService, "environment.REGISTRY")[0])); err == nil {
		t.Fatal("accepted empty environment")
	}
	if _, err := ReadDeployment(strings.NewReader(directService + strings.Replace(directService, "services.registry", "services.other", 1))); err != nil {
		t.Fatal(err)
	}
	if p, err := ReadDeployment(strings.NewReader("")); err != nil || len(p.Services) != 0 {
		t.Fatal(p, err)
	}
}

func TestValidateServiceCodes(t *testing.T) {
	p := Deployment{Services: map[string]Service{"a": {}, "b": {}}}
	input := []string{"b", "a"}
	got, err := p.ValidateServiceCodes(input)
	if err != nil || !slices.Equal(got, []string{"a", "b"}) || input[0] != "b" {
		t.Fatal(got, err)
	}
	if got, err := p.ValidateServiceCodes(nil); err != nil || got == nil || len(got) != 0 {
		t.Fatal(got, err)
	}
	for _, tc := range []struct {
		codes []string
		code  string
		index int
	}{
		{[]string{"a", "missing"}, "unknown_service", 1}, {[]string{"a", "a"}, "validation_error", 1}, {[]string{"BAD"}, "validation_error", 0},
	} {
		_, err := p.ValidateServiceCodes(tc.codes, "configuration", "sandbox")
		problem, ok := errors.AsType[*session.APIError](err)
		if !ok || problem.Problem.Code != tc.code || !reflect.DeepEqual(problem.Problem.Details[0].Path, []any{"body", "configuration", "sandbox", "services", tc.index}) {
			t.Fatal(tc, err)
		}
	}
}

func TestResolveDirectEnvironment(t *testing.T) {
	p, err := ReadDeployment(strings.NewReader(directService))
	if err != nil {
		t.Fatal(err)
	}
	base := EnvironmentInput{Services: []string{"registry"}, Env: map[string]string{"REGION": "eu"}, EnvFrom: []string{"REGISTRY_USER"}}
	t.Setenv("REGISTRY_USER", "reader")
	t.Setenv("WORKER_TOKEN", "first-value")
	want := map[string]string{"URL": "https://registry.example:5443/path/v1", "REGISTRY": "registry.example:5443", "REGION": "eu", "REGISTRY_USER": "reader", "REGISTRY_TOKEN": "first-value", "EMPTY": ""}
	got, err := p.ResolveEnvironment(base, EnvironmentInput{}, nil)
	if err != nil || !maps.Equal(got, want) {
		t.Fatal(got, err)
	}
	t.Setenv("WORKER_TOKEN", "rotated-value")
	base.Env["REGISTRY_TOKEN"] = "disallowed-literal-value"
	_, err = p.ResolveEnvironment(base, EnvironmentInput{}, nil)
	if err == nil || !strings.Contains(err.Error(), "REGISTRY_TOKEN") || !strings.Contains(err.Error(), "registry.credentials.password") || strings.Contains(err.Error(), "value") {
		t.Fatal(err)
	}
	delete(base.Env, "REGISTRY_TOKEN")
	got, err = p.ResolveEnvironment(base, EnvironmentInput{}, nil)
	if err != nil || got["REGISTRY_TOKEN"] != "rotated-value" {
		t.Fatal(got, err)
	}
	t.Setenv("WORKER_TOKEN", "")
	if _, err := p.ResolveEnvironment(base, EnvironmentInput{}, nil); err == nil || !strings.Contains(err.Error(), "WORKER_TOKEN") {
		t.Fatal(err)
	}
	override := EnvironmentInput{Env: map[string]string{"REGISTRY_TOKEN": "run-value"}}
	got, err = p.ResolveEnvironment(base, override, nil)
	if err != nil || got["REGISTRY_TOKEN"] != "run-value" {
		t.Fatal(got, err)
	}
	for _, name := range []string{"REGISTRY_TOKEN", "REGION"} {
		if _, err := p.ResolveEnvironment(EnvironmentInput{EnvFrom: []string{name}}, EnvironmentInput{}, nil); err == nil {
			t.Fatalf("allowed direct source %s", name)
		}
	}
	t.Setenv("WORKER_TOKEN", "")
	got, err = p.ResolveEnvironment(EnvironmentInput{EnvFrom: []string{"WORKER_TOKEN"}}, EnvironmentInput{}, nil)
	if err != nil || !maps.Equal(got, map[string]string{"WORKER_TOKEN": ""}) {
		t.Fatal(got, err)
	}
	base.EnvFrom = nil
	delete(p.Services, "registry")
	if _, err := p.ResolveEnvironment(base, override, nil); err == nil || !strings.Contains(err.Error(), "registry") {
		t.Fatal(err)
	}
}
