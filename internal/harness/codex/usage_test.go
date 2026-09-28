package codex

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/orpheus-agents/orpheus/internal/session"
)

func TestParseUsage(t *testing.T) {
	good := `{"threadId":"thread","turnId":"turn","tokenUsage":{"total":{"inputTokens":90,"outputTokens":10,"totalTokens":100,"cachedInputTokens":80,"reasoningOutputTokens":7},"last":{"totalTokens":9999}}}`
	got, ok := parseUsage(json.RawMessage(good))
	if !ok || got.Total != (session.Usage{InputTokens: 90, OutputTokens: 10, TotalTokens: 100}) || got.CachedInputTokens == nil || *got.CachedInputTokens != 80 || got.ReasoningOutputTokens == nil || *got.ReasoningOutputTokens != 7 {
		t.Fatal(got, ok)
	}
	zero, ok := parseUsage(json.RawMessage(`{"threadId":"thread","turnId":"turn","tokenUsage":{"total":{"inputTokens":0,"cachedInputTokens":0,"outputTokens":0,"reasoningOutputTokens":0,"totalTokens":0}}}`))
	if !ok || zero.CachedInputTokens == nil || *zero.CachedInputTokens != 0 || zero.ReasoningOutputTokens == nil || *zero.ReasoningOutputTokens != 0 {
		t.Fatal(zero, ok)
	}
	for _, raw := range []string{
		`{"inputTokens":90,"outputTokens":10,"totalTokens":100}`,
		`{"inputTokens":90,"cachedInputTokens":null,"outputTokens":10,"reasoningOutputTokens":7,"totalTokens":100}`,
		`{"inputTokens":90,"cachedInputTokens":-1,"outputTokens":10,"reasoningOutputTokens":7,"totalTokens":100}`,
		`{"inputTokens":90,"cachedInputTokens":91,"outputTokens":10,"reasoningOutputTokens":7,"totalTokens":100}`,
		`{"inputTokens":90,"cachedInputTokens":"bad","outputTokens":10,"reasoningOutputTokens":11,"totalTokens":100}`,
	} {
		report, valid := parseUsage(json.RawMessage(`{"threadId":"thread","turnId":"turn","tokenUsage":{"total":` + raw + `}}`))
		if !valid || report.Total != (session.Usage{InputTokens: 90, OutputTokens: 10, TotalTokens: 100}) || report.CachedInputTokens != nil {
			t.Fatal(report, valid, raw)
		}
	}
	for _, raw := range []string{`{}`, `null`, `{"threadId":"thread","turnId":"turn","tokenUsage":{"total":{"inputTokens":1,"outputTokens":2}}}`, `{"threadId":"thread","turnId":"turn","tokenUsage":{"total":{"inputTokens":-1,"outputTokens":2,"totalTokens":1}}}`, `{"threadId":"thread","turnId":"turn","tokenUsage":{"total":{"inputTokens":null,"outputTokens":2,"totalTokens":2}}}`, `{"threadId":"thread","turnId":"turn","tokenUsage":{"total":{"inputTokens":1,"outputTokens":2,"totalTokens":9223372036854775808}}}`} {
		if _, ok := parseUsage(json.RawMessage(raw)); ok {
			t.Fatal("invalid usage accepted", raw)
		}
	}
}
func TestUsageNotificationsSurviveUntilCommit(t *testing.T) {
	for _, historyOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "stream", true: "history_only"}[historyOnly], func(t *testing.T) {
			thread, path, _ := historyFixture(t)
			thread.ID = "thread"
			d, _, _ := fixtureDriver(t, &thread)
			d.historyOnly = historyOnly
			first, err := d.Snapshot(t.Context(), thread.ID, &path, 0)
			if err != nil {
				t.Fatal(err)
			}
			d.Committed()
			if len(first.Usage) != 0 {
				t.Fatal("usage was read from the journal")
			}
			for _, turn := range []string{"first", "second"} {
				raw, _ := json.Marshal(map[string]any{"method": "thread/tokenUsage/updated", "params": map[string]any{"threadId": thread.ID, "turnId": turn, "tokenUsage": map[string]any{"total": map[string]int{"inputTokens": 90, "cachedInputTokens": 80, "outputTokens": 10, "reasoningOutputTokens": 7, "totalTokens": 100}}}})
				d.rpc.receive(t.Context(), raw)
			}
			if !d.HasUpdates() {
				t.Fatal("usage did not wake driver")
			}
			next, err := d.Snapshot(t.Context(), thread.ID, &path, first.Offset)
			if err != nil || len(next.Usage) != 2 || next.Usage[0].TurnID != "first" || next.Usage[1].TurnID != "second" || next.Usage[0].CachedInputTokens == nil || *next.Usage[0].CachedInputTokens != 80 {
				t.Fatal(next.Usage, err)
			}
			retry, err := d.Snapshot(t.Context(), thread.ID, &path, first.Offset)
			if err != nil || !reflect.DeepEqual(next.Usage, retry.Usage) {
				t.Fatal(retry.Usage, err)
			}
			d.Committed()
			last, err := d.Snapshot(t.Context(), thread.ID, &path, first.Offset)
			if err != nil || len(last.Usage) != 0 {
				t.Fatal(last.Usage, err)
			}
		})
	}
}
