//go:build integration

package httpserver

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skillum-ai/orpheus/internal/harness"
	"github.com/skillum-ai/orpheus/internal/session"
	"github.com/skillum-ai/orpheus/internal/store"
)

func TestTokenBudgetHTTPValidation(t *testing.T) {
	server, s := testServer(t)
	s.Settings.DefaultMaxSessionTokens = 321
	for _, value := range []string{"0", "-1", "null", "true", `"10"`, "1.5", "1.0", "1e3", "9223372036854775808"} {
		body := strings.Replace(validBody, `"agent":`, `"limits":{"max_session_tokens":`+value+`},"agent":`, 1)
		status, _, raw := requestHTTP(t, server, "POST", "/api/v1/sessions", body, "key", uuid.NewString())
		if status != 422 {
			t.Fatalf("%s: %d %s", value, status, raw)
		}
		var problem struct {
			Error session.Error `json:"error"`
		}
		if err := json.Unmarshal(raw, &problem); err != nil || len(problem.Error.Details) != 1 {
			t.Fatalf("%s: missing error detail: %s (%v)", value, raw, err)
		}
		path, err := json.Marshal(problem.Error.Details[0].Path)
		if err != nil || string(path) != `["body","configuration","limits","max_session_tokens"]` {
			t.Fatalf("%s: incorrect error path: %s (%v)", value, path, err)
		}
	}
	for _, tc := range []struct {
		value string
		want  int64
	}{{"", 321}, {"1", 1}, {"9223372036854775807", 9223372036854775807}} {
		body := validBody
		if tc.value != "" {
			body = strings.Replace(body, `"agent":`, `"limits":{"max_session_tokens":`+tc.value+`},"agent":`, 1)
		}
		status, _, raw := requestHTTP(t, server, "POST", "/api/v1/sessions", body, "key", uuid.NewString())
		if status != 202 {
			t.Fatalf("%s: %d %s", tc.value, status, raw)
		}
		var a session.Acceptance
		if err := json.Unmarshal(raw, &a); err != nil {
			t.Fatal(err)
		}
		_, _, raw = requestHTTP(t, server, "GET", "/api/v1/sessions/"+a.SessionID.String(), "", "key", "")
		var view session.Session
		if err := json.Unmarshal(raw, &view); err != nil || view.Configuration.Limits.MaxSessionTokens != tc.want || view.Usage != (session.Usage{}) {
			t.Fatal(view, err)
		}
	}
}

func TestTokenBudgetHTTPProjectionAndIdempotency(t *testing.T) {
	server, s := testServer(t)
	s.Settings.DefaultMaxSessionTokens = 100
	key := uuid.NewString()
	status, _, raw := requestHTTP(t, server, "POST", "/api/v1/sessions", validBody, "key", key)
	if status != 202 {
		t.Fatal(status, string(raw))
	}
	var a session.Acceptance
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/sessions/" + a.SessionID.String()
	if err := s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, r *store.SessionRecord) error {
		r.ThreadID = new("thread")
		run, err := store.GetRun(t.Context(), tx, a.SessionID, a.RunID)
		if err != nil {
			return err
		}
		run.NativeTurnID = new("turn")
		run.Status = session.Running
		run.ExecutionStartedAt = new(time.Now())
		return store.SaveRun(t.Context(), tx, &run)
	}); err != nil {
		t.Fatal(err)
	}
	steerKey := uuid.NewString()
	steerPath := path + "/runs/" + a.RunID.String() + "/messages"
	status, _, steer := requestHTTP(t, server, "POST", steerPath, `{"message":{"text":"clarification"}}`, "key", steerKey)
	if status != 202 {
		t.Fatal(status, string(steer))
	}
	want := session.Usage{InputTokens: 90, OutputTokens: 10, TotalTokens: 100}
	if err := s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, r *store.SessionRecord) error {
		return store.ApplyUsage(t.Context(), tx, r, []harness.UsageReport{{ContextID: "thread", TurnID: "turn", Total: want}})
	}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + "/runs/" + a.RunID.String(), path + "/runs", "/api/v1/runs", "/api/v1/sessions", path + "/events"} {
		status, _, raw = requestHTTP(t, server, "GET", p, "", "key", "")
		if status != 200 || !strings.Contains(string(raw), `"total_tokens":100`) {
			t.Fatal(p, status, string(raw))
		}
	}
	for _, p := range []string{path + "/runs", steerPath} {
		status, _, raw = requestHTTP(t, server, "POST", p, `{"message":{"text":"again"}}`, "key", uuid.NewString())
		if status != 409 || !strings.Contains(string(raw), "token_limit_exceeded") {
			t.Fatal(status, string(raw))
		}
	}
	s.Settings.DefaultMaxSessionTokens = 200
	status, _, raw = requestHTTP(t, server, "POST", "/api/v1/sessions", validBody, "key", key)
	if status != 202 {
		t.Fatal(status, string(raw))
	}
	status, _, raw = requestHTTP(t, server, "POST", steerPath, `{"message":{"text":"clarification"}}`, "key", steerKey)
	if status != 202 || string(raw) != string(steer) {
		t.Fatal(status, string(raw))
	}
	view, err := s.Session(t.Context(), a.SessionID)
	if err != nil || view.Configuration.Limits.MaxSessionTokens != 100 {
		t.Fatal(fmt.Sprint(view), err)
	}
}
