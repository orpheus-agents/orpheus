package worker

import (
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/servicecredentials"
)

func TestExecutionDeadline(t *testing.T) {
	issued := time.Date(2026, 11, 5, 15, 44, 29, 0, time.UTC)
	settings := config.DefaultSettings()
	for _, tc := range []struct {
		name     string
		delay    time.Duration
		timeout  int
		absolute []time.Duration
		want     time.Duration
	}{
		{"unprotected", 4 * time.Second, 1200, nil, 1204 * time.Second},
		{"zero absolute ignored", 4 * time.Second, 1200, []time.Duration{0}, 1204 * time.Second},
		{"preparation consumed lifetime", 4 * time.Second, 1200, []time.Duration{1200 * time.Second}, 1169 * time.Second},
		{"long preparation", 10 * time.Minute, 1200, []time.Duration{1200 * time.Second}, 1169 * time.Second},
		{"short configured limit", 4 * time.Second, 60, []time.Duration{1200 * time.Second}, 64 * time.Second},
		{"maximum configured limit", 4 * time.Second, 2147483647, []time.Duration{1200 * time.Second}, 1169 * time.Second},
		{"earliest service wins", 4 * time.Second, 1200, []time.Duration{1200 * time.Second, 500 * time.Second}, 469 * time.Second},
		{"inside stop window", 1180 * time.Second, 1200, []time.Duration{1200 * time.Second}, 1169 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := servicecredentials.State{Entries: map[string]servicecredentials.Entry{}}
			for i, absolute := range tc.absolute {
				grant := servicecredentials.Grant{ExpiresAt: issued.Add(time.Minute)}
				if absolute != 0 {
					grant.RunExpiresAt = issued.Add(absolute)
				}
				state.Entries[string(rune('a'+i))] = servicecredentials.Entry{Grant: grant}
			}
			got := executionDeadline(issued.Add(tc.delay), tc.timeout, state, settings)
			if want := issued.Add(tc.want); !got.Equal(want) {
				t.Fatalf("deadline = %v, want %v", got, want)
			}
			// Lease renewal does not change the absolute execution bound.
			for code, entry := range state.Entries {
				entry.Grant.ExpiresAt = issued.Add(5 * time.Minute)
				state.Entries[code] = entry
			}
			if renewed := executionDeadline(issued.Add(tc.delay), tc.timeout, state, settings); !renewed.Equal(got) {
				t.Fatal("lease renewal moved deadline", renewed, got)
			}
		})
	}
}

func TestExecutionDeadlineHonorsLargeCancellationWindows(t *testing.T) {
	now := time.Now().UTC()
	absolute := now.Add(20 * time.Minute)
	state := servicecredentials.State{Entries: map[string]servicecredentials.Entry{
		"kord": {Grant: servicecredentials.Grant{RunExpiresAt: absolute}},
	}}
	for _, tc := range []struct {
		grace, poll time.Duration
	}{
		{20 * time.Minute, time.Second},
		{2 * time.Hour, time.Second},
		{time.Second, 2 * time.Hour},
		{time.Duration(1<<63 - 1), time.Duration(1<<63 - 1)},
	} {
		settings := config.Settings{CancelGrace: tc.grace, WorkerPoll: tc.poll}
		got := executionDeadline(now, 1200, state, settings)
		if !got.Before(now) || !got.Before(absolute.Add(-tc.grace)) {
			t.Fatalf("unsafe cancellation window (%v, %v): %v", tc.grace, tc.poll, got)
		}
	}
}
