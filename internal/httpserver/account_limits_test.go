//go:build integration

package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus/client"
	"github.com/orpheus-agents/orpheus/internal/accountlimits"
	"github.com/orpheus-agents/orpheus/internal/config"
)

func TestAccountLimitsHTTPAndGeneratedClient(t *testing.T) {
	if client.AccountLimitItemErrorCode("<nil>").Valid() {
		t.Fatal("generated enum accepts a synthetic null string")
	}
	server, storage := testServer(t)
	status, headers, body := requestHTTP(t, server, "GET", "/api/v1/accounts/limits", "", "key", "")
	if status != 200 || headers.Get("Cache-Control") != "no-store" {
		t.Fatal(status, string(body))
	}
	var empty client.AccountLimits
	if err := json.Unmarshal(body, &empty); err != nil || len(empty.Items) != 0 {
		t.Fatal(empty, err)
	}
	p, err := config.ReadProfiles(strings.NewReader(`[credential_stores.s]
bucket="test-credentials"
[profiles.account]
harness="codex"
[profiles.account.auth]
mode="account"
account_id="team-main"
store="s"
key="auth.json"
`))
	if err != nil {
		t.Fatal(err)
	}
	storage.Profiles = p
	metrics := SystemHandler(new(atomic.Bool), storage.AccountLimits)
	scrape := func(want string) {
		t.Helper()
		w := httptest.NewRecorder()
		metrics.ServeHTTP(w, httptest.NewRequest("GET", "/metrics/service", nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), want) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	scrape(`orpheus_account_limit_state{account_id="team-main",state="unknown"} 1`)
	account := p.Accounts()[0]
	c, err := client.NewClientWithResponses(server.URL, client.WithHTTPClient(server.Client()), client.WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer key")
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.GetAccountLimitsWithResponse(t.Context())
	if err != nil || res.JSON200 == nil || len(res.JSON200.Items) != 1 || res.JSON200.Items[0].State != client.AccountLimitItemState("unknown") {
		t.Fatal(res, err)
	}
	if !strings.Contains(string(res.Body), `"reset_credits_available":null`) {
		t.Fatal("unknown count omitted", string(res.Body))
	}
	base := time.Now().UTC().Truncate(time.Microsecond)
	code := "no_data"
	if err := storage.SaveAccountLimitObservation(t.Context(), account.ID, account.Fingerprint, base.Add(-2*time.Second), nil, &code); err != nil {
		t.Fatal(err)
	}
	res, err = c.GetAccountLimitsWithResponse(t.Context())
	if err != nil || res.JSON200 == nil || res.JSON200.Items[0].State != client.AccountLimitItemState("unavailable") || len(res.JSON200.Items[0].Buckets) != 0 {
		t.Fatal(res, err)
	}
	reset := time.Now().UTC().Add(time.Hour)
	sample := accountlimits.Snapshot{ResetCreditsAvailable: new(int64(2)), Buckets: []accountlimits.Bucket{{LimitID: "codex", PlanType: new("pro"), Primary: &accountlimits.Window{UsedPercent: 25, RemainingPercent: 75, WindowMinutes: new(int64(300)), ResetsAt: &reset}}}}
	if err := storage.SaveAccountLimitObservation(t.Context(), account.ID, account.Fingerprint, base.Add(-time.Second), &sample, nil); err != nil {
		t.Fatal(err)
	}
	res, err = c.GetAccountLimitsWithResponse(t.Context())
	if err != nil || res.JSON200 == nil || len(res.JSON200.Items) != 1 {
		t.Fatal(res, err)
	}
	item := res.JSON200.Items[0]
	if item.AccountID != "team-main" || item.State != client.AccountLimitItemState("fresh") || len(item.Buckets) != 1 || item.Buckets[0].Primary == nil || item.Buckets[0].Primary.RemainingPercent != 75 || item.Buckets[0].Primary.WindowMinutes == nil || *item.Buckets[0].Primary.WindowMinutes != 300 || item.ResetCreditsAvailable == nil || *item.ResetCreditsAvailable != 2 {
		t.Fatal(item)
	}
	if strings.Contains(string(res.Body), "auth.json") || strings.Contains(string(res.Body), "test-credentials") {
		t.Fatal("credential source leaked")
	}
	scrape(`orpheus_account_reset_credits_available{account_id="team-main"} 2`)
	scrape(`orpheus_account_limit_state{account_id="team-main",state="fresh"} 1`)
	code = "temporarily_unavailable"
	if err := storage.SaveAccountLimitObservation(t.Context(), account.ID, account.Fingerprint, base, nil, &code); err != nil {
		t.Fatal(err)
	}
	res, err = c.GetAccountLimitsWithResponse(t.Context())
	if err != nil || res.JSON200 == nil || res.JSON200.Items[0].State != client.AccountLimitItemState("stale") || len(res.JSON200.Items[0].Buckets) != 1 || res.JSON200.Items[0].ResetCreditsAvailable == nil || *res.JSON200.Items[0].ResetCreditsAvailable != 2 {
		t.Fatal(res, err)
	}
	scrape(`orpheus_account_limit_state{account_id="team-main",state="stale"} 1`)
	scrape(`orpheus_account_reset_credits_available{account_id="team-main"} 2`)
	for i, count := range []*int64{new(int64(0)), nil} {
		sample.ResetCreditsAvailable = count
		if err := storage.SaveAccountLimitObservation(t.Context(), account.ID, account.Fingerprint, base.Add(time.Duration(i+1)*time.Microsecond), &sample, nil); err != nil {
			t.Fatal(err)
		}
		status, _, body := requestHTTP(t, server, "GET", "/api/v1/accounts/limits", "", "key", "")
		want := `"reset_credits_available":0`
		if count == nil {
			want = `"reset_credits_available":null`
		}
		if status != 200 || !strings.Contains(string(body), want) {
			t.Fatal(status, string(body))
		}
	}
}

func TestAccountLimitsStorageFailure(t *testing.T) {
	server, storage := testServer(t)
	storage.Pool.Close()
	status, _, body := requestHTTP(t, server, "GET", "/api/v1/accounts/limits", "", "key", "")
	if status != 503 || !strings.Contains(string(body), `"account_limits_unavailable"`) {
		t.Fatal(status, string(body))
	}
	w := httptest.NewRecorder()
	SystemHandler(new(atomic.Bool), storage.AccountLimits).ServeHTTP(w, httptest.NewRequest("GET", "/metrics/service", nil))
	if w.Code != 503 || strings.Contains(w.Body.String(), "orpheus_account_") {
		t.Fatal(w.Code, w.Body.String())
	}
}
