package opencode

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/diagnostic"
	"github.com/orpheus-agents/orpheus/internal/session"
)

func TestModelRef(t *testing.T) {
	for _, test := range []struct{ account, model, provider, id string }{
		{"opencode", "model", "opencode", "model"}, {"opencode", "opencode-go/k", "opencode-go", "k"}, {"opencode", "opencode/k", "opencode", "k"}, {"yandex-cloud", "gpt://folder/model", "yandex-cloud", "gpt://folder/model"}, {"custom", "opencode-go/k", "custom", "opencode-go/k"},
	} {
		p, id := modelRef(test.account, test.model)
		if p != test.provider || id != test.id {
			t.Fatal(p, id)
		}
	}
}
func TestProjectionUsesNativeMessageIDsAndCumulativeUsage(t *testing.T) {
	sid, run, mid, op := uuid.New(), uuid.New(), uuid.NewString(), uuid.NewString()
	redactor := new(diagnostic.Redactor)
	redactor.Add("sensitive-key")
	d := New(nil, time.Second, 16, redactor, sid, run, "template-fixture", "provider-fixture", "model-fixture")
	var raw projection
	if err := json.Unmarshal([]byte(`{"run":"`+run.String()+`","fenced":true,"tokens":{"input":7,"output":8,"reasoning":2,"cache":{"read":4,"write":1}},"messages":[{"id":"msg_`+mid+`","type":"user","metadata":{"orpheus_run_id":"`+run.String()+`","orpheus_operation_id":"`+op+`"},"text":"same"},{"id":"step","type":"assistant","finish":"tool-calls","content":[{"type":"reasoning","text":"investigating"},{"type":"tool","id":"call1","name":"shell","state":{"status":"completed","input":{"env":"sensitive-key"},"content":["0123456789ABCDEFGHIJKLMN"]}},{"type":"tool","id":"call2","name":"shell","state":{"status":"running","input":{"x":1}}}]},{"id":"auto","type":"compaction","status":"completed"},{"id":"final","type":"assistant","finish":"stop","content":[{"type":"text","text":"answer sensitive-key https://example.com"}]},{"id":"idle","type":"idle","outcome":"succeeded"}]}`), &raw); err != nil {
		t.Fatal(err)
	}
	snap, err := d.normalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	turn := snap.Turns[0]
	u := snap.Usage[0]
	if snap.FencedRun != run.String() || turn.Status != session.Completed || turn.Items[0].InputID != mid || turn.Items[0].OperationID != op || len(turn.Items) != 5 {
		t.Fatalf("projection: %+v", turn)
	}
	if turn.Items[2].NativeID == turn.Items[3].NativeID || turn.Items[2].Completeness != "truncated" || strings.Contains(string(turn.Items[2].Input), "sensitive-key") || turn.Items[3].Status != "running" {
		t.Fatal(turn.Items)
	}
	if *turn.Items[1].Kind != "progress" || *turn.Items[4].Kind != "answer" || turn.Items[4].Text != "answer [REDACTED] https://example.com" {
		t.Fatal(turn.Items)
	}
	if u.ContextID != d.session || u.TurnID != run.String() || u.Total.InputTokens != 12 || u.Total.OutputTokens != 10 || u.Total.TotalTokens != 22 || *u.CachedInputTokens != 4 || *u.ReasoningOutputTokens != 2 {
		t.Fatal(u)
	}
	raw.Messages[len(raw.Messages)-1].Outcome = "failed"
	raw.Messages[len(raw.Messages)-1].Error = &nativeError{Status: 401, Message: "invalid sensitive-key"}
	snap, err = d.normalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Turns[0].ErrorCode != "authentication_failed" || strings.Contains(snap.Turns[0].ErrorMessage, "sensitive-key") || !strings.Contains(snap.Turns[0].ErrorMessage, "model-fixture") {
		t.Fatal(snap.Turns[0])
	}
	raw.Messages[0].Metadata.Run = uuid.NewString()
	if _, err = d.normalize(raw); err == nil {
		t.Fatal("accepted cross-run native input")
	}
}
