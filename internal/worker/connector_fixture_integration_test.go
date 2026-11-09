//go:build integration

package worker

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/httpserver"
	"github.com/orpheus-agents/orpheus/internal/servicecredentials"
)

type connectorSandbox struct{ *hookRemote }

func (b *connectorSandbox) Create(ctx context.Context, template string, timeout time.Duration, metadata map[string]string) (harness.Sandbox, error) {
	_, err := b.remote.Create(ctx, template, timeout, metadata)
	return b, err
}
func (b *connectorSandbox) Connect(ctx context.Context, id string, timeout time.Duration) (harness.Sandbox, error) {
	_, err := b.remote.Connect(ctx, id, timeout)
	return b, err
}
func (b *connectorSandbox) Start(ctx context.Context, cmd string, env map[string]string, cwd string) (harness.Stream, int, error) {
	stream, pid, err := b.hookRemote.Start(ctx, cmd, env, cwd)
	if err != nil {
		return stream, pid, err
	}
	id := env["ORPHEUS_HOOK_OPERATION_ID"]
	dir := path.Join("/home/template/.orpheus/hooks", id)
	script := string(b.files[path.Join(dir, "script")])
	if strings.Contains(script, "export-output") {
		output, _ := json.Marshal(map[string]any{"schema": 1, "source_id": env["MM_SOURCE_ID"], "bot_id": env["MM_EXPECTED_BOT_ID"], "channel_id": env["MM_CHANNEL_ID"], "session_id": env["ORPHEUS_SESSION_ID"], "run_id": env["ORPHEUS_RUN_ID"], "stage": "ready", "files": []any{}})
		code := 0
		b.files[path.Join(dir, "head.txt")] = output
		result, _ := json.Marshal(hookResultFile{OperationID: id, FinishedAt: time.Now().UTC(), ExitCode: &code, OutputCompleteness: "complete", OriginalBytes: int64(len(output)), HeadFile: "head.txt"})
		b.files[path.Join(dir, "result.json")] = result
	}
	return stream, pid, nil
}

// Long-lived test-only service. Actual Core HTTP/store/executor; sandbox/model
// and hook execution are deterministic fixtures. Source authority is external.
func TestConnectorInteropCoreFixture(t *testing.T) {
	providerURL, observerURL := os.Getenv("INTEROP_PROVIDER_URL"), os.Getenv("INTEROP_OBSERVER_URL")
	if providerURL == "" || observerURL == "" {
		t.Skip("isolated connector interoperability runner")
	}
	s, _, _, unused := setup(t)
	unused.Disconnect()
	s.Profiles.Profiles["test"] = s.Profiles.Profiles["p"]
	s.Settings.PublicAPIKeys = []string{genericCoreKey}
	s.Settings.HarnessEnvAllowlist = []string{"MM_INPUT_MANIFEST", "MM_BASE_URL", "MM_CHANNEL_ID", "MM_ROOT_POST_ID", "MM_SOURCE_ID", "MM_EXPECTED_BOT_ID", "MM_TOKEN_ENV", "TEST_MM_TOKEN"}
	t.Setenv("TEST_MM_TOKEN", "synthetic-mm-token")
	t.Setenv("GENERIC_PROVIDER_KEY", genericProviderKey)
	t.Setenv("GENERIC_SOURCE_KEY", genericSourceKey)
	provider, _ := url.Parse(providerURL)
	observer, _ := url.Parse(observerURL)
	routes := http.NewServeMux()
	routes.Handle("/connector-source", httputil.NewSingleHostReverseProxy(observer))
	routes.Handle("/source-confirmation", httputil.NewSingleHostReverseProxy(observer))
	routes.Handle("/", httputil.NewSingleHostReverseProxy(provider))
	bridge := httptest.NewUnstartedServer(routes)
	_ = bridge.Listener.Close()
	listener, err := net.Listen("tcp", "0.0.0.0:19182")
	if err != nil {
		t.Fatal(err)
	}
	bridge.Listener = listener
	bridge.StartTLS()
	bridge.URL = "https://127.0.0.1:19182"
	defer bridge.Close()
	cfg := servicecredentials.Config{Endpoint: bridge.URL + "/api/v1/agent-credentials/generic-v1", SecretEnv: "GENERIC_PROVIDER_KEY", TokenFile: "~/.kord/run.token", ContextFields: []string{"subject", "scope_context", "context_observed_at", "source"}, MaxLeaseSeconds: 300, MaxRunSeconds: 1200, Confirmation: &servicecredentials.ConfirmationConfig{Endpoint: bridge.URL + "/source-confirmation", SecretEnv: "GENERIC_SOURCE_KEY"}}
	s.Profiles.Services = map[string]config.Service{"kord": {Name: "Kord", Description: "Isolated connector test", Provider: &cfg}}
	s.CredentialClient = servicecredentials.Client{HTTP: bridge.Client()}
	core, err := httpserver.Handler(s, t.Context())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", core)
	var mu sync.Mutex
	boxes := map[uuid.UUID]*connectorSandbox{}
	executors := map[uuid.UUID]*Executor{}
	stop := make(chan struct{})
	var stopOnce sync.Once
	mux.HandleFunc("/fixture/control", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+genericCoreKey {
			w.WriteHeader(401)
			return
		}
		var in struct {
			Operation string    `json:"operation"`
			SessionID uuid.UUID `json:"session_id"`
			RunID     uuid.UUID `json:"run_id"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if in.Operation == "stop" {
			stopOnce.Do(func() { close(stop) })
			_ = json.NewEncoder(w).Encode(map[string]bool{"stopping": true})
			return
		}
		if in.Operation == "ready" {
			_ = json.NewEncoder(w).Encode(map[string]bool{"ready": true})
			return
		}
		e := executors[in.SessionID]
		if e == nil {
			base := &remote{}
			deadlineTokenFiles(base)
			box := &connectorSandbox{&hookRemote{remote: base, files: map[string][]byte{}, autoResult: true}}
			boxes[in.SessionID] = box
			e = executor(in.SessionID, s, base)
			e.Platform = box
			e.RunnerBinary = func(string) ([]byte, error) { return []byte("simulated-hook-runner"), nil }
			executors[in.SessionID] = e
		}
		if in.Operation == "complete" && len(boxes[in.SessionID].turns) > 0 {
			complete(boxes[in.SessionID].remote)
		}
		for range 8 {
			if err := e.Tick(r.Context()); err != nil {
				http.Error(w, "worker fixture failure", 500)
				return
			}
		}
		run, err := s.Run(r.Context(), in.SessionID, in.RunID)
		if err != nil {
			http.Error(w, "run unavailable", 500)
			return
		}
		state, err := s.ServiceCredentials(r.Context(), in.SessionID, in.RunID)
		if err != nil {
			http.Error(w, "credential state unavailable", 500)
			return
		}
		if run.Status.Terminal() {
			if err = s.RevokeTerminalCredentials(r.Context()); err != nil {
				http.Error(w, "revoke pending", 500)
				return
			}
		}
		grant := state.Entries["kord"].Grant
		readOK := false
		if !run.Status.Terminal() && in.Operation == "tick" {
			status, _ := genericPost(t, providerURL+"/authorize", genericProviderKey, map[string]string{"token": grant.AccessToken})
			if status != 200 {
				http.Error(w, "protected fixture read denied", 403)
				return
			}
			readOK = true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"run_status": run.Status, "session_id": in.SessionID, "run_id": in.RunID, "invocation_id": state.Context.InvocationID, "starts": boxes[in.SessionID].starts, "token_present": grant.AccessToken != "", "protected_read_ok": readOK})
	})
	api := httptest.NewUnstartedServer(mux)
	_ = api.Listener.Close()
	listener, err = net.Listen("tcp", "0.0.0.0:19181")
	if err != nil {
		t.Fatal(err)
	}
	api.Listener = listener
	api.StartTLS()
	defer api.Close()
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw})) + string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: bridge.Certificate().Raw}))
	if file := os.Getenv("INTEROP_CA_FILE"); file != "" {
		if err = os.WriteFile(file, []byte(ca), 0600); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.Marshal(map[string]any{"core_callback_origin": os.Getenv("INTEROP_CORE_ORIGIN"), "core_key": genericCoreKey, "source_confirmation_origin": os.Getenv("INTEROP_SOURCE_ORIGIN"), "source_key": genericProviderSourceKey, "ca_pem": ca})
	status, _ := genericPost(t, providerURL+"/fixture/reset", genericProviderKey, json.RawMessage(raw))
	if status != 200 {
		t.Fatal("provider fixture configuration", status)
	}
	ready, _ := json.Marshal(map[string]any{"core_callback_origin": os.Getenv("INTEROP_CORE_ORIGIN"), "ca_pem": ca})
	status, _ = genericPost(t, observerURL+"/fixture/configure", genericProviderKey, json.RawMessage(ready))
	if status != 200 {
		t.Fatal("observer fixture configuration", status)
	}
	if file := os.Getenv("INTEROP_READY_FILE"); file != "" {
		_ = os.WriteFile(file, []byte("ready"), 0600)
	}
	select {
	case <-stop:
	case <-t.Context().Done():
		t.Fatal("interop fixture deadline")
	}
	for _, e := range executors {
		e.Disconnect()
	}
}
