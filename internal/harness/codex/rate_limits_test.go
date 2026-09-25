package codex

import (
	"encoding/json"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus/internal/accountlimits"
	"github.com/orpheus-agents/orpheus/internal/harness"
)

func TestRateLimitReadAndNotificationIsolation(t *testing.T) {
	reads := 0
	rpc, box := newRPC(t, func(req map[string]json.RawMessage) any {
		var method string
		_ = json.Unmarshal(req["method"], &method)
		if method == "account/rateLimits/read" {
			reads++
			return map[string]any{"id": req["id"], "result": map[string]any{"rateLimitsByLimitId": map[string]any{"codex": map[string]any{"limitId": "codex", "primary": map[string]any{"usedPercent": 25, "windowDurationMins": 300, "resetsAt": 1790251200}}}}}
		}
		return map[string]any{"id": req["id"], "result": map[string]any{}}
	})
	d := New(box, time.Second, 1024)
	d.rpc = rpc
	rpc.receive(t.Context(), []byte(`{"method":"thread/tokenUsage/updated","params":{"turnId":"t"}}`))
	for range 500 {
		rpc.receive(t.Context(), []byte(`{"method":"account/rateLimits/updated","params":{"rateLimits":{"primary":{"usedPercent":1}}}}`))
	}
	select {
	case <-d.AccountLimitsEvents():
	default:
		t.Fatal("missing rate-limit signal")
	}
	if !d.AccountLimitsDirty() || d.AccountLimitsDirty() {
		t.Fatal("rate-limit flag did not coalesce")
	}
	notifications, dirty := rpc.drain()
	if dirty || len(notifications) != 1 || notifications[0].Method != "thread/tokenUsage/updated" {
		t.Fatal("rate-limit drain stole execution notification", notifications, dirty)
	}
	snapshot, err := d.ReadAccountLimits(t.Context())
	if err != nil || reads != 1 || len(snapshot.Buckets) != 1 || snapshot.Buckets[0].Primary.RemainingPercent != 75 {
		t.Fatal(snapshot, err)
	}
	rpc.receive(t.Context(), []byte(`{"method":"account/updated","params":{"authMode":"chatgpt","planType":"pro"}}`))
	if _, err := d.ReadAccountLimits(t.Context()); err != nil {
		t.Fatal("plan update invalidated account", err)
	}
	rpc.receive(t.Context(), []byte(`{"method":"account/updated","params":{"planType":"plus"}}`))
	if _, err := d.ReadAccountLimits(t.Context()); err != nil {
		t.Fatal("partial plan update invalidated account", err)
	}
	rpc.receive(t.Context(), []byte(`{"method":"account/updated","params":{"authMode":null,"planType":null}}`))
	if _, err := d.ReadAccountLimits(t.Context()); !errors.Is(err, accountlimits.ErrAuthenticationUnavailable) {
		t.Fatal(err)
	}
	rpc.receive(t.Context(), []byte(`{"method":"account/updated","params":{"authMode":"chatgpt","planType":"pro"}}`))
	if _, err := d.ReadAccountLimits(t.Context()); !errors.Is(err, accountlimits.ErrAuthenticationUnavailable) {
		t.Fatal("account recovered without initialization", err)
	}
	rpc.accountInitialized()
	if _, err := d.ReadAccountLimits(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestRateLimitReadErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result any
		want   error
	}{
		{"unsupported", map[string]any{"error": map[string]any{"code": -32601, "message": "Method not found"}}, accountlimits.ErrUnsupported},
		{"malformed", map[string]any{"result": map[string]any{"rateLimitsByLimitId": map[string]any{"codex": map[string]any{"primary": map[string]any{"usedPercent": "bad"}}}}}, accountlimits.ErrInvalidResponse},
		{"no data", map[string]any{"result": map[string]any{}}, accountlimits.ErrNoData},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rpc, box := newRPC(t, func(req map[string]json.RawMessage) any {
				response := map[string]any{"id": req["id"]}
				maps.Copy(response, tc.result.(map[string]any))
				return response
			})
			d := New(box, time.Second, 1024)
			d.rpc = rpc
			_, err := d.ReadAccountLimits(t.Context())
			if !errors.Is(err, tc.want) {
				t.Fatal(err)
			}
		})
	}
}

func TestRateLimitReadCloseRace(t *testing.T) {
	started := make(chan struct{}, 1)
	rpc, box := newRPC(t, func(req map[string]json.RawMessage) any {
		started <- struct{}{}
		return nil
	})
	d := New(box, time.Second, 1024)
	d.rpc = rpc
	done := make(chan error, 1)
	go func() { _, err := d.ReadAccountLimits(t.Context()); done <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("read did not enter RPC")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, harness.ErrUncertain) {
			t.Fatal("disconnect was not temporary", err)
		}
	case <-time.After(time.Second):
		t.Fatal("read did not stop on close")
	}
	if d.AccountLimitsDirty() {
		t.Fatal("disconnect queued a read on the dead RPC")
	}
	if _, err := d.ReadAccountLimits(t.Context()); !errors.Is(err, harness.ErrUncertain) {
		t.Fatal("disconnected account was classified as authentication failure", err)
	}
}
