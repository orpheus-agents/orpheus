// Package config resolves deployment profiles into immutable execution snapshots.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/secret"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/pelletier/go-toml/v2"
)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var accountID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
var reserved = strings.Fields(`ALL_PROXY NO_PROXY HTTP_PROXY HTTPS_PROXY all_proxy no_proxy http_proxy https_proxy HOME CODEX_HOME ORPHEUS_LAUNCH_ID ORPHEUS_HOOK_OPERATION_ID ORPHEUS_SESSION_ID ORPHEUS_WORKSPACE_PATH ORPHEUS_RUN_ID ORPHEUS_INPUT_FINGERPRINT ORPHEUS_AGENT_STATUS ORPHEUS_STOP_REASON ORPHEUS_BROWSER_AUTH ORPHEUS_PUBLIC_URL ACCOUNT_LIMITS_ENABLED SAML_SP_ENTITY_ID SAML_IDP_METADATA_FILE SAML_SP_CERT_FILE SAML_SP_KEY_FILE BROWSER_SESSION_TTL_SECONDS PUBLIC_API_KEYS OPENAI_API_KEY CODEX_API_KEY OPENAI_BASE_URL OPENAI_ORG_ID OPENAI_ORGANIZATION OPENAI_PROJECT_ID CHATGPT_BASE_URL ENV_ENCRYPTION_KEY AGENTBOX_API_KEY`)

type Auth struct {
	Mode      string `toml:"mode"`
	AccountID string `toml:"account_id"`
	APIKeyEnv string `toml:"api_key_env"`
	Store     string `toml:"store"`
	Key       string `toml:"key"`
}
type CodexProfile struct {
	Effort string `toml:"effort"`
}
type Profile struct {
	Harness      string       `toml:"harness"`
	Model        *string      `toml:"model"`
	Codex        CodexProfile `toml:"codex"`
	Instructions string       `toml:"instructions"`
	Auth         Auth         `toml:"auth"`
}
type Profiles struct {
	Profiles         map[string]Profile                 `toml:"profiles"`
	CredentialStores map[string]session.CredentialStore `toml:"credential_stores"`
}

func ReadProfiles(r io.Reader) (Profiles, error) {
	// Presence matters: an omitted default and an explicitly empty value are
	// different, as are forbidden fields in either authentication variant.
	var source struct {
		Profiles map[string]struct {
			Harness string  `toml:"harness"`
			Model   *string `toml:"model"`
			Codex   struct {
				Effort *string `toml:"effort"`
			} `toml:"codex"`
			Instructions string `toml:"instructions"`
			Auth         struct {
				Mode      string  `toml:"mode"`
				AccountID *string `toml:"account_id"`
				APIKeyEnv *string `toml:"api_key_env"`
				Store     *string `toml:"store"`
				Key       *string `toml:"key"`
			} `toml:"auth"`
		} `toml:"profiles"`
		CredentialStores map[string]struct {
			Type        *string `toml:"type"`
			Bucket      string  `toml:"bucket"`
			Region      *string `toml:"region"`
			EndpointURL *string `toml:"endpoint_url"`
		} `toml:"credential_stores"`
	}
	p := Profiles{Profiles: map[string]Profile{}, CredentialStores: map[string]session.CredentialStore{}}
	if err := toml.NewDecoder(r).DisallowUnknownFields().Decode(&source); err != nil {
		return p, err
	}
	if len(source.Profiles) == 0 {
		return p, errors.New("at least one named profile is required")
	}
	for name, raw := range source.CredentialStores {
		s := session.CredentialStore{Type: "s3", Bucket: raw.Bucket, Region: "us-east-1", EndpointURL: raw.EndpointURL}
		if raw.Type != nil {
			s.Type = *raw.Type
		}
		if raw.Region != nil {
			s.Region = *raw.Region
		}
		if s.Type != "s3" || s.Bucket == "" || s.Region == "" || (s.EndpointURL != nil && *s.EndpointURL == "") {
			return p, errors.New("invalid credential store")
		}
		p.CredentialStores[name] = s
	}
	for name, raw := range source.Profiles {
		v := Profile{Harness: raw.Harness, Model: raw.Model, Instructions: raw.Instructions, Auth: Auth{Mode: raw.Auth.Mode}}
		if raw.Codex.Effort != nil {
			v.Codex.Effort = *raw.Codex.Effort
		}
		if strings.TrimSpace(name) == "" || v.Harness != "codex" || (v.Model != nil && *v.Model == "") || (raw.Codex.Effort != nil && !validCodexEffort(v.Codex.Effort)) {
			return p, errors.New("invalid profile")
		}
		switch v.Auth.Mode {
		case "api_key":
			if raw.Auth.APIKeyEnv == nil || !envName.MatchString(*raw.Auth.APIKeyEnv) || raw.Auth.Store != nil || raw.Auth.Key != nil || raw.Auth.AccountID != nil {
				return p, errors.New("invalid API key profile")
			}
			v.Auth.APIKeyEnv = *raw.Auth.APIKeyEnv
		case "account":
			if raw.Auth.Store == nil || *raw.Auth.Store == "" || raw.Auth.Key == nil || *raw.Auth.Key == "" || raw.Auth.APIKeyEnv != nil || raw.Auth.AccountID == nil || len(*raw.Auth.AccountID) > 128 || !accountID.MatchString(*raw.Auth.AccountID) {
				return p, errors.New("invalid account profile")
			}
			v.Auth.Store, v.Auth.Key, v.Auth.AccountID = *raw.Auth.Store, *raw.Auth.Key, *raw.Auth.AccountID
			if _, ok := p.CredentialStores[v.Auth.Store]; !ok {
				return p, errors.New("invalid account profile")
			}
		default:
			return p, errors.New("invalid authentication mode")
		}
		p.Profiles[name] = v
	}
	byID, bySource := map[string]string{}, map[string]string{}
	for _, v := range p.Profiles {
		if v.Auth.Mode != "account" {
			continue
		}
		credentials := session.Credentials{Mode: "account", AccountID: v.Auth.AccountID, Store: new(p.CredentialStores[v.Auth.Store]), Key: v.Auth.Key}
		fingerprint, err := SourceFingerprint(v.Harness, credentials)
		if err != nil {
			return p, err
		}
		if old, ok := byID[v.Auth.AccountID]; ok && old != fingerprint {
			return p, errors.New("account_id refers to multiple credential sources")
		}
		if old, ok := bySource[fingerprint]; ok && old != v.Auth.AccountID {
			return p, errors.New("credential source refers to multiple account_id values")
		}
		byID[v.Auth.AccountID], bySource[fingerprint] = fingerprint, v.Auth.AccountID
	}
	if len(byID) > 1000 {
		return p, errors.New("too many account_id values")
	}
	return p, nil
}

func validCodexEffort(value string) bool {
	return slices.Contains([]string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}, value)
}
func LoadProfiles(path string) (Profiles, error) {
	f, err := os.Open(path)
	if err != nil {
		return Profiles{}, err
	}
	defer func() { _ = f.Close() }()
	return ReadProfiles(f)
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
func ValidateText(text string) error {
	if !session.ValidText(text) {
		return invalid("A nonblank text without NUL is required.", "message", "text")
	}
	return nil
}
func invalid(message string, path ...any) *session.APIError {
	p := session.Problem(422, "validation_error", message)
	p.Problem.Details = []session.Detail{{Path: append([]any{"body"}, path...), Code: "invalid_value"}}
	return p
}
func Resolve(in session.ConfigurationInput, p Profiles, allowlist []string, defaultMaxTokens int64) (session.ResolvedConfiguration, error) {
	var out session.ResolvedConfiguration
	if err := ValidateSandbox(in.Sandbox); err != nil {
		return out, err
	}
	hooks, err := resolveHooks(in.Hooks)
	if err != nil {
		return out, err
	}
	profile, ok := p.Profiles[in.Agent.Profile]
	if !ok {
		problem := invalid("Unknown agent profile.", "configuration", "agent", "profile")
		problem.Problem.Code = "unknown_profile"
		return out, problem
	}
	for _, name := range in.Sandbox.EnvFrom {
		if !slices.Contains(allowlist, name) {
			return out, invalid("Environment reference is not allowed.", "configuration", "sandbox", "env_from")
		}
	}
	model := profile.Model
	if in.Agent.Model != nil {
		model = in.Agent.Model
	}
	if model == nil || strings.TrimSpace(*model) == "" {
		return out, invalid("An explicit model is required.", "configuration", "agent", "model")
	}
	if profile.Codex.Effort != "" && !validCodexEffort(profile.Codex.Effort) {
		return out, invalid("Invalid Codex reasoning effort.", "configuration", "agent", "effort")
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
	creds := session.Credentials{Mode: profile.Auth.Mode, AccountID: profile.Auth.AccountID, APIKeyEnv: profile.Auth.APIKeyEnv, Key: profile.Auth.Key}
	if profile.Auth.Mode == "account" {
		creds.Store = new(p.CredentialStores[profile.Auth.Store])
	}
	names := slices.Sorted(maps.Keys(in.Sandbox.Env))
	if names == nil {
		names = []string{}
	}
	refs := slices.Clone(in.Sandbox.EnvFrom)
	if refs == nil {
		refs = []string{}
	}
	out = session.ResolvedConfiguration{Version: 1, Harness: "codex", Public: session.Configuration{Agent: session.AgentConfiguration{Profile: in.Agent.Profile, Model: *model, Codex: session.CodexConfiguration{Effort: profile.Codex.Effort}, Instructions: instructions}, Sandbox: session.SandboxConfiguration{Template: in.Sandbox.Template, EnvNames: names, EnvFrom: refs}, Limits: in.Limits, Hooks: hooks}, Credentials: creds}
	return out, nil
}
func Environment(cipher *secret.Cipher, id uuid.UUID, token *string, cfg session.Configuration, allowlist []string) (map[string]string, error) {
	return MergedEnvironment(cipher, id, token, cfg, allowlist, nil, nil)
}

// MergedEnvironment resolves selected sources after run-level overrides have
// replaced session-level sources. An overridden reference is never read.
func MergedEnvironment(cipher *secret.Cipher, id uuid.UUID, token *string, cfg session.Configuration, allowlist []string, overrides map[string]string, overrideFrom []string) (map[string]string, error) {
	env, err := cipher.Decrypt(id, token)
	if err != nil {
		return nil, err
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
	BrowserAuth             BrowserAuth
	AccountLimitsEnabled    bool
	DefaultMaxSessionTokens int64
	DatabaseURL             string
	ConfigFile              string
	PublicAPIKeys           []string
	EnvEncryptionKey        string
	HarnessEnvAllowlist     []string
	SandboxProxyURL         string
	MaxConcurrentSessions   int
	MaxToolResultBytes      int
	MaxHookOutputBytes      int
	MaxRequestBytes         int64
	ReadinessTimeout        time.Duration
	CancelGrace             time.Duration
	WorkerPoll              time.Duration
	RPCTimeout              time.Duration
}

func DefaultSettings() Settings {
	return Settings{BrowserAuth: BrowserAuth{Mode: "api_only", SessionTTL: 12 * time.Hour}, AccountLimitsEnabled: true, DefaultMaxSessionTokens: DefaultMaxSessionTokens, ConfigFile: "orpheus.toml", SandboxProxyURL: "socks5h://sandbox-proxy.agentbox.ru:65180", MaxConcurrentSessions: 50, MaxToolResultBytes: 524288, MaxHookOutputBytes: 524288, MaxRequestBytes: 1048576, ReadinessTimeout: 2 * time.Second, CancelGrace: 30 * time.Second, WorkerPoll: time.Second, RPCTimeout: 30 * time.Second}
}
func Load() (Settings, error) {
	s := DefaultSettings()
	if raw, ok := os.LookupEnv("ACCOUNT_LIMITS_ENABLED"); ok {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return s, errors.New("invalid ACCOUNT_LIMITS_ENABLED")
		}
		s.AccountLimitsEnabled = value
	}
	for name, dest := range map[string]*string{"ORPHEUS_BROWSER_AUTH": &s.BrowserAuth.Mode, "ORPHEUS_PUBLIC_URL": &s.BrowserAuth.PublicURL, "SAML_SP_ENTITY_ID": &s.BrowserAuth.EntityID, "SAML_IDP_METADATA_FILE": &s.BrowserAuth.MetadataFile, "SAML_SP_CERT_FILE": &s.BrowserAuth.CertFile, "SAML_SP_KEY_FILE": &s.BrowserAuth.KeyFile, "DATABASE_URL": &s.DatabaseURL, "ORPHEUS_CONFIG_FILE": &s.ConfigFile, "ENV_ENCRYPTION_KEY": &s.EnvEncryptionKey, "SANDBOX_PROXY_URL": &s.SandboxProxyURL} {
		if v, ok := os.LookupEnv(name); ok {
			*dest = v
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
	for name, dest := range map[string]*int{"MAX_CONCURRENT_SESSIONS": &s.MaxConcurrentSessions, "MAX_TOOL_RESULT_BYTES": &s.MaxToolResultBytes, "MAX_HOOK_OUTPUT_BYTES": &s.MaxHookOutputBytes} {
		if v, ok := os.LookupEnv(name); ok {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 || (name == "MAX_HOOK_OUTPUT_BYTES" && n < 2) {
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
	for name, dest := range map[string]*time.Duration{"READINESS_TIMEOUT": &s.ReadinessTimeout, "CANCEL_GRACE_SECONDS": &s.CancelGrace, "WORKER_POLL_SECONDS": &s.WorkerPoll, "RPC_TIMEOUT_SECONDS": &s.RPCTimeout} {
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
func (s Settings) Runtime(api bool) (Profiles, *secret.Cipher, error) {
	if api {
		if _, err := s.BrowserAuth.Validated(); err != nil {
			return Profiles{}, nil, err
		}
	}
	if api && ((s.BrowserAuth.Mode == "api_only" && len(s.PublicAPIKeys) == 0) || slices.Contains(s.PublicAPIKeys, "")) {
		return Profiles{}, nil, errors.New("PUBLIC_API_KEYS must contain nonempty keys")
	}
	p, err := LoadProfiles(s.ConfigFile)
	if err != nil {
		return p, nil, err
	}
	c, err := secret.New(s.EnvEncryptionKey)
	return p, c, err
}
