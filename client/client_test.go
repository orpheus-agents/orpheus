package client_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/client"
)

var sessionID = uuid.MustParse("00000000-0000-4000-8000-000000000001")
var runID = uuid.MustParse("00000000-0000-4000-8000-000000000002")
var messageID = uuid.MustParse("00000000-0000-4000-8000-000000000003")

func testClient(t *testing.T, handler http.HandlerFunc) *client.ClientWithResponses {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c, err := client.NewClientWithResponses(server.URL+"/gateway", client.WithHTTPClient(server.Client()), client.WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer test-token")
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type acceptedResponse interface {
	GetJSON202() *client.Accepted
	StatusCode() int
}

func TestAcceptedRequests(t *testing.T) {
	key := uuid.NewString()
	base := "/gateway/api/v1/sessions"
	cases := []struct {
		name, path, body string
		call             func(context.Context, *client.ClientWithResponses) (acceptedResponse, error)
	}{
		{"session", base, `{"namespace":"mattermost/test","external_key":"thread:1","configuration":{"agent":{"profile":"default","instructions":""},"sandbox":{"template":"test"}},"messages":[{"text":"hello","external_key":"post:1"}],"env":{"INPUT":"value"},"env_from":["BOT_TOKEN"]}`,
			func(ctx context.Context, c *client.ClientWithResponses) (acceptedResponse, error) {
				return c.CreateSessionWithResponse(ctx, &client.CreateSessionParams{IdempotencyKey: &key}, client.CreateSession{
					Namespace: new("mattermost/test"), ExternalKey: new("thread:1"),
					Configuration: client.ConfigurationInput{Agent: client.AgentInput{Profile: "default", Instructions: new("")}, Sandbox: client.SandboxInput{Template: "test"}},
					Messages:      []client.TextMessage{{Text: "hello", ExternalKey: new("post:1")}}, Env: &map[string]string{"INPUT": "value"}, EnvFrom: &[]string{"BOT_TOKEN"},
				})
			}},
		{"run", base + "/" + sessionID.String() + "/runs", `{"messages":[{"text":"next"}],"input_fingerprint":"revision:2"}`,
			func(ctx context.Context, c *client.ClientWithResponses) (acceptedResponse, error) {
				return c.CreateRunWithResponse(ctx, sessionID, &client.CreateRunParams{IdempotencyKey: &key}, client.CreateRun{Messages: []client.TextMessage{{Text: "next"}}, InputFingerprint: new("revision:2")})
			}},
		{"clarification", base + "/" + sessionID.String() + "/runs/" + runID.String() + "/messages", `{"messages":[{"text":"clarify","external_key":"post:2"}]}`,
			func(ctx context.Context, c *client.ClientWithResponses) (acceptedResponse, error) {
				return c.SendMessageWithResponse(ctx, sessionID, runID, &client.SendMessageParams{IdempotencyKey: &key}, client.SendMessage{Messages: []client.TextMessage{{Text: "clarify", ExternalKey: new("post:2")}}})
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != tc.path || r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("Idempotency-Key") != key || r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("unexpected request: %s %s %v", r.Method, r.URL, r.Header)
				}
				var got, want any
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Error(err)
				}
				if err := json.Unmarshal([]byte(tc.body), &want); err != nil {
					t.Error(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("body: got %v, want %v", got, want)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Location", "/api/v1/sessions/"+sessionID.String()+"/runs/"+runID.String())
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(client.Accepted{SessionID: sessionID, RunID: runID, MessageID: messageID})
			})
			res, err := tc.call(t.Context(), c)
			if err != nil {
				t.Fatal(err)
			}
			if res.StatusCode() != http.StatusAccepted || res.GetJSON202() == nil || res.GetJSON202().MessageID != messageID || requests.Load() != 1 {
				t.Fatalf("acceptance: %#v; requests: %d", res, requests.Load())
			}
			if typed, ok := res.(*client.CreateSessionHTTPResponse); ok && (typed.Headers202 == nil || !strings.HasSuffix(typed.Headers202.Location, runID.String())) {
				t.Fatal("missing Location", typed.Headers202)
			}
		})
	}
}

func TestPaginationAndQueryEncoding(t *testing.T) {
	cursor := "opaque+/=?& cursor"
	namespace := "workflow/a & b"
	var requests atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		q := r.URL.Query()
		if r.URL.Path != "/gateway/api/v1/runs" || q.Get("namespace") != namespace || q.Get("external_key") != "source:тред/1" || q.Get("status") != "completed" || q.Get("order") != "asc" || q.Get("limit") != "1" || q.Get("input_fingerprint") != "sha256:abc" {
			t.Errorf("query: %s", r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		switch q.Get("cursor") {
		case "":
			_ = json.NewEncoder(w).Encode(client.RunPage{Items: []client.Run{{ID: runID, SessionID: sessionID, Status: client.RunStatusCompleted}}, NextCursor: &cursor})
		case cursor:
			_ = json.NewEncoder(w).Encode(client.RunPage{Items: []client.Run{}, NextCursor: nil})
		default:
			t.Errorf("unexpected cursor: %q", q.Get("cursor"))
		}
	})
	params := &client.ListAllRunsParams{Namespace: &namespace, ExternalKey: new("source:тред/1"), Status: new(client.RunStatusCompleted), Order: new(client.ListAllRunsParamsOrderAsc), Limit: new(1), InputFingerprint: new("sha256:abc")}
	first, err := c.ListAllRunsWithResponse(t.Context(), params)
	if err != nil {
		t.Fatal(err)
	}
	if first.JSON200 == nil || first.JSON200.NextCursor == nil || *first.JSON200.NextCursor != cursor || len(first.JSON200.Items) != 1 || first.JSON200.Items[0].ID != runID || requests.Load() != 1 {
		t.Fatalf("first page: %#v", first)
	}
	params.Cursor = first.JSON200.NextCursor
	last, err := c.ListAllRunsWithResponse(t.Context(), params)
	if err != nil {
		t.Fatal(err)
	}
	if last.JSON200 == nil || last.JSON200.NextCursor != nil || len(last.JSON200.Items) != 0 || requests.Load() != 2 {
		t.Fatalf("last page: %#v", last)
	}
}

func TestHTTPFailuresAndMalformedResponses(t *testing.T) {
	for _, status := range []int{401, 409, 422, 503, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var requests atomic.Int32
			c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "5")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"code":"fixture_error","message":"test","phase":null,"details":[]}}`))
			})
			res, err := c.GetSessionWithResponse(t.Context(), sessionID)
			if err != nil {
				t.Fatal(err)
			}
			if res.StatusCode() != status || res.HTTPResponse.Header.Get("Retry-After") != "5" || len(res.Body) == 0 || requests.Load() != 1 {
				t.Fatalf("response: %#v", res)
			}
			problem := map[int]*client.ErrorResponse{401: res.JSON401, 409: res.JSON409, 422: res.JSON422, 503: res.JSON503}[status]
			if status != 429 && (problem == nil || problem.Error.Code != "fixture_error") {
				t.Fatalf("missing typed error: %#v", res)
			}
		})
	}
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":`))
	})
	if _, err := c.GetSessionWithResponse(t.Context(), sessionID); err == nil {
		t.Fatal("malformed JSON accepted")
	}
}

func TestStreamingReturnsBeforeEOFAndCancels(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Last-Event-ID") != "42" || r.URL.Query().Get("after") != "1" || !strings.HasSuffix(r.URL.Path, "/"+sessionID.String()+"/events/stream") {
			t.Errorf("stream request: %s %v", r.URL, r.Header)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "id: 43\nevent: run.updated\ndata: {}\n\n")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Error(err)
		}
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	res, err := c.StreamEvents(ctx, sessionID, &client.StreamEventsParams{LastEventID: new("42"), After: new("1")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatal(res.Status)
	}
	reader := bufio.NewReader(res.Body)
	for _, want := range []string{"id: 43\n", "event: run.updated\n", "data: {}\n", "\n"} {
		line, err := reader.ReadString('\n')
		if err != nil || line != want {
			t.Fatalf("stream: %q, %v", line, err)
		}
	}
	cancel()
	if _, err := reader.ReadByte(); !errors.Is(err, context.Canceled) {
		t.Fatalf("stream ignored cancellation: %v", err)
	}
}

func TestCancelledContextStopsRequest(t *testing.T) {
	c := testClient(t, func(http.ResponseWriter, *http.Request) { t.Error("request sent after cancellation") })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.GetSessionWithResponse(ctx, sessionID); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
