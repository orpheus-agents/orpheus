package diagnostic

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Fields contains only explicitly selected, bounded diagnostic values.
// NativeMessage and StderrTail must have passed Redactor before being assigned.
type Fields struct {
	ID                     uuid.UUID  `json:"diagnostic_id"`
	SessionID              uuid.UUID  `json:"session_id,omitzero"`
	RunID                  *uuid.UUID `json:"run_id,omitempty"`
	Namespace              *string    `json:"namespace,omitempty"`
	Phase                  string     `json:"phase,omitempty"`
	PublicCode             string     `json:"public_code,omitempty"`
	RPCMethod              string     `json:"rpc_method,omitempty"`
	RPCID                  string     `json:"rpc_id,omitempty"`
	RPCCode                *int       `json:"rpc_code,omitempty"`
	DurationMS             *int64     `json:"duration_ms,omitempty"`
	Kind                   string     `json:"error_kind"`
	NativeMessage          *string    `json:"native_message,omitempty"`
	StderrTail             *string    `json:"stderr_tail,omitempty"`
	TextOmitted            []string   `json:"text_omitted,omitempty"`
	NativeMessageTruncated bool       `json:"native_message_truncated,omitempty"`
	StderrTruncated        bool       `json:"stderr_truncated,omitempty"`
	OperationID            *uuid.UUID `json:"operation_id,omitempty"`
	SandboxID              *string    `json:"sandbox_id,omitempty"`
	LaunchID               *uuid.UUID `json:"launch_id,omitempty"`
	ProcessID              *int       `json:"process_id,omitempty"`
}

// Error preserves the controlling cause independently of the selected diagnostic
// fields. In particular a persistence failure must not unwrap an RPC rejection.
type Error struct {
	Cause  error
	Fields Fields
}

func (e *Error) Error() string { return "execution diagnostic" }
func (e *Error) Unwrap() error { return e.Cause }

func Capture(err error) Fields {
	if e, ok := errors.AsType[*Error](err); ok {
		out := e.Fields
		out.TextOmitted = slices.Clone(out.TextOmitted)
		return out
	}
	return Fields{ID: uuid.New(), Kind: "unknown"}
}

func With(err error, fields Fields) error {
	if err == nil {
		return nil
	}
	if fields.ID == uuid.Nil {
		fields.ID = uuid.New()
	}
	return &Error{Cause: err, Fields: fields}
}

func Truncate(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	for limit > 0 && !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit], true
}

// Log emits one flat structured event. The fixed envelope fits in the remaining
// 512 bytes after the fields budget, including JSON escaping and the timestamp.
func Log(ctx context.Context, level slog.Level, fields Fields) {
	fields.RPCMethod, _ = Truncate(fields.RPCMethod, 256)
	fields.RPCID, _ = Truncate(fields.RPCID, 256)
	fields.PublicCode, _ = Truncate(fields.PublicCode, 256)
	fields.Phase, _ = Truncate(fields.Phase, 64)
	for _, ptr := range []**string{&fields.Namespace, &fields.SandboxID} {
		if *ptr != nil {
			value, _ := Truncate(**ptr, 256)
			*ptr = &value
		}
	}
	const budget = (64 << 10) - 512
	encoded, _ := json.Marshal(fields)
	if len(encoded) > budget {
		fields.StderrTail = nil
		fields.TextOmitted = append(fields.TextOmitted, "event_limit")
		encoded, _ = json.Marshal(fields)
	}
	if len(encoded) > budget {
		fields.NativeMessage = nil
		encoded, _ = json.Marshal(fields)
	}
	// The round trip gives slog the same flat keys as the documented JSON shape.
	// It contains no raw error or unselected protocol data.
	var values map[string]json.RawMessage
	_ = json.Unmarshal(encoded, &values)
	attrs := make([]slog.Attr, 0, len(values)+1)
	attrs = append(attrs, slog.String("event", "execution.failure"))
	for key, value := range values {
		attrs = append(attrs, slog.Any(key, value))
	}
	slog.LogAttrs(ctx, level, "Execution failed", attrs...)
}
