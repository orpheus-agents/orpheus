package codex

import (
	"encoding/json"

	"github.com/skillum-ai/orpheus/internal/harness"
	"github.com/skillum-ai/orpheus/internal/session"
)

func parseUsage(raw json.RawMessage) (harness.UsageReport, bool) {
	var wire struct {
		ThreadID   string `json:"threadId"`
		TurnID     string `json:"turnId"`
		TokenUsage struct {
			Total struct {
				Input  *int64 `json:"inputTokens"`
				Output *int64 `json:"outputTokens"`
				Total  *int64 `json:"totalTokens"`
			} `json:"total"`
		} `json:"tokenUsage"`
	}
	if json.Unmarshal(raw, &wire) != nil {
		return harness.UsageReport{}, false
	}
	v := wire.TokenUsage.Total
	if wire.ThreadID == "" || wire.TurnID == "" || v.Input == nil || v.Output == nil || v.Total == nil || *v.Input < 0 || *v.Output < 0 || *v.Total < 0 {
		return harness.UsageReport{}, false
	}
	return harness.UsageReport{ContextID: wire.ThreadID, TurnID: wire.TurnID, Total: session.Usage{InputTokens: *v.Input, OutputTokens: *v.Output, TotalTokens: *v.Total}}, true
}
