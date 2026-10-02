package config

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/orpheus-agents/orpheus/internal/api"
	"github.com/orpheus-agents/orpheus/internal/session"
)

const catalogProfile = "[profiles.default]\nharness='codex'\ndescription='Research profile'\nmodel='model'\ninstructions='Profile instructions'\n[profiles.default.auth]\nmode='api_key'\napi_key_env='KEY'\n"

func TestTemplateCatalog(t *testing.T) {
	for _, tc := range []struct {
		name, templates string
		valid           bool
	}{
		{"missing", "", false},
		{"empty catalog", "[templates]\n", false},
		{"empty name", "[templates.'']\n", false},
		{"blank name", "[templates.' ']\n", false},
		{"leading whitespace", "[templates.' codex']\n", false},
		{"trailing whitespace", "[templates.'codex ']\n", false},
		{"unknown field", "[templates.codex]\nunknown = 'value'\n", false},
		{"wrong description type", "[templates.codex]\ndescription = 42\n", false},
		{"name", "[templates.codex]\n", true},
		{"tagged name", "[templates.'codex:v1.2.0']\ndescription = 'Tagged sandbox'\n", true},
		{"empty description", "[templates.codex]\ndescription = ''\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ReadProfiles(strings.NewReader(tc.templates + catalogProfile))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
	p, err := ReadProfiles(strings.NewReader("[templates.codex]\n[templates.'codex:v1.2.0']\ndescription = 'Tagged sandbox'\n" + catalogProfile))
	if err != nil {
		t.Fatal(err)
	}
	if p.Templates["codex"].Description != nil || *p.Templates["codex:v1.2.0"].Description != "Tagged sandbox" || *p.Profiles["default"].Description != "Research profile" {
		t.Fatal("descriptions were not preserved")
	}
	for _, description := range []string{"", "description = ''", "description = 'New description'"} {
		source := strings.Replace(catalogProfile, "description='Research profile'", description, 1)
		if _, err := ReadProfiles(strings.NewReader("[templates.codex]\n" + source)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ReadProfiles(strings.NewReader("[templates.codex]\n" + strings.Replace(catalogProfile, "description='Research profile'", "description = 42", 1))); err == nil {
		t.Fatal("accepted invalid profile description")
	}
}

func TestResolveCatalogAndDescriptions(t *testing.T) {
	p, err := ReadProfiles(strings.NewReader("[templates.'codex:v1.2.0']\n" + catalogProfile))
	if err != nil {
		t.Fatal(err)
	}
	input := session.ConfigurationInput{Agent: session.AgentInput{Profile: "default"}, Sandbox: session.SandboxInput{Template: "codex:v1.2.0"}, Limits: session.Limits{RunTimeoutSeconds: 3600}}
	before, err := Resolve(input, p, nil, DefaultMaxSessionTokens)
	if err != nil {
		t.Fatal(err)
	}
	profile := p.Profiles["default"]
	profile.Description = new("Changed profile description")
	p.Profiles["default"] = profile
	p.Templates[input.Sandbox.Template] = Template{Description: new("Changed template description")}
	after, err := Resolve(input, p, nil, DefaultMaxSessionTokens)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("descriptions changed execution snapshot", err)
	}
	raw, err := json.Marshal(after)
	if err != nil || strings.Contains(string(raw), "description") {
		t.Fatal("description in snapshot", err)
	}
	for _, template := range []string{"codex", "codex:v2", " codex:v1.2.0"} {
		input.Sandbox.Template = template
		_, err := Resolve(input, p, nil, DefaultMaxSessionTokens)
		problem, ok := errors.AsType[*session.APIError](err)
		if !ok || problem.Status != 422 || problem.Problem.Code != "unknown_template" {
			t.Fatalf("%q: %v", template, err)
		}
		want := []session.Detail{{Path: []any{"body", "configuration", "sandbox", "template"}, Code: "invalid_value"}}
		if !reflect.DeepEqual(problem.Problem.Details, want) {
			t.Fatal(problem.Problem.Details)
		}
	}
}

func TestCodexOptionEnumsMatchOpenAPI(t *testing.T) {
	spec, err := api.GetSpec()
	if err != nil {
		t.Fatal(err)
	}
	properties := spec.Components.Schemas["CodexProfile"].Value.Properties
	fromAPI := map[string][]string{}
	for name, property := range properties {
		if len(property.Value.Enum) == 0 {
			continue
		}
		for _, value := range property.Value.Enum {
			text, ok := value.(string)
			if !ok {
				t.Fatalf("%s: non-string enum value %v", name, value)
			}
			fromAPI[name] = append(fromAPI[name], text)
		}
		slices.Sort(fromAPI[name])
	}
	fromConfig := map[string][]string{}
	for name, values := range codexOptionEnums {
		fromConfig[name] = slices.Sorted(slices.Values(values))
	}
	if !reflect.DeepEqual(fromAPI, fromConfig) {
		t.Fatalf("Codex enums differ: OpenAPI=%v config=%v", fromAPI, fromConfig)
	}
}
