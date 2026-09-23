//go:build integration

package httpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/jackc/pgx/v5"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/api"
	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/harness/codex"
	"github.com/orpheus-agents/orpheus/internal/secret"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
	"github.com/orpheus-agents/orpheus/internal/testutil"
)

func testServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	c, _ := secret.New(base64.URLEncoding.EncodeToString(make([]byte, 32)))
	s := &store.Store{Pool: testutil.Database(t), Cipher: c, Settings: config.DefaultSettings(), Profiles: config.Profiles{Profiles: map[string]config.Profile{"default": {Harness: "codex", Model: new("fixture"), Instructions: "profile instruction", Auth: config.Auth{Mode: "api_key", APIKeyEnv: "OPENAI_API_KEY"}}}}}
	s.Settings.PublicAPIKeys = []string{"key"}
	handler, err := Handler(s, t.Context())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server, s
}
func requestHTTP(t *testing.T, server *httptest.Server, method, path, body, token, key string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if path != "/openapi.json" {
		spec, err := api.GetSpec()
		if err != nil {
			t.Fatal(err)
		}
		spec.Servers = nil
		router, err := legacy.NewRouter(spec)
		if err != nil {
			t.Fatal(err)
		}
		route, params, err := router.FindRoute(req)
		if err == nil {
			input := &openapi3filter.ResponseValidationInput{RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route}, Status: res.StatusCode, Header: res.Header, Body: io.NopCloser(bytes.NewReader(raw))}
			if err := openapi3filter.ValidateResponse(t.Context(), input); err != nil {
				t.Fatalf("response violates contract: %v", err)
			}
		}
	}
	return res.StatusCode, res.Header, raw
}

const validBody = `{"configuration":{"agent":{"profile":"default"},"sandbox":{"template":"codex"}},"message":{"text":"hello"}}`

func TestHTTPContract(t *testing.T) {
	server, _ := testServer(t)
	for _, path := range []string{"/health", "/ready", "/openapi.json"} {
		status, _, body := requestHTTP(t, server, "GET", path, "", "", "")
		if status != 200 {
			t.Fatalf("%s %d %s", path, status, body)
		}
	}
	status, h, _ := requestHTTP(t, server, "GET", "/api/v1/sessions", "", "bad", "")
	if status != 401 || h.Get("WWW-Authenticate") != "Bearer" {
		t.Fatal(status, h)
	}
	key := uuid.NewString()
	status, h, raw := requestHTTP(t, server, "POST", "/api/v1/sessions", validBody, "key", key)
	if status != 202 {
		t.Fatalf("create: %d %s", status, raw)
	}
	var accepted session.Acceptance
	if err := json.Unmarshal(raw, &accepted); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/sessions/" + accepted.SessionID.String()
	if h.Get("Location") != path+"/runs/"+accepted.RunID.String() {
		t.Fatal(h)
	}
	status, _, again := requestHTTP(t, server, "POST", "/api/v1/sessions", validBody, "key", key)
	if status != 202 || !bytes.Equal(raw, again) {
		t.Fatal(status, string(again))
	}
	for _, suffix := range []string{"", "/runs", "/runs/" + accepted.RunID.String(), "/events", "/history"} {
		status, _, raw = requestHTTP(t, server, "GET", path+suffix, "", "key", "")
		if status != 200 {
			t.Fatalf("%s: %d %s", suffix, status, raw)
		}
	}
	_, _, raw = requestHTTP(t, server, "GET", path, "", "key", "")
	var record map[string]any
	_ = json.Unmarshal(raw, &record)
	if v, ok := record["final_message"]; !ok || v != nil {
		t.Fatalf("null final_message lost: %s", raw)
	}
	// Explicit empty override remains distinguishable from an omitted override.
	empty := strings.Replace(validBody, `"profile":"default"`, `"profile":"default","instructions":""`, 1)
	status, _, raw = requestHTTP(t, server, "POST", "/api/v1/sessions", empty, "key", key)
	if status != 409 {
		t.Fatalf("override fingerprint: %d %s", status, raw)
	}
	status, _, raw = requestHTTP(t, server, "POST", h.Get("Location")+"/cancel", "", "key", "")
	if status != 200 {
		t.Fatal(status, string(raw))
	}
}

func TestHealthAndReadinessAfterDatabaseLoss(t *testing.T) {
	server, s := testServer(t)
	s.Pool.Close()
	status, _, _ := requestHTTP(t, server, "GET", "/ready", "", "", "")
	if status != 503 {
		t.Fatal("readiness ignored lost database", status)
	}
	status, _, _ = requestHTTP(t, server, "GET", "/health", "", "", "")
	if status != 200 {
		t.Fatal("health depends on database", status)
	}
	for _, path := range []string{"/docs", "/redoc"} {
		status, _, _ := requestHTTP(t, server, "GET", path, "", "", "")
		if status != 404 {
			t.Fatal("documentation endpoint enabled", path, status)
		}
	}
}
func TestRequestBoundary(t *testing.T) {
	server, s := testServer(t)
	tests := []struct {
		body   string
		status int
	}{{`{`, 400}, {validBody + "{}", 400}, {strings.Replace(validBody, `"text":"hello"`, `"text":"hello","text":"secret"`, 1), 400}, {strings.Replace(validBody, `"text":"hello"`, `"text":12`, 1), 422}, {strings.Replace(validBody, `"profile":"default"`, `"profile":"default","unknown":"secret"`, 1), 422}, {strings.Replace(validBody, `"profile":"default"`, `"profile":"default","model":null`, 1), 422}, {strings.Replace(validBody, `"template":"codex"`, `"template":"codex","env":null`, 1), 422}, {strings.Replace(validBody, `"text":"hello"`, `"text":" "`, 1), 422}, {strings.Replace(validBody, `"text":"hello"`, `"text":NaN`, 1), 400}, {strings.Replace(validBody, "hello", string([]byte{0xff}), 1), 400}}
	for i, tc := range tests {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			status, _, raw := requestHTTP(t, server, "POST", "/api/v1/sessions", tc.body, "key", uuid.NewString())
			if status != tc.status {
				t.Fatalf("want %d got %d %s", tc.status, status, raw)
			}
			if bytes.Contains(raw, []byte("secret")) {
				t.Fatal("validation leaked input")
			}
		})
	}
	s.Settings.MaxRequestBytes = 20
	status, _, _ := requestHTTP(t, server, "POST", "/api/v1/sessions", validBody, "key", uuid.NewString())
	if status != 413 {
		t.Fatal(status)
	}
}
func TestSSE(t *testing.T) {
	server, _ := testServer(t)
	_, _, raw := requestHTTP(t, server, "POST", "/api/v1/sessions", validBody, "key", uuid.NewString())
	var a session.Acceptance
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), "GET", server.URL+"/api/v1/sessions/"+a.SessionID.String()+"/events/stream?after=999", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer key")
	req.Header.Set("Last-Event-ID", "1")
	client := &http.Client{Timeout: 3 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != 200 {
		raw, _ := io.ReadAll(res.Body)
		t.Fatalf("%d %s", res.StatusCode, raw)
	}
	buf := make([]byte, 4096)
	n, err := res.Body.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(buf[:n], []byte("id: 2\nevent: run.updated")) {
		t.Fatalf("%s", buf[:n])
	}
}

func TestValidationDetailsAndAdditionalBoundaries(t *testing.T) {
	server, _ := testServer(t)
	for _, tc := range []struct {
		name, body, key, path string
		code                  int
		detailPath            string
	}{
		{"type", strings.Replace(validBody, `"text":"hello"`, `"text":12`, 1), uuid.NewString(), "/api/v1/sessions", 422, `["body","message","text"]`},
		{"missing header", validBody, "", "/api/v1/sessions", 422, `["header","Idempotency-Key"]`},
		{"empty model", strings.Replace(validBody, `"profile":"default"`, `"profile":"default","model":""`, 1), uuid.NewString(), "/api/v1/sessions", 422, `["body","configuration","agent","model"]`},
		{"null instructions", strings.Replace(validBody, `"profile":"default"`, `"profile":"default","instructions":null`, 1), uuid.NewString(), "/api/v1/sessions", 422, `["body","configuration","agent","instructions"]`},
		{"cancel body", `{}`, "", "/api/v1/sessions/" + uuid.NewString() + "/runs/" + uuid.NewString() + "/cancel", 400, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, _, raw := requestHTTP(t, server, "POST", tc.path, tc.body, "key", tc.key)
			if status != tc.code {
				t.Fatalf("status %d: %s", status, raw)
			}
			if tc.detailPath != "" {
				var problem struct {
					Error session.Error `json:"error"`
				}
				if err := json.Unmarshal(raw, &problem); err != nil {
					t.Fatal(err)
				}
				if len(problem.Error.Details) == 0 {
					t.Fatal("missing details", string(raw))
				}
				path, _ := json.Marshal(problem.Error.Details[0].Path)
				if string(path) != tc.detailPath {
					t.Fatalf("path %s; want %s (%s)", path, tc.detailPath, raw)
				}
			}
		})
	}
	req, err := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/api/v1/sessions", strings.NewReader(validBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer key")
	req.Header.Set("Content-Type", "text/plain")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != 415 {
		t.Fatal(res.StatusCode)
	}
}

func TestSSEKeepaliveAndDisconnectReleaseHandler(t *testing.T) {
	apiServer, s := testServer(t)
	_, _, raw := requestHTTP(t, apiServer, "POST", "/api/v1/sessions", validBody, "key", uuid.NewString())
	var a session.Acceptance
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	page, err := s.Events(t.Context(), a.SessionID, "0", 100)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e := eventStream{ctx: r.Context(), streams: t.Context(), store: s, id: a.SessionID, after: page.NextCursor}
		done <- e.VisitStreamEventsResponse(w)
	}))
	defer server.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	res, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(res.Body).ReadString('\n')
	if err != nil || line != ": keep-alive\n" {
		t.Error(line, err)
	}
	if s.Pool.Stat().AcquiredConns() != 0 {
		t.Error("SSE kept a database connection while idle")
	}
	_ = res.Body.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("disconnect did not stop SSE")
	}
}

func TestShutdownClosesSSEAndDrainsOrdinaryRequest(t *testing.T) {
	apiServer, s := testServer(t)
	_, _, raw := requestHTTP(t, apiServer, "POST", "/api/v1/sessions", validBody, "key", uuid.NewString())
	var a session.Acceptance
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	streams, stopStreams := context.WithCancel(t.Context())
	defer stopStreams()
	handler, err := Handler(s, streams)
	if err != nil {
		t.Fatal(err)
	}
	started, release := make(chan context.Context, 1), make(chan struct{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ordinary" {
			started <- r.Context()
			<-release
			w.WriteHeader(http.StatusNoContent)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	server.Config.RegisterOnShutdown(stopStreams)
	server.Start()
	defer server.Close()
	req, err := http.NewRequestWithContext(t.Context(), "GET", server.URL+"/api/v1/sessions/"+a.SessionID.String()+"/events/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer key")
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatal(res.Status)
	}
	streamDone := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, res.Body); streamDone <- err }()
	response := make(chan error, 1)
	go func() {
		r, err := server.Client().Get(server.URL + "/ordinary")
		if err == nil {
			_ = r.Body.Close()
			if r.StatusCode != http.StatusNoContent {
				err = fmt.Errorf("status %d", r.StatusCode)
			}
		}
		response <- err
	}()
	requestCtx := <-started
	shutdown, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Config.Shutdown(shutdown) }()
	select {
	case <-streamDone:
	case <-shutdown.Done():
		t.Error("SSE did not close during shutdown")
	}
	if err := requestCtx.Err(); err != nil {
		t.Error("ordinary request cancelled", err)
	}
	close(release)
	if err := <-response; err != nil {
		t.Error(err)
	}
	if err := <-done; err != nil {
		t.Error("shutdown failed", err)
	}
}

func TestProjectedHistoryAndResultContract(t *testing.T) {
	server, s := testServer(t)
	_, _, raw := requestHTTP(t, server, "POST", "/api/v1/sessions", validBody, "key", uuid.NewString())
	var a session.Acceptance
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	err := s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, record *store.SessionRecord) error {
		values := []json.RawMessage{json.RawMessage(`"short"`), json.RawMessage(`{"stdout":"output","nested":[1,2]}`), json.RawMessage(`"` + strings.Repeat("я", 200) + `"`)}
		for i, value := range values {
			out := codex.Bounded(value, 100, new(0), false)
			tool := store.ToolRecord{ToolCall: session.ToolCall{ID: uuid.New(), SessionID: a.SessionID, RunID: a.RunID, Name: "command", Input: json.RawMessage(`["echo","test"]`), Status: "completed", Result: out.Result, OutputCompleteness: out.Completeness, TruncationReason: out.Reason, Position: &session.Position{RunNumber: 1, ItemIndex: i}}}
			if err := store.PublishTool(t.Context(), tx, record, &tool); err != nil {
				return err
			}
		}
		for i, kind := range []string{"progress", "answer"} {
			message := store.MessageRecord{Message: session.Message{ID: uuid.New(), SessionID: a.SessionID, RunID: a.RunID, Role: "assistant", Kind: &kind, Text: kind, Position: &session.Position{RunNumber: 1, ItemIndex: i + 3}}}
			if err := store.PublishMessage(t.Context(), tx, record, &message); err != nil {
				return err
			}
		}
		run, err := store.GetRun(t.Context(), tx, a.SessionID, a.RunID)
		if err != nil {
			return err
		}
		return store.Finish(t.Context(), tx, record, &run, session.Completed, nil, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/sessions/" + a.SessionID.String()
	for _, suffix := range []string{"", "/runs", "/runs/" + a.RunID.String(), "/history", "/events"} {
		status, _, raw := requestHTTP(t, server, "GET", path+suffix, "", "key", "")
		if status != 200 {
			t.Fatalf("%s: %d %s", suffix, status, raw)
		}
	}
	status, _, raw := requestHTTP(t, server, "POST", path+"/runs", `{"message":{"text":"next"}}`, "key", uuid.NewString())
	if status != 202 {
		t.Fatal(status, string(raw))
	}
	var next session.Acceptance
	if err := json.Unmarshal(raw, &next); err != nil {
		t.Fatal(err)
	}
	if err := s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, record *store.SessionRecord) error {
		run, err := store.GetRun(t.Context(), tx, a.SessionID, next.RunID)
		if err != nil {
			return err
		}
		run.Status = session.Running
		run.NativeTurnID = new("next-turn")
		return store.PublishRun(t.Context(), tx, record, &run)
	}); err != nil {
		t.Fatal(err)
	}
	status, _, raw = requestHTTP(t, server, "POST", path+"/runs/"+next.RunID.String()+"/messages", `{"message":{"text":"steer"}}`, "key", uuid.NewString())
	if status != 202 {
		t.Fatal(status, string(raw))
	}
}
