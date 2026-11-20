package opencode

import (
	"cmp"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/harness/codex"
	"github.com/orpheus-agents/orpheus/internal/session"
)

type nativeError struct {
	Type, Message string
	Status        int
}
type part struct {
	Type, Text, ID, Name string
	State                struct {
		Status  string
		Input   json.RawMessage
		Content []json.RawMessage
		Error   *nativeError
	}
}
type message struct {
	ID, Type, Text, Finish, Outcome, Status string
	Metadata                                struct {
		Run       string `json:"orpheus_run_id"`
		Operation string `json:"orpheus_operation_id"`
	}
	Content []part
	Error   *nativeError
}
type projection struct {
	Run      string
	Fenced   bool
	Messages []message
	Tokens   struct {
		Input, Output, Reasoning int64
		Cache                    struct{ Read, Write int64 }
	}
}

func (d *Driver) safeJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var value any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if dec.Decode(&value) != nil {
		return nil
	}
	var clean func(any) any
	clean = func(v any) any {
		switch x := v.(type) {
		case string:
			return d.redactor.RedactValues(x)
		case []any:
			for i, item := range x {
				x[i] = clean(item)
			}
		case map[string]any:
			for k, item := range x {
				x[k] = clean(item)
			}
		}
		return v
	}
	out, _ := json.Marshal(clean(value))
	return out
}
func (d *Driver) normalize(p projection) (harness.Snapshot, error) {
	var snapshot harness.Snapshot
	if p.Fenced {
		snapshot.FencedRun = d.run
	}
	if p.Run == "" {
		return snapshot, nil
	}
	t := harness.Turn{NativeID: p.Run, Status: session.Running}
	itemIndex := 0
	for _, m := range p.Messages {
		switch m.Type {
		case "user":
			id, ok := strings.CutPrefix(m.ID, "msg_")
			if !ok || uuid.Validate(id) != nil || uuid.Validate(m.Metadata.Operation) != nil || m.Metadata.Run != p.Run {
				return snapshot, harness.Failure("context_lost", "Invalid identity in OpenCode transcript.")
			}
			t.Items = append(t.Items, harness.Item{NativeID: m.ID, Type: "user", InputID: id, OperationID: m.Metadata.Operation, Text: m.Text, Index: itemIndex})
			itemIndex++
		case "assistant":
			for i, c := range m.Content {
				item := harness.Item{NativeID: fmt.Sprintf("%s/%d", m.ID, i), Index: itemIndex}
				itemIndex++
				switch c.Type {
				case "text":
					item.Type = "assistant"
					item.Kind = new("progress")
					if m.Finish == "stop" {
						item.Kind = new("answer")
					}
					item.Text = d.redactor.RedactValues(c.Text)
				case "tool":
					item.NativeID = m.ID + "/" + c.ID
					item.Type = "tool"
					item.Name = c.Name
					item.Status = cmp.Or(map[string]string{"running": "running", "streaming": "running", "completed": "completed", "error": "failed"}[c.State.Status], "unknown")
					item.Completeness = "unknown"
					item.Input = d.safeJSON(c.State.Input)
					var raw json.RawMessage
					switch {
					case len(c.State.Content) == 1:
						raw = c.State.Content[0]
					case len(c.State.Content) > 1:
						raw, _ = json.Marshal(c.State.Content)
					case c.State.Error != nil:
						raw, _ = json.Marshal(c.State.Error.Message)
					}
					if len(raw) > 0 {
						o := codex.Bounded(d.safeJSON(raw), d.limit, nil, false)
						item.Result = o.Result
						item.TruncationReason = o.Reason
						item.Completeness = o.Completeness
					}
				default:
					continue
				}
				t.Items = append(t.Items, item)
			}
		case "idle":
			t.Status = map[string]session.Status{"succeeded": session.Completed, "interrupted": session.Cancelled, "failed": session.Failed}[m.Outcome]
			if t.Status == "" {
				return snapshot, harness.Failure("context_lost", "Unknown OpenCode completion outcome.")
			}
			if t.Status == session.Failed {
				t.ErrorCode = "harness_failed"
				t.ErrorMessage = fmt.Sprintf("OpenCode failed (template %s, provider %s, model %s).", d.template, d.provider, d.model)
				if e := m.Error; e != nil {
					if e.Status == 401 || strings.Contains(strings.ToLower(e.Type), "auth") || strings.Contains(strings.ToLower(e.Message), "invalid_grant") {
						t.ErrorCode = "authentication_failed"
					}
					if msg, ok := d.redactor.Clean(e.Message); ok && msg != "" {
						t.ErrorMessage += " " + msg
					}
				}
			}
		}
	}
	snapshot.Turns = []harness.Turn{t}
	tok := p.Tokens
	input, output := tok.Input+tok.Cache.Read+tok.Cache.Write, tok.Output+tok.Reasoning
	snapshot.Usage = []harness.UsageReport{{ContextID: d.session, TurnID: p.Run, Total: session.Usage{InputTokens: input, OutputTokens: output, TotalTokens: input + output}, CachedInputTokens: &tok.Cache.Read, ReasoningOutputTokens: &tok.Reasoning}}
	return snapshot, nil
}
