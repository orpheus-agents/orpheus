package config

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/orpheus-agents/orpheus/internal/api"
	"github.com/orpheus-agents/orpheus/internal/session"
)

const catalogProfile = "[profiles.default]\nharness='codex'\ndescription='Research profile'\nmodel='model'\ninstructions='Profile instructions'\n[profiles.default.auth]\nmode='api_key'\napi_key_env='KEY'\n"

func TestProfilesWithoutTemplateCatalog(t *testing.T) {
	for _, description := range []string{"", "description = ''", "description = 'New description'"} {
		source := strings.Replace(catalogProfile, "description='Research profile'", description, 1)
		if _, err := ReadProfiles(strings.NewReader(source)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ReadProfiles(strings.NewReader(strings.Replace(catalogProfile, "description='Research profile'", "description = 42", 1))); err == nil {
		t.Fatal("accepted invalid profile description")
	}
	// The existing strict decoder rejects the removed configuration field.
	if _, err := ReadProfiles(strings.NewReader("[templates.codex]\n" + catalogProfile)); err == nil {
		t.Fatal("accepted obsolete template catalog")
	}
}

func TestResolveProfileDescriptionAndTemplateName(t *testing.T) {
	p, err := ReadProfiles(strings.NewReader(catalogProfile))
	if err != nil {
		t.Fatal(err)
	}
	input := session.ConfigurationInput{Agent: session.AgentInput{Profile: "default"}, Sandbox: session.SandboxInput{Template: "team/codex:v1.2.0"}, Limits: session.Limits{RunTimeoutSeconds: 3600}}
	before, err := Resolve(input, p, nil, DefaultMaxSessionTokens)
	if err != nil {
		t.Fatal(err)
	}
	profile := p.Profiles["default"]
	profile.Description = new("Changed profile description")
	p.Profiles["default"] = profile
	after, err := Resolve(input, p, nil, DefaultMaxSessionTokens)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("description changed execution snapshot", err)
	}
	if after.Public.Sandbox.Template != input.Sandbox.Template {
		t.Fatal("template name changed", after.Public.Sandbox.Template)
	}
	raw, err := json.Marshal(after)
	if err != nil || strings.Contains(string(raw), "description") {
		t.Fatal("description in snapshot", err)
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
