//go:build integration

package store

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/accountlimits"
	"github.com/orpheus-agents/orpheus/internal/llm"
)

func TestOpenCodeAccountsAndSourceChanges(t *testing.T) {
	s := fixture(t)
	ctx := t.Context()
	account := func(name, provider string) llm.AccountDetails {
		t.Helper()
		a, err := s.CreateAccount(ctx, llm.AccountInput{Name: name, Harness: "opencode", AuthMode: "api_key", Provider: provider, APIKey: new("first-private-key")})
		if err != nil || a.Provider != provider || a.Harness != "opencode" {
			t.Fatal(a, err)
		}
		return a
	}
	kimi := account("global plan", "kimi-code-plan-global")
	custom := account("custom", "test-provider")
	if _, err := s.CreateProfile(ctx, llm.ProfileInput{Name: "invalid-options", AccountID: kimi.ID, Model: "k3.1-pro", Codex: map[string]string{"effort": "high"}}); err == nil {
		t.Fatal("OpenCode accepted Codex-only options")
	}
	p, err := s.CreateProfile(ctx, llm.ProfileInput{Name: "kimi", AccountID: kimi.ID, Model: "k3.1-pro"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateProfile(ctx, p.ID, []byte(`{"codex":{"effort":"high"}}`)); err == nil {
		t.Fatal("OpenCode accepted Codex-only patch")
	}
	run, err := s.Accept(ctx, requestForProfile(p.Name))
	if err != nil {
		t.Fatal(err)
	}
	record, err := GetSession(ctx, s.Pool, run.SessionID, false)
	if err != nil || record.Configuration.Harness != "opencode" || record.Configuration.Provider != kimi.Provider {
		t.Fatal("snapshot", err)
	}
	sources := func() KimiLimitCredential {
		t.Helper()
		keys, err := s.KimiLimitCredentials(ctx)
		if err != nil || len(keys) != 1 || keys[0].AccountID != kimi.ID.String() {
			t.Fatal("Kimi credential selection", err)
		}
		return keys[0]
	}
	current := sources()
	inspect := func(state string) {
		t.Helper()
		report, err := s.AccountLimitsFor(ctx, []uuid.UUID{kimi.ID, custom.ID})
		if err != nil || len(report.Items) != 1 || report.Items[0].AccountID != kimi.ID.String() || report.Items[0].State != state {
			t.Fatal(report, err)
		}
	}
	inspect("unknown")
	now := time.Now().UTC()
	snapshot := &accountlimits.Snapshot{Buckets: []accountlimits.Bucket{{LimitID: "limit_5h", Primary: &accountlimits.Window{UsedPercent: 25, RemainingPercent: 75}}}}
	if err := s.SaveAccountLimitObservation(ctx, current.AccountID, current.Fingerprint, now, snapshot, nil); err != nil {
		t.Fatal(err)
	}
	inspect("fresh")
	if err := s.ReplaceAccountKey(ctx, kimi.ID, "second-private-key"); err != nil {
		t.Fatal(err)
	}
	updated := sources()
	if updated.Fingerprint == current.Fingerprint || updated.Key != "second-private-key" {
		t.Fatal("key change did not invalidate observation source")
	}
	inspect("unknown")
	if err := s.SaveAccountLimitObservation(ctx, current.AccountID, current.Fingerprint, now, snapshot, nil); err == nil {
		t.Fatal("late sample from old key accepted")
	}
	if err := s.SaveAccountLimitObservation(ctx, updated.AccountID, updated.Fingerprint, now, nil, new("authentication_unavailable")); err != nil {
		t.Fatal(err)
	}
	inspect("unavailable")
}
