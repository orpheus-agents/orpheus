package codex

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/skillum-ai/orpheus/internal/harness"
	"github.com/skillum-ai/orpheus/internal/session"
)

type NativeItem struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
	Text             string          `json:"text"`
	Phase            string          `json:"phase"`
	Tool             string          `json:"tool"`
	Status           string          `json:"status"`
	AggregatedOutput *string         `json:"aggregatedOutput"`
	Result           json.RawMessage `json:"result"`
	Arguments        json.RawMessage `json:"arguments"`
	Command          json.RawMessage `json:"command"`
	Changes          json.RawMessage `json:"changes"`
	ExitCode         *int            `json:"exitCode"`
}
type NativeTurn struct {
	ID     string          `json:"id"`
	Status string          `json:"status"`
	Items  []NativeItem    `json:"items"`
	Error  json.RawMessage `json:"error"`
}
type Thread struct {
	ID    string       `json:"id"`
	Path  *string      `json:"path"`
	Turns []NativeTurn `json:"turns"`
}
type Output struct {
	Result       json.RawMessage `json:"result"`
	Completeness string          `json:"completeness"`
	Reason       *string         `json:"reason"`
}
type Result struct {
	Type          string          `json:"type"`
	Text          *string         `json:"text,omitzero"`
	Value         json.RawMessage `json:"value,omitzero"`
	Head          *string         `json:"head,omitzero"`
	Tail          *string         `json:"tail,omitzero"`
	SourceType    string          `json:"source_type,omitzero"`
	ExitCode      *int            `json:"exit_code"`
	OriginalBytes *int            `json:"original_bytes"`
}

func Bounded(value json.RawMessage, limit int, exit *int, incomplete bool) Output {
	var text *string
	isText := json.Unmarshal(value, &text) == nil && text != nil
	var raw []byte
	if isText {
		raw = []byte(*text)
	}
	sourceType := "text"
	if !isText {
		var compact bytes.Buffer
		var decoded any
		decoder := json.NewDecoder(bytes.NewReader(value))
		decoder.UseNumber()
		if decoder.Decode(&decoded) == nil {
			encoder := json.NewEncoder(&compact)
			encoder.SetEscapeHTML(false)
			_ = encoder.Encode(decoded)
		}
		raw = bytes.TrimSuffix(compact.Bytes(), []byte{'\n'})
		sourceType = "json"
	}
	size := new(len(raw))
	if incomplete {
		size = nil
	}
	r := Result{OriginalBytes: size, ExitCode: exit}
	out := Output{Completeness: "complete"}
	if len(raw) > limit {
		head, tail := raw[:limit/2], raw[len(raw)-(limit-limit/2):]
		for !utf8.Valid(head) {
			head = head[:len(head)-1]
		}
		for !utf8.Valid(tail) {
			tail = tail[1:]
		}
		r.Type = "truncated_text"
		r.Head = new(string(head))
		r.Tail = new(string(tail))
		r.SourceType = sourceType
		out.Completeness = "truncated"
		out.Reason = new("orpheus_limit")
	} else {
		if isText {
			r.Type = "text"
			r.Text = text
		} else {
			r.Type = "json"
			r.Value = value
		}
		if incomplete {
			out.Completeness = "truncated"
			out.Reason = new("harness_limit")
		}
	}
	if r.Type == "json" {
		out.Result, _ = json.Marshal(struct {
			Type          string          `json:"type"`
			Value         json.RawMessage `json:"value"`
			OriginalBytes *int            `json:"original_bytes"`
		}{r.Type, r.Value, r.OriginalBytes})
	} else {
		out.Result, _ = json.Marshal(r)
	}
	return out
}

var toolKinds = []string{"commandExecution", "fileChange", "mcpToolCall", "dynamicToolCall", "collabAgentToolCall", "webSearch", "imageView", "imageGeneration"}
var internalKinds = []string{"reasoning", "plan", "contextCompaction", "enteredReviewMode", "exitedReviewMode"}

func Normalize(thread Thread, completed map[string]bool, outputs map[string]Output, limit int, orders map[string]int64, commands map[string]json.RawMessage) harness.Snapshot {
	snapshot := harness.Snapshot{Turns: []harness.Turn{}}
	for _, native := range thread.Turns {
		status := map[string]session.Status{"inProgress": session.Running, "completed": session.Completed, "failed": session.Failed, "interrupted": session.Cancelled}[native.Status]
		if status == "" {
			status = "unknown"
		}
		turn := harness.Turn{NativeID: native.ID, Status: status, Items: []harness.Item{}}
		if len(native.Error) > 0 && string(native.Error) != "null" {
			turn.ErrorCode = "harness_failed"
			detail := strings.ToLower(string(native.Error))
			for _, v := range []string{"unauthorized", "authentication", "401", "token_expired", "refresh_token"} {
				if strings.Contains(detail, v) {
					turn.ErrorCode = "authentication_failed"
				}
			}
		}
		items := make([]NativeItem, 0, len(native.Items))
		for _, item := range native.Items {
			if item.Type == "userMessage" || item.Type == "agentMessage" || slices.Contains(toolKinds, item.Type) || slices.Contains(internalKinds, item.Type) {
				items = append(items, item)
			}
		}
		if len(orders) > 0 {
			slices.SortStableFunc(items, func(a, b NativeItem) int {
				at, aok := orders[a.ID]
				bt, bok := orders[b.ID]
				if !aok && !bok {
					return 0
				}
				if !aok {
					return 1
				}
				if !bok {
					return -1
				}
				if at < bt {
					return -1
				}
				if at > bt {
					return 1
				}
				return strings.Compare(a.ID, b.ID)
			})
		}
		for index, item := range items {
			projected := harness.Item{NativeID: item.ID, Index: index, Status: "unknown", Completeness: "unknown"}
			switch item.Type {
			case "userMessage":
				projected.Type = "user"
				for _, part := range item.Content {
					projected.Text += part.Text
				}
			case "agentMessage":
				if !completed[item.ID] && !status.Terminal() {
					continue
				}
				projected.Type = "assistant"
				projected.Text = item.Text
				projected.Kind = new("answer")
				if item.Phase == "commentary" {
					projected.Kind = new("progress")
				}
			default:
				if !slices.Contains(toolKinds, item.Type) {
					continue
				}
				projected.Type = "tool"
				projected.Name = item.Tool
				if projected.Name == "" {
					projected.Name = item.Type
				}
				state := map[string]string{"inProgress": "running", "completed": "completed", "failed": "failed", "declined": "cancelled"}[item.Status]
				if state == "" {
					state = "unknown"
					if completed[item.ID] {
						state = "completed"
					}
				}
				if state == "running" && status.Terminal() {
					state = "unknown"
				}
				projected.Status = state
				output, ok := outputs[item.ID]
				switch {
				case ok:
					var result Result
					if json.Unmarshal(output.Result, &result) == nil && result.Type != "json" {
						result.ExitCode = item.ExitCode
						output.Result, _ = json.Marshal(result)
					}
				case item.AggregatedOutput != nil:
					raw, _ := json.Marshal(*item.AggregatedOutput)
					output = Bounded(raw, limit, item.ExitCode, true)
				case len(item.Result) > 0 && string(item.Result) != "null":
					output = Bounded(item.Result, limit, nil, false)
				}
				if output.Result != nil {
					projected.Result = output.Result
					projected.Completeness = output.Completeness
					projected.TruncationReason = output.Reason
				}
				input := item.Arguments
				if input == nil {
					input = item.Command
				}
				if input == nil {
					input = item.Changes
				}
				if input == nil {
					input = json.RawMessage("{}")
				}
				if item.Type == "commandExecution" {
					if argv, ok := commands[item.ID]; ok {
						input = argv
					}
					var display *string
					if json.Unmarshal(input, &display) == nil && display != nil {
						if argv, ok := splitDisplay(*display); ok {
							input, _ = json.Marshal(argv)
						}
					}
				}
				if item.Type == "fileChange" {
					var changes []json.RawMessage
					if json.Unmarshal(input, &changes) == nil {
						slices.SortStableFunc(changes, func(a, b json.RawMessage) int {
							var aa, bb struct {
								Path string `json:"path"`
							}
							_ = json.Unmarshal(a, &aa)
							_ = json.Unmarshal(b, &bb)
							return strings.Compare(aa.Path, bb.Path)
						})
						input, _ = json.Marshal(changes)
					}
				}
				projected.Input = input
			}
			turn.Items = append(turn.Items, projected)
		}
		snapshot.Turns = append(snapshot.Turns, turn)
	}
	return snapshot
}
