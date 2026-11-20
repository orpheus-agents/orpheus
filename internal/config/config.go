// Package config loads infrastructure settings and resolves execution snapshots.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/orpheus-agents/orpheus/internal/llm"
	"github.com/orpheus-agents/orpheus/internal/secret"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/pelletier/go-toml/v2"
)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var reserved = strings.Fields(`ALL_PROXY NO_PROXY HTTP_PROXY HTTPS_PROXY all_proxy no_proxy http_proxy https_proxy HOME CODEX_HOME ORPHEUS_LAUNCH_ID ORPHEUS_HOOK_OPERATION_ID ORPHEUS_SESSION_ID ORPHEUS_WORKSPACE_PATH ORPHEUS_RUN_ID ORPHEUS_INPUT_FINGERPRINT ORPHEUS_AGENT_STATUS ORPHEUS_STOP_REASON ORPHEUS_BROWSER_AUTH ORPHEUS_PUBLIC_URL SAML_SP_ENTITY_ID SAML_IDP_METADATA_FILE SAML_SP_CERT_FILE SAML_SP_KEY_FILE BROWSER_SESSION_TTL_SECONDS PUBLIC_API_KEYS OPENAI_API_KEY CODEX_API_KEY OPENAI_BASE_URL OPENAI_ORG_ID OPENAI_ORGANIZATION OPENAI_PROJECT_ID CHATGPT_BASE_URL ENV_ENCRYPTION_KEY AGENTBOX_API_KEY`)

type Deployment struct {
	Services         map[string]Service                 `toml:"services"`
	CredentialStores map[string]session.CredentialStore `toml:"credential_stores"`
}

func ReadDeployment(r io.Reader) (Deployment, error) {
	var source struct {
		Services         map[string]Service `toml:"services"`
		CredentialStores map[string]struct {
			Type        *string `toml:"type"`
			Bucket      string  `toml:"bucket"`
			Region      *string `toml:"region"`
			EndpointURL *string `toml:"endpoint_url"`
		} `toml:"credential_stores"`
	}
	out := Deployment{CredentialStores: map[string]session.CredentialStore{}}
	if err := toml.NewDecoder(r).DisallowUnknownFields().Decode(&source); err != nil {
		return out, err
	}
	if err := validateServices(source.Services); err != nil {
		return out, err
	}
	out.Services = source.Services
	for name, raw := range source.CredentialStores {
		value := session.CredentialStore{Type: "s3", Bucket: raw.Bucket, Region: "us-east-1", EndpointURL: raw.EndpointURL}
		if raw.Type != nil {
			value.Type = *raw.Type
		}
		if raw.Region != nil {
			value.Region = *raw.Region
		}
		if name == "" || value.Type != "s3" || value.Bucket == "" || value.Region == "" || (value.EndpointURL != nil && *value.EndpointURL == "") {
			return out, errors.New("invalid credential store")
		}
		if _, err := canonicalEndpoint(value.EndpointURL); err != nil {
			return out, err
		}
		out.CredentialStores[name] = value
	}
	return out, nil
}
func LoadDeployment(path string) (Deployment, error) {
	file, err := os.Open(path)
	if err != nil {
		return Deployment{}, err
	}
	defer func() { _ = file.Close() }()
	return ReadDeployment(file)
}

func ValidateSandbox(s session.SandboxInput) error {
	if s.Template == "" {
		return invalid("Sandbox template is required.", "configuration", "sandbox", "template")
	}
	return validateEnvironment(s.Env, s.EnvFrom, []any{"configuration", "sandbox"})
}
func resolveHooks(input *session.HooksInput) (session.HooksConfiguration, error) {
	hooks := session.HooksConfiguration{TimeoutSeconds: 300}
	if input == nil {
		return hooks, nil
	}
	if input.TimeoutSeconds != nil {
		if *input.TimeoutSeconds <= 0 || int64(*input.TimeoutSeconds) > 2147483647 {
			return hooks, invalid("Invalid hook timeout.", "configuration", "hooks", "timeout_seconds")
		}
		hooks.TimeoutSeconds = *input.TimeoutSeconds
	}
	for _, script := range []struct {
		name string
		text *string
		dest **string
	}{
		{"after_create", input.AfterCreate, &hooks.AfterCreate},
		{"before_run", input.BeforeRun, &hooks.BeforeRun},
		{"after_run", input.AfterRun, &hooks.AfterRun},
		{"before_remove", input.BeforeRemove, &hooks.BeforeRemove},
	} {
		if script.text == nil {
			continue
		}
		line, _, _ := strings.Cut(*script.text, "\n")
		if len(*script.text) > 65536 || strings.ContainsRune(*script.text, 0) || strings.ContainsRune(line, '\r') || !strings.HasPrefix(line, "#!") || strings.TrimSpace(line[2:]) == "" {
			return hooks, invalid("A hook requires an LF-terminated shebang and at most 65536 UTF-8 bytes without NUL.", "configuration", "hooks", script.name)
		}
		*script.dest = script.text
	}
	return hooks, nil
}
func ValidateRunEnvironment(env map[string]string, from, allowlist []string) error {
	if err := validateEnvironment(env, from, nil); err != nil {
		return err
	}
	for _, name := range from {
		if !slices.Contains(allowlist, name) {
			return invalid("Environment reference is not allowed.", "env_from")
		}
	}
	return nil
}
func validateEnvironment(env map[string]string, from []string, prefix []any) error {
	names := make(map[string]bool, len(env)+len(from))
	for name, value := range env {
		if strings.ContainsRune(value, 0) {
			return invalid("NUL is not allowed in environment variables.", append(slices.Clone(prefix), "env", name)...)
		}
		if !envName.MatchString(name) || slices.Contains(reserved, name) {
			return invalid("Invalid or reserved environment variable.", append(slices.Clone(prefix), "env", name)...)
		}
		names[name] = true
	}
	for _, name := range from {
		if !envName.MatchString(name) || slices.Contains(reserved, name) {
			return invalid("Invalid or reserved environment variable.", append(slices.Clone(prefix), "env_from")...)
		}
		if names[name] {
			return invalid("Duplicate environment variable.", append(slices.Clone(prefix), "env_from")...)
		}
		names[name] = true
	}
	return nil
}
func ValidateText(text string, index int) error {
	if !session.ValidText(text) {
		return invalid("A nonblank text without NUL is required.", "messages", index, "text")
	}
	return nil
}
func invalid(message string, path ...any) *session.APIError {
	p := session.Problem(422, "validation_error", message)
	p.Problem.Details = []session.Detail{{Path: append([]any{"body"}, path...), Code: "invalid_value"}}
	return p
}
func Resolve(in session.ConfigurationInput, profile llm.ProfileInput, harness string, p Deployment, allowlist []string, defaultMaxTokens int64) (session.ResolvedConfiguration, error) {
	var out session.ResolvedConfiguration
	if err := ValidateSandbox(in.Sandbox); err != nil {
		return out, err
	}
	refs, services, err := ResolveServices(in.Sandbox.Env, in.Sandbox.EnvFrom, in.Sandbox.Services, p, "configuration", "sandbox")
	if err != nil {
		return out, err
	}
	allowlist = p.EnvironmentAllowlist(allowlist)
	hooks, err := resolveHooks(in.Hooks)
	if err != nil {
		return out, err
	}
	for _, name := range refs {
		if !slices.Contains(allowlist, name) {
			return out, invalid("Environment reference is not allowed.", "configuration", "sandbox", "env_from")
		}
	}
	model := new(profile.Model)
	if in.Agent.Model != nil {
		model = in.Agent.Model
	}
	if model == nil || strings.TrimSpace(*model) == "" {
		return out, invalid("An explicit model is required.", "configuration", "agent", "model")
	}
	instructions := profile.Instructions
	if in.Agent.Instructions != nil {
		instructions = *in.Agent.Instructions
	}
	if in.Limits.RunTimeoutSeconds <= 0 || in.Limits.RunTimeoutSeconds > 2147483647 {
		return out, invalid("Invalid run timeout.", "configuration", "limits", "run_timeout_seconds")
	}
	if in.Limits.MaxSessionTokens == 0 {
		in.Limits.MaxSessionTokens = defaultMaxTokens
	}
	if in.Limits.MaxSessionTokens <= 0 {
		return out, invalid("Invalid session token budget.", "configuration", "limits", "max_session_tokens")
	}
	names := slices.Sorted(maps.Keys(in.Sandbox.Env))
	if names == nil {
		names = []string{}
	}
	out = session.ResolvedConfiguration{Version: 1, Harness: harness, Public: session.Configuration{Agent: session.AgentConfiguration{Profile: in.Agent.Profile, Model: *model, Codex: profile.Options(), OpenCode: profile.OpenCode, Instructions: instructions}, Sandbox: session.SandboxConfiguration{Template: in.Sandbox.Template, EnvNames: names, EnvFrom: refs, Services: services}, Limits: in.Limits, Hooks: hooks}}
	return out, nil
}
func Environment(env map[string]string, cfg session.Configuration, allowlist []string) (map[string]string, error) {
	return MergedEnvironment(env, cfg, allowlist, nil, nil)
}

// MergedEnvironment resolves selected sources after run-level overrides have
// replaced session-level sources. An overridden reference is never read.
func MergedEnvironment(env map[string]string, cfg session.Configuration, allowlist []string, overrides map[string]string, overrideFrom []string) (map[string]string, error) {
	env = maps.Clone(env)
	if env == nil {
		env = map[string]string{}
	}
	for _, name := range cfg.Sandbox.EnvFrom {
		if _, ok := overrides[name]; ok || slices.Contains(overrideFrom, name) {
			continue
		}
		if !slices.Contains(allowlist, name) {
			return nil, errors.New("environment reference is no longer allowed")
		}
		value, ok := os.LookupEnv(name)
		if !ok {
			return nil, errors.New("environment reference is unavailable")
		}
		env[name] = value
	}
	maps.Copy(env, overrides)
	for _, name := range overrideFrom {
		if !slices.Contains(allowlist, name) {
			return nil, errors.New("run environment reference is no longer allowed")
		}
		value, ok := os.LookupEnv(name)
		if !ok {
			return nil, errors.New("run environment reference is unavailable")
		}
		env[name] = value
	}
	if err := ValidateSandbox(session.SandboxInput{Template: cfg.Sandbox.Template, Env: env}); err != nil {
		return nil, err
	}
	return env, nil
}

const DefaultMaxSessionTokens int64 = 100_000_000

type Settings struct {
	BrowserAuth                BrowserAuth
	DefaultMaxSessionTokens    int64
	DatabaseURL                string
	ConfigFile                 string
	PublicAPIKeys              []string
	EnvEncryptionKey           string
	HarnessEnvAllowlist        []string
	SandboxProxyURL            string
	MaxConcurrentSessions      int
	MaxToolResultBytes         int
	MaxHookOutputBytes         int
	MaxRequestBytes            int64
	DatabasePingTimeout        time.Duration
	CancelGrace                time.Duration
	WorkerPoll                 time.Duration
	RPCTimeout                 time.Duration
	SandboxProvisionTimeout    time.Duration
	SandboxPausedRetentionDays int
	SandboxCleanupBatchSize    int
}

func DefaultSettings() Settings {
	return Settings{BrowserAuth: BrowserAuth{Mode: "api_only", SessionTTL: 12 * time.Hour}, DefaultMaxSessionTokens: DefaultMaxSessionTokens, ConfigFile: "orpheus.toml", SandboxProxyURL: "https://sandbox-proxy.agentbox.ru:65181", MaxConcurrentSessions: 50, MaxToolResultBytes: 524288, MaxHookOutputBytes: 524288, MaxRequestBytes: 1048576, DatabasePingTimeout: 2 * time.Second, CancelGrace: 30 * time.Second, WorkerPoll: time.Second, RPCTimeout: 30 * time.Second, SandboxProvisionTimeout: 120 * time.Second, SandboxPausedRetentionDays: 14, SandboxCleanupBatchSize: 1000}
}

// AddSandboxProxy applies the sandbox egress proxy to an agent process environment.
func AddSandboxProxy(env map[string]string, proxy string) {
	if proxy == "" {
		return
	}
	scheme, _, _ := strings.Cut(proxy, ":")
	switch strings.ToLower(scheme) {
	case "socks5", "socks5h":
		env["ALL_PROXY"] = proxy
	default:
		env["HTTPS_PROXY"] = proxy
		env["HTTP_PROXY"] = proxy
	}
	env["NO_PROXY"] = "localhost,127.0.0.1"
}
func Load() (Settings, error) {
	s := DefaultSettings()
	for name, dest := range map[string]*string{"ORPHEUS_BROWSER_AUTH": &s.BrowserAuth.Mode, "ORPHEUS_PUBLIC_URL": &s.BrowserAuth.PublicURL, "SAML_SP_ENTITY_ID": &s.BrowserAuth.EntityID, "SAML_IDP_METADATA_FILE": &s.BrowserAuth.MetadataFile, "SAML_SP_CERT_FILE": &s.BrowserAuth.CertFile, "SAML_SP_KEY_FILE": &s.BrowserAuth.KeyFile, "DATABASE_URL": &s.DatabaseURL, "ORPHEUS_CONFIG_FILE": &s.ConfigFile, "ENV_ENCRYPTION_KEY": &s.EnvEncryptionKey, "SANDBOX_PROXY_URL": &s.SandboxProxyURL} {
		if v, ok := os.LookupEnv(name); ok {
			*dest = v
		}
	}
	if s.SandboxProxyURL != "" {
		proxy, err := url.Parse(s.SandboxProxyURL)
		if err != nil || !slices.Contains([]string{"http", "https", "socks5", "socks5h"}, proxy.Scheme) || proxy.Hostname() == "" {
			return s, errors.New("SANDBOX_PROXY_URL must be an HTTP, HTTPS, SOCKS5 or SOCKS5H proxy URL with a host")
		}
	}
	s.BrowserAuth.ttlSeconds = os.Getenv("BROWSER_SESSION_TTL_SECONDS")
	if s.DatabaseURL == "" {
		return s, errors.New("DATABASE_URL is required")
	}
	for name, dest := range map[string]*[]string{"PUBLIC_API_KEYS": &s.PublicAPIKeys, "HARNESS_ENV_ALLOWLIST": &s.HarnessEnvAllowlist} {
		if v, ok := os.LookupEnv(name); ok {
			var values []*string
			if err := json.Unmarshal([]byte(v), &values); err != nil || values == nil {
				return s, fmt.Errorf("invalid %s", name)
			}
			*dest = make([]string, 0, len(values))
			for _, value := range values {
				if value == nil {
					return s, fmt.Errorf("invalid %s", name)
				}
				*dest = append(*dest, *value)
			}
		}
	}
	for name, dest := range map[string]*int{"SANDBOX_PAUSED_RETENTION_DAYS": &s.SandboxPausedRetentionDays, "SANDBOX_CLEANUP_BATCH_SIZE": &s.SandboxCleanupBatchSize, "MAX_CONCURRENT_SESSIONS": &s.MaxConcurrentSessions, "MAX_TOOL_RESULT_BYTES": &s.MaxToolResultBytes, "MAX_HOOK_OUTPUT_BYTES": &s.MaxHookOutputBytes} {
		if v, ok := os.LookupEnv(name); ok {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 || (name == "SANDBOX_PAUSED_RETENTION_DAYS" && n > 100) || (name == "SANDBOX_CLEANUP_BATCH_SIZE" && n > 2147483647) || (name == "MAX_HOOK_OUTPUT_BYTES" && n < 2) {
				return s, fmt.Errorf("invalid %s", name)
			}
			*dest = n
		}
	}
	if v, ok := os.LookupEnv("DEFAULT_MAX_SESSION_TOKENS"); ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return s, errors.New("invalid DEFAULT_MAX_SESSION_TOKENS")
		}
		s.DefaultMaxSessionTokens = n
	}
	if v, ok := os.LookupEnv("MAX_REQUEST_BYTES"); ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return s, errors.New("invalid MAX_REQUEST_BYTES")
		}
		s.MaxRequestBytes = n
	}
	if s.MaxToolResultBytes < 2 {
		return s, errors.New("MAX_TOOL_RESULT_BYTES must be at least 2")
	}
	for name, dest := range map[string]*time.Duration{"DATABASE_PING_TIMEOUT": &s.DatabasePingTimeout, "CANCEL_GRACE_SECONDS": &s.CancelGrace, "WORKER_POLL_SECONDS": &s.WorkerPoll, "RPC_TIMEOUT_SECONDS": &s.RPCTimeout, "SANDBOX_PROVISION_TIMEOUT_SECONDS": &s.SandboxProvisionTimeout} {
		if v, ok := os.LookupEnv(name); ok {
			d, err := time.ParseDuration(v + "s")
			if err != nil || d <= 0 {
				return s, fmt.Errorf("invalid %s", name)
			}
			*dest = d
		}
	}
	return s, nil
}
func (s Settings) Runtime(api bool) (Deployment, *secret.Cipher, error) {
	if api {
		if _, err := s.BrowserAuth.Validated(); err != nil {
			return Deployment{}, nil, err
		}
	}
	if api && ((s.BrowserAuth.Mode == "api_only" && len(s.PublicAPIKeys) == 0) || slices.Contains(s.PublicAPIKeys, "")) {
		return Deployment{}, nil, errors.New("PUBLIC_API_KEYS must contain nonempty keys")
	}
	p, err := LoadDeployment(s.ConfigFile)
	if err != nil {
		return p, nil, err
	}
	c, err := secret.New(s.EnvEncryptionKey)
	return p, c, err
}
