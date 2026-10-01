package accountlimits

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseResetCreditsFixture(t *testing.T) {
	raw, err := os.ReadFile("../../tests/fixtures/codex-reset-credits-0.159.3.json")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := Parse(raw)
	if err != nil || snapshot.ResetCreditsAvailable == nil || *snapshot.ResetCreditsAvailable != 3 || len(snapshot.Buckets) != 1 {
		t.Fatal(snapshot, err)
	}
}

func TestParseResetCredits(t *testing.T) {
	for _, limits := range []string{
		`"rateLimitsByLimitId":{"codex":{}}`,
		`"rateLimits":{"limitId":"codex"}`,
		`"rateLimitsByLimitId":{}`,
	} {
		for _, tc := range []struct {
			name, credits string
			want          *int64
		}{
			{"absent", "", nil},
			{"null", `,"rateLimitResetCredits":null`, nil},
			{"zero", `,"rateLimitResetCredits":{"availableCount":0}`, new(int64(0))},
			{"count", `,"rateLimitResetCredits":{"availableCount":3}`, new(int64(3))},
			{"capped details", `,"rateLimitResetCredits":{"availableCount":3,"credits":[{"id":"synthetic"}]}`, new(int64(3))},
			{"null details", `,"rateLimitResetCredits":{"availableCount":2,"credits":null}`, new(int64(2))},
		} {
			t.Run(limits+"/"+tc.name, func(t *testing.T) {
				snapshot, err := Parse([]byte("{" + limits + tc.credits + "}"))
				if err != nil {
					t.Fatal(err)
				}
				got := snapshot.ResetCreditsAvailable
				if (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want {
					t.Fatalf("reset credits: got %v want %v", got, tc.want)
				}
				raw, err := json.Marshal(snapshot)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(raw), "synthetic") || strings.Contains(string(raw), `"credits"`) {
					t.Fatal("credit details persisted")
				}
				var roundTrip Snapshot
				if err := json.Unmarshal(raw, &roundTrip); err != nil {
					t.Fatal(err)
				}
				if (roundTrip.ResetCreditsAvailable == nil) != (got == nil) || got != nil && *roundTrip.ResetCreditsAvailable != *got {
					t.Fatal("reset credits lost in stored JSON", string(raw))
				}
			})
		}
	}
}

func TestParseRejectsInvalidResetCredits(t *testing.T) {
	for _, credits := range []string{
		`{}`, `[]`, `"bad"`, `true`, `2`,
		`{"availableCount":null}`, `{"availableCount":"2"}`,
		`{"availableCount":-1}`, `{"availableCount":1.5}`,
		`{"availableCount":9223372036854775808}`,
	} {
		t.Run(credits, func(t *testing.T) {
			_, err := Parse([]byte(`{"rateLimitsByLimitId":{},"rateLimitResetCredits":` + credits + `}`))
			if !errors.Is(err, ErrInvalidResponse) {
				t.Fatal("accepted invalid reset summary", err)
			}
		})
	}
	if _, err := Parse([]byte(`{"rateLimitResetCredits":{"availableCount":2}}`)); !errors.Is(err, ErrNoData) {
		t.Fatal("reset credits bypassed missing limits", err)
	}
}

func TestParseCompleteMultiBucketSnapshot(t *testing.T) {
	raw, err := os.ReadFile("../../tests/fixtures/codex-rate-limits-0.156.1.json")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := Parse(raw)
	if err != nil || len(snapshot.Buckets) != 2 {
		t.Fatal(snapshot, err)
	}
	first, second := snapshot.Buckets[0], snapshot.Buckets[1]
	if first.LimitID != "codex" || first.Primary == nil || first.Primary.UsedPercent != 25 || first.Primary.RemainingPercent != 75 || *first.Primary.WindowMinutes != 300 || first.Primary.ResetsAt == nil || first.Primary.ResetsAt.Unix() != 1790251200 || first.Secondary != nil || first.PlanType == nil || *first.PlanType != "pro" {
		t.Fatal(first)
	}
	if second.LimitID != "other" || second.Primary != nil || second.Secondary == nil || second.Secondary.UsedPercent != 125 || second.Secondary.RemainingPercent != 0 || second.Secondary.ResetsAt != nil {
		t.Fatal(second)
	}
	if snapshot.ResetElapsed(time.Unix(1790251199, 0)) || !snapshot.ResetElapsed(time.Unix(1790251200, 0)) {
		t.Fatal("reset boundary")
	}
}

func TestParseLegacyAndEmptySnapshot(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
		count     int
	}{
		{`{"rateLimits":{"primary":{"usedPercent":0}}}`, "legacy", 1},
		{`{"rateLimits":{"limitId":"codex","primary":{"usedPercent":101}}}`, "codex", 1},
		{`{"rateLimits":{"limitId":"ignored"},"rateLimitsByLimitId":{}}`, "", 0},
	} {
		got, err := Parse([]byte(tc.raw))
		if err != nil || len(got.Buckets) != tc.count {
			t.Fatal(tc, got, err)
		}
		if tc.count == 1 && got.Buckets[0].LimitID != tc.want {
			t.Fatal(got)
		}
	}
}

func TestParseRejectsMalformedKnownFields(t *testing.T) {
	for _, raw := range []string{
		`null`, `{}`, `{"rateLimits":null}`, `{"rateLimitsByLimitId":null}`,
		`{"rateLimitsByLimitId":[]}`, `{"rateLimitsByLimitId":{"codex":{"limitId":"other"}}}`,
		`{"rateLimits":"bad","rateLimitsByLimitId":{"codex":{}}}`,
		`{"rateLimits":{"primary":{}}}`, `{"rateLimits":{"primary":{"usedPercent":-1}}}`,
		`{"rateLimits":{"primary":{"usedPercent":"25"}}}`,
		`{"rateLimits":{"primary":{"usedPercent":1,"windowDurationMins":0}}}`,
		`{"rateLimits":{"primary":{"usedPercent":1,"resetsAt":1790251200000}}}`,
		`{"rateLimits":{"limitName":7}}`,
	} {
		_, err := Parse([]byte(raw))
		if err == nil {
			t.Fatalf("accepted %s", raw)
		}
		if raw == `{}` || raw == `{"rateLimits":null}` || raw == `{"rateLimitsByLimitId":null}` {
			if !errors.Is(err, ErrNoData) {
				t.Fatal(raw, err)
			}
		} else if !errors.Is(err, ErrInvalidResponse) {
			t.Fatal(raw, err)
		}
	}
}
