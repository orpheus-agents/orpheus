package accountlimits

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type kimiTransport func(*http.Request) (*http.Response, error)

func (f kimiTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestKimiUsage(t *testing.T) {
	const payload = `{"usages":{"limit_month_total":{"used_ratio":1.25,"reset_time":"2026-12-01T00:00:00Z"},"limit_5h":{"used_ratio":0.2,"reset_time":"2026-11-20T05:00:00Z"}}}`
	for _, tc := range []struct {
		status int
		body   string
		want   error
	}{
		{200, payload, nil}, {401, "denied", ErrAuthenticationUnavailable}, {403, "denied", ErrAuthenticationUnavailable}, {200, `{"usages":{}}`, ErrNoData}, {200, `{"usages":{"x":{"used_ratio":-1}}}`, ErrInvalidResponse}, {200, strings.Repeat(" ", (1<<20)+1), ErrInvalidResponse},
	} {
		c := &http.Client{Transport: kimiTransport(func(r *http.Request) (*http.Response, error) {
			if r.Method != "GET" || r.URL.String() != "https://api.kimi.ai/coding/v1/usages" || r.Header.Get("Authorization") != "Bearer test-key" {
				t.Fatal("wrong Kimi request")
			}
			return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: http.Header{}}, nil
		})}
		s, err := readKimi(t.Context(), c, "test-key")
		if !errors.Is(err, tc.want) {
			t.Fatal(err, tc.want)
		}
		if err == nil && (len(s.Buckets) != 2 || *s.Buckets[0].Primary.WindowMinutes != 300 || s.Buckets[0].Primary.RemainingPercent != 80 || s.Buckets[1].Primary.UsedPercent != 125 || s.Buckets[1].Primary.RemainingPercent != 0 || s.Buckets[1].Primary.WindowMinutes != nil) {
			t.Fatal(s)
		}
	}
}

func TestConsoleAllowances(t *testing.T) {
	const raw = `{"workspace":{"id":"ws"},"go":{"rolling":{"percent":2,"resetsAt":1800000000000},"weekly":{"percent":26,"resetsAt":1800100000000},"monthly":{"percent":75,"resetsAt":1800200000000}}}`
	s, err := ParseConsole([]byte(raw))
	if err != nil || len(s.Buckets) != 3 || *s.Buckets[0].Primary.WindowMinutes != 300 || *s.Buckets[1].Primary.WindowMinutes != 10080 || s.Buckets[2].Primary.WindowMinutes != nil || s.Buckets[0].Primary.RemainingPercent != 98 || !s.Buckets[0].Primary.ResetsAt.Equal(time.UnixMilli(1800000000000)) {
		t.Fatal(s, err)
	}
	s, err = ParseConsole([]byte(`{"workspace":{"id":"ws"},"budgets":[]}`))
	if err != nil || s.Buckets == nil || len(s.Buckets) != 0 {
		t.Fatal(s, err)
	}
	for _, tc := range []struct {
		input string
		want  error
	}{
		{"null", ErrNoData}, {"{}", ErrInvalidResponse}, {strings.Replace(raw, "rolling", "unknown", 1), ErrInvalidResponse},
	} {
		if _, err := ParseConsole([]byte(tc.input)); !errors.Is(err, tc.want) {
			t.Fatal(err)
		}
	}
}
