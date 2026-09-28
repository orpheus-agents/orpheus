package codex

import (
	"encoding/json"

	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/session"
)

func parseUsage(raw json.RawMessage) (harness.UsageReport, bool) {
	var wire struct {
		ThreadID   string `json:"threadId"`
		TurnID     string `json:"turnId"`
		TokenUsage struct {
			Total struct {
				Input     *int64          `json:"inputTokens"`
				Cached    json.RawMessage `json:"cachedInputTokens"`
				Output    *int64          `json:"outputTokens"`
				Reasoning json.RawMessage `json:"reasoningOutputTokens"`
				Total     *int64          `json:"totalTokens"`
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
	return harness.UsageReport{
		ContextID:             wire.ThreadID,
		TurnID:                wire.TurnID,
		Total:                 session.Usage{InputTokens: *v.Input, OutputTokens: *v.Output, TotalTokens: *v.Total},
		CachedInputTokens:     parseUsageBreakdown(v.Cached, *v.Input),
		ReasoningOutputTokens: parseUsageBreakdown(v.Reasoning, *v.Output),
	}, true
}

func parseUsageBreakdown(raw json.RawMessage, parent int64) *int64 {
	var value *int64
	if json.Unmarshal(raw, &value) != nil || value == nil || *value < 0 || *value > parent {
		return nil
	}
	return value
}
