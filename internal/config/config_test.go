package config

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/secret"
	"github.com/orpheus-agents/orpheus/internal/session"
)

func TestResolve(t *testing.T) {
	p, err := ReadProfiles(strings.NewReader(`[profiles.default]
harness="codex"
model="model"
instructions="default instructions"
[profiles.default.auth]
mode="api_key"
api_key_env="OPENAI_API_KEY"
`))
	if err != nil {
		t.Fatal(err)
	}
	in := session.ConfigurationInput{Agent: session.AgentInput{Profile: "default"}, Sandbox: session.SandboxInput{Template: "codex", Env: map[string]string{"TOKEN": "secret"}, EnvFrom: []string{"GITHUB_TOKEN"}}, Limits: session.Limits{RunTimeoutSeconds: 3600}}
	first, err := Resolve(in, p, []string{"GITHUB_TOKEN"}, DefaultMaxSessionTokens)
	if err != nil {
		t.Fatal(err)
	}
	if first.Public.Agent.Instructions != "default instructions" {
		t.Fatal(first)
	}
	in.Agent.Instructions = new("")
	next, err := Resolve(in, p, []string{"GITHUB_TOKEN"}, DefaultMaxSessionTokens)
	if err != nil || next.Public.Agent.Instructions != "" {
		t.Fatal(next, err)
	}
	if _, err := Resolve(in, p, nil, DefaultMaxSessionTokens); err == nil {
		t.Fatal("allowlist bypass")
	}
	in.Agent.Model = new(" ")
	if _, err := Resolve(in, p, []string{"GITHUB_TOKEN"}, DefaultMaxSessionTokens); err == nil {
		t.Fatal("blank model")
	}
	c, _ := secret.New(base64.URLEncoding.EncodeToString(make([]byte, 32)))
	id := uuid.New()
	token, _ := c.Encrypt(id, in.Sandbox.Env)
	t.Setenv("GITHUB_TOKEN", "a")
	env, err := Environment(c, id, token, first.Public, []string{"GITHUB_TOKEN"})
	if err != nil || env["GITHUB_TOKEN"] != "a" {
		t.Fatal(env, err)
	}
	t.Setenv("GITHUB_TOKEN", "b")
	env, err = Environment(c, id, token, first.Public, []string{"GITHUB_TOKEN"})
	if err != nil || env["GITHUB_TOKEN"] != "b" {
		t.Fatal(env, err)
	}
	if _, err := Environment(c, id, token, first.Public, nil); err == nil {
		t.Fatal("revoked allowlist accepted")
	}
}
func TestSandboxValidation(t *testing.T) {
	for _, name := range append(append([]string{}, reserved...), "1BAD", "*", "") {
		if err := ValidateSandbox(session.SandboxInput{Template: "codex", Env: map[string]string{name: "x"}}); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	for _, s := range []session.SandboxInput{{Template: "codex", Env: map[string]string{"A": "\x00"}}, {Template: "codex", Env: map[string]string{"A": "x"}, EnvFrom: []string{"A"}}, {Template: "codex", EnvFrom: []string{"A", "A"}}} {
		if err := ValidateSandbox(s); err == nil {
			t.Fatal("invalid environment accepted")
		}
	}
	for _, text := range []string{"", " \n", "a\x00b"} {
		if ValidateText(text) == nil {
			t.Fatal("invalid text accepted")
		}
	}
}
func TestAccountProfiles(t *testing.T) {
	source := `[credential_stores.s]
bucket="b"
[profiles.account]
harness="codex"
model="m"
[profiles.account.auth]
mode="account"
store="s"
key="auth.json"
`
	p, err := ReadProfiles(strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	in := session.ConfigurationInput{Agent: session.AgentInput{Profile: "account"}, Sandbox: session.SandboxInput{Template: "codex"}, Limits: session.Limits{RunTimeoutSeconds: 3600}}
	got, err := Resolve(in, p, nil, DefaultMaxSessionTokens)
	if err != nil || got.Credentials.Store.Bucket != "b" || got.Credentials.Store.Region != "us-east-1" {
		t.Fatal(got, err)
	}
	if _, err := ReadProfiles(strings.NewReader(strings.Replace(source, `store="s"`, `store="missing"`, 1))); err == nil {
		t.Fatal("unknown credential store")
	}
	if _, err := ReadProfiles(strings.NewReader(source + "\nunknown=true")); err == nil {
		t.Fatal("unknown profile field")
	}
}
func TestSettingsValidation(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://fixture")
	t.Setenv("PUBLIC_API_KEYS", `["x"]`)
	got, err := Load()
	if err != nil || got.MaxConcurrentSessions != 50 {
		t.Fatal(got, err)
	}
	for _, name := range []string{"MAX_CONCURRENT_SESSIONS", "MAX_TOOL_RESULT_BYTES", "MAX_REQUEST_BYTES", "READINESS_TIMEOUT", "RPC_TIMEOUT_SECONDS"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "0")
			if _, err := Load(); err == nil {
				t.Fatal("accepted zero")
			}
		})
	}
}
