//go:build integration

package httpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
)

func TestMessageMetadataContract(t *testing.T) {
	server, s := testServer(t)
	const initial = `{"messages":[{"text":"agent-visible task","external_key":"post:1","metadata":{"source":"mm","nested":{"a":1,"b":[2,3],"big":9007199254740993}}}],"configuration":{"agent":{"profile":"default"},"sandbox":{"template":"codex"}}}`
	key := uuid.NewString()
	a := decodeHTTP[session.Acceptance](t, externalRequest(t, server, "POST", "/api/v1/sessions", initial, key, 202))
	base := "/api/v1/sessions/" + a.SessionID.String()
	if again := decodeHTTP[session.Acceptance](t, externalRequest(t, server, "POST", "/api/v1/sessions", strings.Replace(initial, `"source":"mm","nested":{"a":1,"b":[2,3],"big":9007199254740993}`, `"nested":{"big":9007199254740993,"b":[2,3],"a":1},"source":"mm"`, 1), key, 202)); again != a {
		t.Fatal("equivalent metadata did not replay")
	}
	externalRequest(t, server, "POST", "/api/v1/sessions", strings.Replace(initial, `"source":"mm"`, `"source":"other"`, 1), key, 409)
	check := func(runID uuid.UUID, external, expected string) {
		t.Helper()
		history := decodeHTTP[session.HistoryPage](t, externalRequest(t, server, "GET", base+"/history?run_id="+runID.String()+"&message_external_key="+external, "", "", 200))
		if len(history.Items) != 1 || history.Items[0].Message == nil {
			t.Fatalf("missing metadata history: %+v", history)
		}
		message := history.Items[0].Message
		if message.Text != expected || !json.Valid(message.Metadata) || len(message.Metadata) == 0 || message.Metadata[0] != '{' {
			t.Fatalf("text or metadata changed: %+v", message)
		}
		var object map[string]any
		if err := json.Unmarshal(message.Metadata, &object); err != nil || object["source"] != "mm" {
			t.Fatalf("wrong metadata: %s %v", message.Metadata, err)
		}
		stored, err := store.GetMessage(t.Context(), s.Pool, message.ID)
		if err != nil || !json.Valid(stored.Metadata) || stored.Text != expected {
			t.Fatalf("stored message changed: %+v %v", stored, err)
		}
	}
	check(a.RunID, "post:1", "agent-visible task")
	initialHistory := decodeHTTP[session.HistoryPage](t, externalRequest(t, server, "GET", base+"/history?message_external_key=post%3A1", "", "", 200))
	if !strings.Contains(string(initialHistory.Items[0].Message.Metadata), "9007199254740993") {
		t.Fatal("large JSON number rounded in metadata")
	}
	if err := s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, rec *store.SessionRecord) error {
		r, err := store.GetRun(t.Context(), tx, a.SessionID, a.RunID)
		if err != nil {
			return err
		}
		return store.Finish(t.Context(), tx, rec, &r, session.Completed, nil, nil)
	}); err != nil {
		t.Fatal(err)
	}
	const runBody = `{"messages":[{"text":"second task","external_key":"post:2","metadata":{"source":"mm","step":2}}]}`
	runKey := uuid.NewString()
	b := decodeHTTP[session.Acceptance](t, externalRequest(t, server, "POST", base+"/runs", runBody, runKey, 202))
	check(b.RunID, "post:2", "second task")
	externalRequest(t, server, "POST", base+"/runs", strings.Replace(runBody, `"step":2`, `"step":3`, 1), runKey, 409)
	if err := s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, rec *store.SessionRecord) error {
		r, err := store.GetRun(t.Context(), tx, a.SessionID, b.RunID)
		if err != nil {
			return err
		}
		r.Status = session.Running
		return store.PublishRun(t.Context(), tx, rec, &r)
	}); err != nil {
		t.Fatal(err)
	}
	steerPath := base + "/runs/" + b.RunID.String() + "/messages"
	const steerBody = `{"messages":[{"text":"preceding clarification"},{"text":"clarification","external_key":"post:3","metadata":{"source":"mm","step":3}}]}`
	steerKey := uuid.NewString()
	externalRequest(t, server, "POST", steerPath, steerBody, steerKey, 202)
	check(b.RunID, "post:3", "clarification")
	externalRequest(t, server, "POST", steerPath, strings.Replace(steerBody, `"step":3`, `"step":4`, 1), steerKey, 409)
	events := decodeHTTP[session.EventPage](t, externalRequest(t, server, "GET", base+"/events", "", "", 200))
	seen := map[uuid.UUID]bool{}
	for _, event := range events.Items {
		if event.Type != "message.updated" {
			continue
		}
		var message session.Message
		if err := json.Unmarshal(event.Data, &message); err != nil {
			t.Fatal(err)
		}
		if message.Role == "user" && len(message.Metadata) > 0 && message.Metadata[0] == '{' {
			seen[message.ID] = true
		}
	}
	if len(seen) != 3 {
		t.Fatalf("metadata missing in events: %d", len(seen))
	}
}

func TestMessageMetadataValidation(t *testing.T) {
	server, _ := testServer(t)
	for _, path := range []string{"/api/v1/sessions", "/api/v1/sessions/" + uuid.NewString() + "/runs", "/api/v1/sessions/" + uuid.NewString() + "/runs/" + uuid.NewString() + "/messages"} {
		for _, value := range []string{"null", "[]", `"text"`, "42"} {
			body := `{"messages":[{"text":"hello","metadata":` + value + `}]}`
			if path == "/api/v1/sessions" {
				body = strings.TrimSuffix(body, "}") + `,"configuration":{"agent":{"profile":"default"},"sandbox":{"template":"codex"}}}`
			}
			raw := externalRequest(t, server, "POST", path, body, uuid.NewString(), 422)
			if !strings.Contains(string(raw), `"metadata"`) || !strings.Contains(string(raw), `"invalid_type"`) {
				t.Fatalf("missing metadata validation detail: %s", raw)
			}
		}
	}
	without := decodeHTTP[session.Acceptance](t, externalRequest(t, server, "POST", "/api/v1/sessions", validBody, uuid.NewString(), 202))
	base := "/api/v1/sessions/" + without.SessionID.String()
	history := decodeHTTP[session.HistoryPage](t, externalRequest(t, server, "GET", base+"/history", "", "", 200))
	if len(history.Items) != 1 || history.Items[0].Message == nil || string(history.Items[0].Message.Metadata) != "null" {
		t.Fatal("absent metadata is not null")
	}
	empty := strings.Replace(validBody, `"text":"hello"`, `"text":"hello","metadata":{}`, 1)
	externalRequest(t, server, "POST", "/api/v1/sessions", empty, uuid.NewString(), 202)
}

func TestBatchedMessagesPreserveOrderAndReplay(t *testing.T) {
	server, s := testServer(t)
	const initial = `{"messages":[{"text":"first","external_key":"post:1"},{"text":"second","external_key":"post:2","metadata":{"batch":1}}],"configuration":{"agent":{"profile":"default"},"sandbox":{"template":"codex"}}}`
	key := uuid.NewString()
	a := decodeHTTP[session.Acceptance](t, externalRequest(t, server, "POST", "/api/v1/sessions", initial, key, 202))
	base := "/api/v1/sessions/" + a.SessionID.String()
	if replay := decodeHTTP[session.Acceptance](t, externalRequest(t, server, "POST", "/api/v1/sessions", initial, key, 202)); replay != a {
		t.Fatal("batch replay changed acceptance")
	}
	externalRequest(t, server, "POST", "/api/v1/sessions", strings.Replace(initial, `"text":"first"`, `"text":"changed"`, 1), key, 409)
	externalRequest(t, server, "POST", "/api/v1/sessions", strings.Replace(initial, `{"text":"first","external_key":"post:1"},{"text":"second","external_key":"post:2","metadata":{"batch":1}}`, `{"text":"second","external_key":"post:2","metadata":{"batch":1}},{"text":"first","external_key":"post:1"}`, 1), key, 409)
	invalidSecond := strings.Replace(initial, `"metadata":{"batch":1}`, `"metadata":42`, 1)
	if raw := externalRequest(t, server, "POST", "/api/v1/sessions", invalidSecond, uuid.NewString(), 422); !strings.Contains(string(raw), `["body","messages",1,"metadata"]`) {
		t.Fatalf("missing second-message path: %s", raw)
	}
	invalidText := strings.Replace(initial, `"text":"second"`, `"text":" "`, 1)
	if raw := externalRequest(t, server, "POST", "/api/v1/sessions", invalidText, uuid.NewString(), 422); !strings.Contains(string(raw), `["body","messages",1,"text"]`) {
		t.Fatalf("missing second-text path: %s", raw)
	}
	history := decodeHTTP[session.HistoryPage](t, externalRequest(t, server, "GET", base+"/history?run_id="+a.RunID.String(), "", "", 200))
	if len(history.Items) != 2 || history.Items[0].Message == nil || history.Items[1].Message == nil {
		t.Fatalf("batch history: %+v", history)
	}
	if history.Items[0].Message.Text != "first" || history.Items[1].Message.Text != "second" || history.Items[1].Message.ID != a.MessageID || string(history.Items[0].Message.Metadata) != "null" || string(history.Items[1].Message.Metadata) != `{"batch":1}` {
		t.Fatalf("batch order or metadata changed: %+v", history)
	}
	if err := s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, rec *store.SessionRecord) error {
		r, err := store.GetRun(t.Context(), tx, a.SessionID, a.RunID)
		if err != nil {
			return err
		}
		return store.Finish(t.Context(), tx, rec, &r, session.Completed, nil, nil)
	}); err != nil {
		t.Fatal(err)
	}
	runBody := `{"messages":[{"text":"third"},{"text":"fourth","metadata":{"batch":2}}]}`
	b := decodeHTTP[session.Acceptance](t, externalRequest(t, server, "POST", base+"/runs", runBody, uuid.NewString(), 202))
	runHistory := decodeHTTP[session.HistoryPage](t, externalRequest(t, server, "GET", base+"/history?run_id="+b.RunID.String(), "", "", 200))
	if len(runHistory.Items) != 2 || runHistory.Items[0].Message.Text != "third" || runHistory.Items[1].Message.Text != "fourth" || runHistory.Items[1].Message.ID != b.MessageID {
		t.Fatalf("run batch order changed: %+v", runHistory)
	}
	if raw := externalRequest(t, server, "POST", base+"/runs", `{"messages":[]}`, uuid.NewString(), 422); !strings.Contains(string(raw), `["body","messages"]`) {
		t.Fatalf("missing empty-batch path: %s", raw)
	}
	tooMany := `{"configuration":{"agent":{"profile":"default"},"sandbox":{"template":"codex"}},"messages":[` + strings.Repeat(`{"text":"x"},`, 256) + `{"text":"x"}]}`
	if raw := externalRequest(t, server, "POST", "/api/v1/sessions", tooMany, uuid.NewString(), 422); !strings.Contains(string(raw), `["body","messages"]`) {
		t.Fatalf("missing batch-limit path: %s", raw)
	}
}
