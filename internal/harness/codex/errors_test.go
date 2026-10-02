package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/orpheus-agents/orpheus/internal/session"
)

func TestNormalizeTurnErrorMessage(t *testing.T) {
	for _, tc := range []struct {
		name, raw, message, code string
	}{
		{"provider", `{"message":"Model request failed","additionalDetails":"HTTP 503: service unavailable","codexErrorInfo":{"httpConnectionFailed":{"httpStatusCode":503}}}`, "Model request failed\nHTTP 503: service unavailable", "harness_failed"},
		{"authentication", `{"message":"Authentication failed: token_expired"}`, "Authentication failed: token_expired", "authentication_failed"},
		{"string", `"Connection closed"`, "Connection closed", "harness_failed"},
		{"details only", `{"message":"  ","additionalDetails":"Upstream failed"}`, "Upstream failed", "harness_failed"},
		{"duplicate details", `{"message":" Failure ","additionalDetails":"Failure"}`, "Failure", "harness_failed"},
		{"null details", `{"message":"Ошибка провайдера","additionalDetails":null}`, "Ошибка провайдера", "harness_failed"},
		{"empty", `{}`, "", "harness_failed"},
		{"blank", `{"message":" \n "}`, "", "harness_failed"},
		{"unknown shape", `{"message":{"unexpected":true}}`, "", "harness_failed"},
		{"malformed", `{`, "", "harness_failed"},
		{"null", `null`, "", ""},
		{"missing", ``, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			thread := Thread{Turns: []NativeTurn{{ID: "turn", Status: "failed", Error: json.RawMessage(tc.raw)}}}
			turn := Normalize(thread, nil, nil, 1024, nil, nil).Turns[0]
			if turn.Status != session.Failed || turn.ErrorCode != tc.code || turn.ErrorMessage != tc.message {
				t.Fatalf("got %+v; want code %q, message %q", turn, tc.code, tc.message)
			}
		})
	}
}

func TestRecoverHistoryPreservesErrorMessage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	history := "{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_started\",\"turn_id\":\"turn\"}}\n" +
		"{\"type\":\"event_msg\",\"payload\":{\"type\":\"error\",\"message\":\"Model request failed\"}}\n"
	if err := os.WriteFile(path, []byte(history), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := recoverHistory(t.Context(), &testBox{}, nil, &path, nil, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Turns) != 1 || snapshot.Turns[0].Status != session.Failed || snapshot.Turns[0].ErrorMessage != "Model request failed" {
		t.Fatalf("lost native error: %+v", snapshot)
	}
}
