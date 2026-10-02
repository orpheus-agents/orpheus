//go:build integration

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus/internal/accountlimits"
	"github.com/orpheus-agents/orpheus/internal/config"
)

func accountLimitFixture(t *testing.T) (*Store, config.Account) {
	t.Helper()
	s := fixture(t)
	p, err := config.ReadProfiles(strings.NewReader(`[templates.codex]
[credential_stores.s]
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
	if got := read(); got.State != "unknown" || got.ObservedAt != nil || got.ResetCreditsAvailable != nil || len(got.Buckets) != 0 || strings.Join(got.Profiles, ",") != "deep,fast" {
		t.Fatal(got)
	}
	old := time.Now().UTC().Add(-2 * time.Minute)
	code := "no_data"
	if err := s.SaveAccountLimitObservation(ctx, account.ID, account.Fingerprint, old, nil, &code); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.State != "unavailable" || got.ErrorCode == nil || *got.ErrorCode != "no_data" || got.ObservedAt != nil || got.ResetCreditsAvailable != nil {
		t.Fatal(got)
	}
	reset := time.Now().UTC().Add(time.Hour)
	sample := accountlimits.Snapshot{ResetCreditsAvailable: new(int64(2)), Buckets: []accountlimits.Bucket{{LimitID: "codex", Primary: &accountlimits.Window{UsedPercent: 25, RemainingPercent: 75, ResetsAt: &reset}}}}
	at := old.Add(time.Minute).Truncate(time.Microsecond)
	if err := s.SaveAccountLimitObservation(ctx, account.ID, account.Fingerprint, at, &sample, nil); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.State != "fresh" || got.ErrorCode != nil || got.ObservedAt == nil || !got.ObservedAt.Equal(at) || len(got.Buckets) != 1 || got.ResetCreditsAvailable == nil || *got.ResetCreditsAvailable != 2 {
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
	if got := read(); got.State != "stale" || got.ObservedAt == nil || !got.ObservedAt.Equal(at) || got.ErrorCode == nil || *got.ErrorCode != code || len(got.Buckets) != 1 || got.ResetCreditsAvailable == nil || *got.ResetCreditsAvailable != 2 {
		t.Fatal(got)
	}
	newSample := accountlimits.Snapshot{Buckets: []accountlimits.Bucket{}}
	if err := s.SaveAccountLimitObservation(ctx, account.ID, account.Fingerprint, failedAt.Add(time.Second), &newSample, nil); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.State != "fresh" || len(got.Buckets) != 0 || got.ErrorCode != nil || got.ResetCreditsAvailable != nil {
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
	sample := accountlimits.Snapshot{ResetCreditsAvailable: new(int64(2)), Buckets: []accountlimits.Bucket{{LimitID: "codex", Primary: &accountlimits.Window{UsedPercent: 30, RemainingPercent: 70, ResetsAt: &reset}}}}
	if err := s.SaveAccountLimitObservation(ctx, account.ID, account.Fingerprint, time.Now().UTC(), &sample, nil); err != nil {
		t.Fatal(err)
	}
	response, err = s.AccountLimits(ctx)
	if err != nil || response.Items[0].State != "stale" {
		t.Fatal(response, err)
	}
	item := response.Items[0]
	if item.ResetCreditsAvailable == nil || *item.ResetCreditsAvailable != 2 || len(item.Buckets) != 1 || item.Buckets[0].Primary == nil || item.Buckets[0].Primary.RemainingPercent != 70 {
		t.Fatal("elapsed reset changed observed values", item)
	}
	profile := s.Profiles.Profiles["fast"]
	profile.Auth.Key = "new.json"
	s.Profiles.Profiles["fast"] = profile
	profile = s.Profiles.Profiles["deep"]
	profile.Auth.Key = "new.json"
	s.Profiles.Profiles["deep"] = profile
	response, err = s.AccountLimits(ctx)
	if err != nil || response.Items[0].State != "unknown" || response.Items[0].ResetCreditsAvailable != nil {
		t.Fatal(response, err)
	}
	if err := s.SaveAccountLimitObservation(ctx, account.ID, account.Fingerprint, time.Now().UTC(), &sample, nil); err == nil {
		t.Fatal("old source was allowed to publish")
	}
}

func TestAccountLimitsContextErrorLogging(t *testing.T) {
	s, _ := accountLimitFixture(t)
	for _, kind := range []string{"deadline", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			var output bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			ctx, cancel := context.WithCancel(t.Context())
			if kind == "deadline" {
				cancel()
				ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
			} else {
				cancel()
			}
			defer cancel()
			if _, err := s.AccountLimits(ctx); err == nil {
				t.Fatal("expired context returned success")
			}
			if kind == "cancelled" {
				if output.Len() != 0 {
					t.Fatal("client cancellation logged as a failure", output.String())
				}
				return
			}
			var record struct {
				Level     string `json:"level"`
				Message   string `json:"msg"`
				ErrorType string `json:"error_type"`
			}
			if err := json.Unmarshal(output.Bytes(), &record); err != nil || record.Level != "WARN" || record.Message != "Account limits query failed" || !strings.HasPrefix(record.ErrorType, "deadline_exceeded:") {
				t.Fatal("deadline warning missing", output.String(), err)
			}
		})
	}
}

func TestAccountResetCreditsStoredSnapshots(t *testing.T) {
	s, account := accountLimitFixture(t)
	ctx := t.Context()
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	// An observation written before reset credits were collected has no field.
	_, err := s.Pool.Exec(ctx, `INSERT INTO account_limit_observations
		(account_id, source_fingerprint, last_attempt_at, last_success_at, snapshot)
		VALUES ($1, $2, $3, $3, '{"buckets":[]}')`, account.ID, account.Fingerprint, at)
	if err != nil {
		t.Fatal(err)
	}
	for i, count := range []*int64{nil, new(int64(2)), new(int64(0)), nil} {
		if i > 0 {
			at = at.Add(time.Second)
			snapshot := accountlimits.Snapshot{Buckets: []accountlimits.Bucket{}, ResetCreditsAvailable: count}
			if err := s.SaveAccountLimitObservation(ctx, account.ID, account.Fingerprint, at, &snapshot, nil); err != nil {
				t.Fatal(err)
			}
		}
		report, err := s.AccountLimits(ctx)
		if err != nil || len(report.Items) != 1 {
			t.Fatal(report, err)
		}
		item := report.Items[0]
		if item.State != "fresh" || item.ObservedAt == nil || !item.ObservedAt.Equal(at) || len(item.Profiles) != 2 {
			t.Fatal(item)
		}
		if (item.ResetCreditsAvailable == nil) != (count == nil) || count != nil && *item.ResetCreditsAvailable != *count {
			t.Fatal("count changed during storage read", item)
		}
	}
}
