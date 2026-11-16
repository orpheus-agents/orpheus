package codex

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/diagnostic"
	"github.com/orpheus-agents/orpheus/internal/harness"
)

func TestRPCDiagnosticsKeepEachConcurrentCall(t *testing.T) {
	rpc, box := newRPC(t, func(req map[string]json.RawMessage) any {
		var method string
		_ = json.Unmarshal(req["method"], &method)
		return map[string]any{"id": req["id"], "error": map[string]any{"code": -32000, "message": method + " failed: rotated-secret", "data": "private-payload"}}
	})
	rpc.redactor.Add("rotated-secret")
	// Unbuffered stderr must be consumed without any stdout or active call.
	for _, part := range []string{"backend: rotated-", "secret\n"} {
		select {
		case box.stream.errout <- []byte(part):
		case <-time.After(time.Second):
			t.Fatal("stderr stalled")
		}
	}
	var wg sync.WaitGroup
	results := make(chan diagnostic.Fields, 2)
	for _, method := range []string{"thread/read", "account/rateLimits/read"} {
		wg.Go(func() {
			err := rpc.Call(t.Context(), method, nil, nil)
			if !errors.Is(err, harness.ErrRejected) || strings.Contains(err.Error(), "rotated-secret") {
				t.Errorf("unsafe or reclassified rejection: %v", err)
			}
			fields := diagnostic.Capture(err)
			if fields.RPCMethod != method || fields.RPCCode == nil || *fields.RPCCode != -32000 || fields.Kind != "rpc_rejected" || fields.DurationMS == nil {
				t.Errorf("lost RPC context: %+v", fields)
			}
			if fields.NativeMessage == nil || *fields.NativeMessage != method+" failed: [REDACTED]" || fields.StderrTail == nil || *fields.StderrTail != "backend: [REDACTED]\n" {
				t.Errorf("missing or unsafe text: %+v", fields)
			}
			if fields.RunID != nil || fields.SessionID != uuid.Nil {
				t.Error("RPC invented a run binding")
			}
			results <- fields
		})
	}
	wg.Wait()
	a, b := <-results, <-results
	if a.ID == b.ID || a.RPCID == b.RPCID {
		t.Fatal("independent calls share identity")
	}
}

func TestRPCTransportDiagnosticKinds(t *testing.T) {
	for _, kind := range []string{"timeout", "cancelled", "transport_error", "stream_closed", "invalid_response"} {
		t.Run(kind, func(t *testing.T) {
			rpc, box := newRPC(t, func(req map[string]json.RawMessage) any {
				if kind == "invalid_response" {
					return map[string]any{"id": req["id"], "result": "not-an-object"}
				}
				return nil
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch kind {
			case "cancelled":
				cancel()
			case "transport_error":
				box.stream.writeErr = errors.New("private transport payload")
			case "stream_closed":
				close(box.stream.out)
				close(box.stream.errout)
				<-rpc.done
			}
			var dest struct{ Text string }
			err := rpc.Call(ctx, "thread/read", nil, &dest)
			fields := diagnostic.Capture(err)
			if !errors.Is(err, harness.ErrUncertain) || fields.Kind != kind || fields.NativeMessage != nil || fields.RPCCode != nil {
				t.Fatalf("wrong transport diagnostic: %+v %v", fields, err)
			}
		})
	}
}

func TestRPCWriteContextErrorDiagnosticKinds(t *testing.T) {
	for _, tc := range []struct {
		kind  string
		cause error
	}{
		{"timeout", context.DeadlineExceeded},
		{"cancelled", context.Canceled},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			rpc, box := newRPC(t, func(map[string]json.RawMessage) any { return nil })
			box.stream.writeErr = errors.Join(errors.New("write failed"), tc.cause)
			err := rpc.Call(t.Context(), "thread/read", nil, nil)
			fields := diagnostic.Capture(err)
			if !errors.Is(err, harness.ErrUncertain) || !errors.Is(err, tc.cause) || fields.Kind != tc.kind {
				t.Fatalf("wrong write diagnostic: %+v %v", fields, err)
			}
		})
	}
}

func TestRPCResponseBeforeEOFRemainsAuthoritative(t *testing.T) {
	for range 20 {
		rpc, box := newRPC(t, func(req map[string]json.RawMessage) any {
			return map[string]any{"id": req["id"], "error": map[string]any{"code": -32000, "message": "rejected"}}
		})
		write := box.stream.write
		box.stream.write = func(raw []byte) {
			write(raw)
			close(box.stream.out)
			close(box.stream.errout)
			<-rpc.done
		}
		if err := rpc.Call(t.Context(), "account/read", nil, nil); !errors.Is(err, harness.ErrRejected) {
			t.Fatal("EOF hid a known response", err)
		}
	}
}
