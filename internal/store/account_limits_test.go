//go:build integration

package store

import (
	"strings"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus/internal/accountlimits"
	"github.com/orpheus-agents/orpheus/internal/config"
)

func accountLimitFixture(t *testing.T) (*Store, config.Account) {
	t.Helper()
	s := fixture(t)
	p, err := config.ReadProfiles(strings.NewReader(`[credential_stores.s]
bucket="test-credentials"
[profiles.fast]
harness="codex"
[profiles.fast.auth]
mode="account"
account_id="team-main"
store="s"
key="auth.json"
[profiles.deep]
harness="codex"
[profiles.deep.auth]
mode="account"
account_id="team-main"
store="s"
key="auth.json"
`))
	if err != nil {
		t.Fatal(err)
	}
	s.Profiles = p
	return s, p.Accounts()[0]
}

func TestAccountLimitObservationStateAndOrdering(t *testing.T) {
	s, account := accountLimitFixture(t)
	ctx := t.Context()
	read := func() LimitItem {
		t.Helper()
		response, err := s.AccountLimits(ctx)
		if err != nil || len(response.Items) != 1 || response.StaleAfterSeconds != 300 {
			t.Fatal(response, err)
		}
		return response.Items[0]
	}
	if got := read(); got.State != "unknown" || got.ObservedAt != nil || len(got.Buckets) != 0 || strings.Join(got.Profiles, ",") != "deep,fast" {
		t.Fatal(got)
	}
	old := time.Now().UTC().Add(-2 * time.Minute)
	code := "no_data"
	if err := s.SaveAccountLimitObservation(ctx, account.ID, account.Fingerprint, old, nil, &code); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.State != "unavailable" || got.ErrorCode == nil || *got.ErrorCode != "no_data" || got.ObservedAt != nil {
		t.Fatal(got)
	}
	reset := time.Now().UTC().Add(time.Hour)
	sample := accountlimits.Snapshot{Buckets: []accountlimits.Bucket{{LimitID: "codex", Primary: &accountlimits.Window{UsedPercent: 25, RemainingPercent: 75, ResetsAt: &reset}}}}
	at := old.Add(time.Minute).Truncate(time.Microsecond)
	if err := s.SaveAccountLimitObservation(ctx, account.ID, account.Fingerprint, at, &sample, nil); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.State != "fresh" || got.ErrorCode != nil || got.ObservedAt == nil || !got.ObservedAt.Equal(at) || len(got.Buckets) != 1 {
		t.Fatal(got)
	}
	if err := s.SaveAccountLimitObservation(ctx, account.ID, account.Fingerprint, old, nil, &code); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.State != "fresh" || got.ErrorCode != nil {
		t.Fatal("old retry replaced sample", got)
	}
	failedAt := at.Add(time.Second)
	code = "temporarily_unavailable"
	if err := s.SaveAccountLimitObservation(ctx, account.ID, account.Fingerprint, failedAt, nil, &code); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.State != "stale" || got.ObservedAt == nil || !got.ObservedAt.Equal(at) || got.ErrorCode == nil || *got.ErrorCode != code || len(got.Buckets) != 1 {
		t.Fatal(got)
	}
	newSample := accountlimits.Snapshot{Buckets: []accountlimits.Bucket{}}
	if err := s.SaveAccountLimitObservation(ctx, account.ID, account.Fingerprint, failedAt.Add(time.Second), &newSample, nil); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.State != "fresh" || len(got.Buckets) != 0 || got.ErrorCode != nil {
		t.Fatal(got)
	}
}

func TestAccountLimitStaleAgeResetAndSource(t *testing.T) {
	s, account := accountLimitFixture(t)
	ctx := t.Context()
	aged := accountlimits.Snapshot{Buckets: []accountlimits.Bucket{{LimitID: "codex", Primary: &accountlimits.Window{UsedPercent: 10, RemainingPercent: 90}}}}
	if err := s.SaveAccountLimitObservation(ctx, account.ID, account.Fingerprint, time.Now().UTC().Add(-6*time.Minute), &aged, nil); err != nil {
		t.Fatal(err)
	}
	response, err := s.AccountLimits(ctx)
	if err != nil || response.Items[0].State != "stale" {
		t.Fatal(response, err)
	}
	reset := time.Now().UTC().Add(-time.Second)
	sample := accountlimits.Snapshot{Buckets: []accountlimits.Bucket{{LimitID: "codex", Primary: &accountlimits.Window{UsedPercent: 30, RemainingPercent: 70, ResetsAt: &reset}}}}
	if err := s.SaveAccountLimitObservation(ctx, account.ID, account.Fingerprint, time.Now().UTC(), &sample, nil); err != nil {
		t.Fatal(err)
	}
	response, err = s.AccountLimits(ctx)
	if err != nil || response.Items[0].State != "stale" {
		t.Fatal(response, err)
	}
	profile := s.Profiles.Profiles["fast"]
	profile.Auth.Key = "new.json"
	s.Profiles.Profiles["fast"] = profile
	profile = s.Profiles.Profiles["deep"]
	profile.Auth.Key = "new.json"
	s.Profiles.Profiles["deep"] = profile
	response, err = s.AccountLimits(ctx)
	if err != nil || response.Items[0].State != "unknown" {
		t.Fatal(response, err)
	}
	if err := s.SaveAccountLimitObservation(ctx, account.ID, account.Fingerprint, time.Now().UTC(), &sample, nil); err == nil {
		t.Fatal("old source was allowed to publish")
	}
}
