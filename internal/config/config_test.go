package config

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus/internal/llm"
	"github.com/orpheus-agents/orpheus/internal/session"
)

func TestResolve(t *testing.T) {
	p := llm.ProfileInput{Name: "default", Model: "model", Instructions: "default instructions", Codex: map[string]string{"effort": "medium"}}
	in := session.ConfigurationInput{Agent: session.AgentInput{Profile: "default"}, Sandbox: session.SandboxInput{Template: "codex", Env: map[string]string{"TOKEN": "secret"}, EnvFrom: []string{"GITHUB_TOKEN"}}, Limits: session.Limits{RunTimeoutSeconds: 3600}}
	first, err := Resolve(in, p, "codex", Deployment{}, []string{"GITHUB_TOKEN"}, DefaultMaxSessionTokens)
	if err != nil {
		t.Fatal(err)
	}
	if first.Public.Agent.Instructions != "default instructions" || first.Public.Agent.Codex.Effort != "medium" {
		t.Fatal(first)
	}
	in.Agent.Instructions = new("")
	next, err := Resolve(in, p, "codex", Deployment{}, []string{"GITHUB_TOKEN"}, DefaultMaxSessionTokens)
	if err != nil || next.Public.Agent.Instructions != "" || next.Public.Agent.Codex.Effort != "medium" {
		t.Fatal(next, err)
	}
	if _, err := Resolve(in, p, "codex", Deployment{}, nil, DefaultMaxSessionTokens); err == nil {
		t.Fatal("allowlist bypass")
	}
	in.Agent.Model = new(" ")
	if _, err := Resolve(in, p, "codex", Deployment{}, []string{"GITHUB_TOKEN"}, DefaultMaxSessionTokens); err == nil {
		t.Fatal("blank model")
	}
	t.Setenv("GITHUB_TOKEN", "a")
	env, err := (Deployment{}).ResolveEnvironment(EnvironmentInput{Env: in.Sandbox.Env, EnvFrom: first.Public.Sandbox.EnvFrom}, EnvironmentInput{}, []string{"GITHUB_TOKEN"})
	if err != nil || env["GITHUB_TOKEN"] != "a" {
		t.Fatal(env, err)
	}
	t.Setenv("GITHUB_TOKEN", "b")
	env, err = (Deployment{}).ResolveEnvironment(EnvironmentInput{Env: in.Sandbox.Env, EnvFrom: first.Public.Sandbox.EnvFrom}, EnvironmentInput{}, []string{"GITHUB_TOKEN"})
	if err != nil || env["GITHUB_TOKEN"] != "b" {
		t.Fatal(env, err)
	}
	if _, err := (Deployment{}).ResolveEnvironment(EnvironmentInput{Env: in.Sandbox.Env, EnvFrom: first.Public.Sandbox.EnvFrom}, EnvironmentInput{}, nil); err == nil {
		t.Fatal("revoked allowlist accepted")
	}
}
func TestSandboxProxyURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://fixture")
	for _, tc := range []struct {
		name  string
		value string
		valid bool
	}{
		{"AgentBox HTTPS", "https://sandbox-proxy.agentbox.ru:65181", true},
		{"custom HTTP", "http://proxy.example:3128", true},
		{"disabled", "", true},
		{"AgentBox SOCKS5H", "socks5h://sandbox-proxy.agentbox.ru:65180", true},
		{"custom SOCKS5", "socks5://proxy.example:1080", true},
		{"uppercase SOCKS5H", "SOCKS5H://proxy.example:1080", true},
		{"unsupported scheme", "socks4://proxy.example:1080", false},
		{"missing host", "https://", false},
		{"missing SOCKS host", "socks5h://:1080", false},
		{"invalid URL", "socks5h://proxy.example:invalid", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SANDBOX_PROXY_URL", tc.value)
			settings, err := Load()
			if (err == nil) != tc.valid {
				t.Fatalf("proxy=%q settings=%+v err=%v", tc.value, settings, err)
			}
			if err == nil && settings.SandboxProxyURL != tc.value {
				t.Fatalf("proxy=%q, want %q", settings.SandboxProxyURL, tc.value)
			}
		})
	}
}

func TestSandboxValidation(t *testing.T) {
	for _, name := range append(append([]string{}, reserved...), "1BAD", "*", "") {
		if err := ValidateSandbox(session.SandboxInput{Template: "codex", Env: map[string]string{name: "x"}}); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	for _, s := range []session.SandboxInput{{Template: "codex", Env: map[string]string{"A": "\x00"}}, {Template: "codex", EnvFrom: []string{"A", "A"}}} {
		if err := ValidateSandbox(s); err == nil {
			t.Fatal("invalid environment accepted")
		}
	}
	for _, text := range []string{"", " \n", "a\x00b"} {
		if ValidateText(text, 0) == nil {
			t.Fatal("invalid text accepted")
		}
	}
}
func TestSettingsValidation(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://fixture")
	t.Setenv("PUBLIC_API_KEYS", `["x"]`)
	got, err := Load()
	if err != nil || got.MaxConcurrentSessions != 50 {
		t.Fatal(got, err)
	}
	for _, name := range []string{"MAX_CONCURRENT_SESSIONS", "MAX_TOOL_RESULT_BYTES", "MAX_REQUEST_BYTES", "DATABASE_PING_TIMEOUT", "RPC_TIMEOUT_SECONDS", "SANDBOX_PROVISION_TIMEOUT_SECONDS"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "0")
			if _, err := Load(); err == nil {
				t.Fatal("accepted zero")
			}
		})
	}
}

func TestDatabasePingTimeout(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://fixture")
	t.Setenv("PUBLIC_API_KEYS", `["x"]`)
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{{"", 2 * time.Second}, {"7", 7 * time.Second}} {
		t.Run(tc.want.String(), func(t *testing.T) {
			t.Setenv("DATABASE_PING_TIMEOUT", tc.value)
			if tc.value == "" {
				if err := os.Unsetenv("DATABASE_PING_TIMEOUT"); err != nil {
					t.Fatal(err)
				}
			}
			got, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if got.DatabasePingTimeout != tc.want {
				t.Fatalf("want %s, got %s", tc.want, got.DatabasePingTimeout)
			}
		})
	}
}

func TestSandboxRetentionSettings(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://fixture")
	defaults := DefaultSettings()
	if defaults.SandboxPausedRetentionDays != 14 || defaults.SandboxCleanupBatchSize != 1000 {
		t.Fatal(defaults)
	}
	for _, tc := range []struct {
		name string
		max  string
	}{
		{"SANDBOX_PAUSED_RETENTION_DAYS", "101"},
		{"SANDBOX_CLEANUP_BATCH_SIZE", "2147483648"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, value := range []string{"", "0", "-1", "1.5", "no", tc.max, "99999999999999999999"} {
				t.Setenv(tc.name, value)
				if _, err := Load(); err == nil {
					t.Fatalf("accepted %s=%q", tc.name, value)
				}
			}
			t.Setenv(tc.name, "7")
			got, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "SANDBOX_PAUSED_RETENTION_DAYS" && got.SandboxPausedRetentionDays != 7 || tc.name == "SANDBOX_CLEANUP_BATCH_SIZE" && got.SandboxCleanupBatchSize != 7 {
				t.Fatal(got)
			}
		})
	}
}

func TestSandboxRetentionBoundaryDays(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://fixture")
	for _, days := range []int{1, 100} {
		t.Setenv("SANDBOX_PAUSED_RETENTION_DAYS", fmt.Sprint(days))
		got, err := Load()
		if err != nil || got.SandboxPausedRetentionDays != days {
			t.Fatal(got, err)
		}
	}
}
