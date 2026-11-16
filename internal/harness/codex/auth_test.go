package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus/internal/diagnostic"
	"github.com/orpheus-agents/orpheus/internal/session"
)

func TestReconnectRemembersOldKeyBeforeRPC(t *testing.T) {
	for _, raw := range []string{`{"OPENAI_API_KEY":"prior-process-key"}`, `{"tokens":{"access_token":"prior-access","refresh_token":"prior-refresh","id_token":"prior-id"}}`} {
		for _, login := range []bool{false, true} {
			t.Run(fmt.Sprintf("login=%t/%s", login, raw), func(t *testing.T) {
				redactor := new(diagnostic.Redactor)
				calls := 0
				rpc, box := newRPC(t, func(req map[string]json.RawMessage) any {
					calls++
					clean, ok := redactor.Clean("prior-process-key prior-access prior-refresh prior-id")
					secret := "prior-process-key"
					if strings.Contains(raw, "tokens") {
						secret = "prior-access"
					}
					if !ok || strings.Contains(clean, secret) {
						t.Error("RPC began before local credentials were registered")
					}
					if string(req["method"]) == `"initialized"` {
						return nil
					}
					return map[string]any{"id": req["id"], "result": map[string]any{}}
				})
				box.files = map[string][]byte{"/state/auth.json": []byte(raw)}
				box.run = func(context.Context, string) ([]byte, error) { return []byte("/state"), nil }
				rpc.redactor = redactor
				d := New(box, time.Second, 1024, redactor)
				d.rpc = rpc
				if err := d.Initialize(t.Context(), session.Credentials{Mode: "api_key", APIKey: "replacement-key"}, login); err != nil {
					t.Fatal(err)
				}
				if calls == 0 {
					t.Fatal("initialization skipped")
				}
			})
		}
	}
}

func TestReconnectRejectsMissingOrInvalidLocalAuthBeforeRPC(t *testing.T) {
	for _, raw := range []string{"", `{}`, `{"OPENAI_API_KEY":123}`, strings.Repeat("x", (1<<20)+1)} {
		rpc, box := newRPC(t, func(map[string]json.RawMessage) any { t.Error("RPC called without local auth"); return nil })
		box.run = func(context.Context, string) ([]byte, error) { return []byte("/state"), nil }
		if raw != "" {
			box.files = map[string][]byte{"/state/auth.json": []byte(raw)}
		}
		d := New(box, time.Second, 1024, new(diagnostic.Redactor))
		d.rpc = rpc
		if err := d.Initialize(t.Context(), session.Credentials{Mode: "api_key", APIKey: "replacement"}, false); err == nil {
			t.Fatal("invalid local auth accepted")
		}
	}
}
