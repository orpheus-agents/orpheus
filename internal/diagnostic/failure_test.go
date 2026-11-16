package diagnostic

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestDiagnosticPreservesControllingCauseAndIdentity(t *testing.T) {
	original := errors.New("private transport error")
	err := With(original, Fields{Kind: "transport_error", RPCMethod: "account/read"})
	fields := Capture(err)
	persistence := errors.New("persistence")
	replaced := With(persistence, fields)
	if !errors.Is(err, original) || !errors.Is(replaced, persistence) || errors.Is(replaced, original) || strings.Contains(err.Error(), "private") || Capture(replaced).ID != fields.ID || fields.ID == uuid.Nil {
		t.Fatal("cause classification or diagnostic identity changed")
	}
}

func TestDiagnosticLogIsFlatAndBoundedAfterEscaping(t *testing.T) {
	var output bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	id := uuid.New()
	Log(t.Context(), slog.LevelError, Fields{ID: id, Kind: "rpc_rejected", RPCMethod: "account/read", NativeMessage: new("safe"), StderrTail: new(strings.Repeat("\x01", MaxStderrBytes))})
	var got map[string]any
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["event"] != "execution.failure" || got["diagnostic_id"] != id.String() || got["rpc_method"] != "account/read" || got["native_message"] != "safe" || len(output.Bytes()) > 64<<10 || got["stderr_tail"] != nil {
		t.Fatalf("invalid event: %s", output.Bytes())
	}
	if got["text_omitted"] == nil {
		t.Fatal("missing budget indication")
	}
}

func TestDiagnosticLogMasksMixedCasePercentEncodedSecrets(t *testing.T) {
	for _, tc := range []struct {
		secret string
		forms  []string
	}{
		{"private/+token", []string{"private/+token", "private%2F%2Btoken", "private%2f%2btoken", "private%2F%2btoken", "private%2f%2Btoken", "private%2f+token"}},
		{"private%2fb/+token", []string{"private%2fb/+token", "private%252fb%2f%2Btoken", "private%252fb%2F%2btoken", "private%252fb%2f+token"}},
	} {
		t.Run(tc.secret, func(t *testing.T) {
			var output bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
			t.Cleanup(func() { slog.SetDefault(old) })
			r := new(Redactor)
			r.Add(tc.secret)
			input := strings.Join(tc.forms, ", ")
			message, allowed := r.Clean(input)
			if !allowed {
				t.Fatal("bounded input was omitted")
			}
			tail := NewStderr(r, false)
			tail.Append([]byte(input + "\n"))
			stderr, _, _ := tail.Snapshot()
			Log(t.Context(), slog.LevelError, Fields{ID: uuid.New(), NativeMessage: &message, StderrTail: stderr})
			var event map[string]any
			if err := json.Unmarshal(output.Bytes(), &event); err != nil {
				t.Fatal(err)
			}
			want := strings.TrimSuffix(strings.Repeat(redacted+", ", len(tc.forms)), ", ")
			if event["native_message"] != want || event["stderr_tail"] != want+"\n" || strings.Contains(output.String(), "private") {
				t.Fatalf("encoded secret leaked into JSON log: %s", &output)
			}
		})
	}
}
