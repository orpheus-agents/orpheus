package llm

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/api"
)

func TestProfileValidationAndPatch(t *testing.T) {
	in := ProfileInput{Name: "KeepCase", AccountID: uuid.New(), Model: "model", Codex: map[string]string{"effort": "high", "personality": "friendly"}}
	got, err := PatchProfile(in, []byte(`{"codex":{"effort":null,"summary":"none"},"description":"  ","active":false}`))
	if err != nil || *got.Active || got.Description != nil || !reflect.DeepEqual(got.Codex, map[string]string{"personality": "friendly", "summary": "none"}) {
		t.Fatal(got, err)
	}
	if in.Codex["effort"] != "high" {
		t.Fatal("patch mutated its input")
	}
	for _, raw := range []string{`{}`, `{"name":"other"}`, `{"active":null}`, `{"model":null}`, `{"model":" "}`, `{"model":" padded"}`, `{"instructions":null}`, `{"codex":{"effort":""}}`, `{"codex":{"unknown":null}}`, `{"account_id":"bad"}`, `{"extra":1}`} {
		if _, err := PatchProfile(in, []byte(raw)); err == nil {
			t.Fatal("accepted invalid patch", raw)
		}
	}
	got, err = PatchProfile(in, []byte(`{"codex":null,"instructions":"\nkeep spaces\n"}`))
	if err != nil || len(got.Codex) != 0 || got.Instructions != "\nkeep spaces\n" {
		t.Fatal(got, err)
	}
	for _, model := range []string{"", " ", "a\x00b", strings.Repeat("ы", 256)} {
		changed := in
		changed.Model = model
		if _, err := NormalizeProfile(changed); err == nil {
			t.Fatal("accepted invalid model")
		}
	}
	in.Model = strings.Repeat("ы", 255)
	if _, err := NormalizeProfile(in); err != nil {
		t.Fatal("Unicode limit counted bytes", err)
	}
}
func TestCodexOptionsMatchSchemaAndRemainOptional(t *testing.T) {
	spec, err := api.GetSpec()
	if err != nil {
		t.Fatal(err)
	}
	base := ProfileInput{Name: "profile", AccountID: uuid.New(), Model: "model"}
	normalized, err := NormalizeProfile(base)
	if err != nil || normalized.Codex == nil || len(normalized.Codex) != 0 {
		t.Fatal(normalized, err)
	}
	for field, schema := range spec.Components.Schemas["CodexProfile"].Value.Properties {
		for _, value := range schema.Value.Enum {
			candidate := base
			candidate.Codex = map[string]string{field: value.(string)}
			if _, err := NormalizeProfile(candidate); err != nil {
				t.Fatal(field, value, err)
			}
		}
		for _, value := range []string{"", " ", " padded "} {
			candidate := base
			candidate.Codex = map[string]string{field: value}
			if _, err := NormalizeProfile(candidate); err == nil {
				t.Fatal("empty option accepted", field)
			}
		}
	}
}
func TestAccountCredentialsAndMasking(t *testing.T) {
	for key, want := range map[string]string{"a": "***", "ab": "***", "abc": "a...c", "12345678": "1...8", "123456789": "1234...6789", "  абвгдежзи  ": "абвг...ежзи"} {
		if got := MaskKey(key); got != want {
			t.Fatal(got, want)
		}
	}
	for _, tc := range []struct{ harness, mode, provider, want string }{
		{"codex", "api_key", "", "openai"},
		{"opencode", "api_key", "kimi-code-plan-global", "kimi-code-plan-global"},
		{"opencode", "api_key", "selectel", "selectel"},
		{"opencode", "account", "", "opencode"},
		{"opencode", "account", "opencode", "opencode"},
	} {
		a := AccountInput{Name: "Account", Harness: tc.harness, AuthMode: tc.mode, Provider: tc.provider}
		if a.AuthMode == "api_key" {
			a.APIKey = new("key")
		} else {
			a.CredentialStore = new("s")
			a.AuthFile = json.RawMessage(`{}`)
		}
		got, err := NormalizeAccount(a)
		if err != nil || got.Provider != tc.want {
			t.Fatal(tc, got.Provider, err)
		}
	}
	in := AccountInput{Name: "Account", Harness: "codex", AuthMode: "api_key", APIKey: new("key")}
	if _, err := NormalizeAccount(in); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []AccountInput{
		{Name: "Account", Harness: "opencode", AuthMode: "api_key", APIKey: new("key")},
		{Name: "Account", Harness: "codex", AuthMode: "api_key", Provider: "someone", APIKey: new("key")},
		{Name: "Account", Harness: "opencode", AuthMode: "api_key", Provider: "https://example.org", APIKey: new("key")},
		{Name: "Account", Harness: "opencode", AuthMode: "account", Provider: "opencode-go", CredentialStore: new("s"), AuthFile: json.RawMessage(`{}`)},
		{Name: "Account", Harness: "codex", AuthMode: "api_key", APIKey: new(" ")},
		{Name: "Account", Harness: "codex", AuthMode: "api_key", APIKey: new("key"), CredentialStore: new("s")},
		{Name: "Account", Harness: "codex", AuthMode: "account", CredentialStore: new("s"), AuthFile: json.RawMessage(`null`)},
		{Name: "Account", Harness: "other", AuthMode: "api_key", APIKey: new("key")},
	} {
		if _, err := NormalizeAccount(invalid); err == nil {
			t.Fatal("invalid account accepted")
		}
	}
	for _, raw := range []string{`{}`, `{"provider":"custom"}`, `{"harness":"codex"}`, `{"auth_mode":"account"}`, `{"active":null}`, `{"name":" "}`} {
		if _, err := PatchAccount(Account{Name: "Account", Active: true}, []byte(raw)); err == nil {
			t.Fatal("invalid account patch accepted", raw)
		}
	}
}
