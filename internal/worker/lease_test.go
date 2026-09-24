//go:build integration

package worker

import (
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus/internal/session"
)

func TestSandboxLeaseIndependentOfExecutionDeadlines(t *testing.T) {
	for _, tc := range []struct {
		name      string
		run, hook int
		grace     time.Duration
	}{
		{"short_run", 1, 1, 30 * time.Second},
		{"mattermost_defaults", 3600, 120, 30 * time.Second},
		{"long_deadlines", 86400, 7200, 2 * time.Hour},
		{"maximum_deadlines", 2147483647, 2147483647, 30 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, remote, id, e := setup(t)
			remote.maxTimeout = time.Hour
			s.Settings.CancelGrace = tc.grace
			record, run, err := s.Read(t.Context(), id.SessionID)
			if err != nil || run == nil {
				t.Fatal("read pending run", err)
			}
			// Exercise sandbox management without running the harness or hooks.
			record.Configuration.Public.Limits.RunTimeoutSeconds = tc.run
			record.Configuration.Public.Hooks.TimeoutSeconds = tc.hook
			ensure := func() {
				t.Helper()
				if err := e.ensureSandbox(t.Context(), &record, run); err != nil {
					t.Fatal(err)
				}
			}
			ensure()
			initialRenewals := remote.renewals
			e.timeoutRenewAt = time.Time{}
			ensure()
			e.Disconnect()
			ensure() // Reconnect after worker restart.
			if err := remote.Pause(t.Context()); err != nil {
				t.Fatal(err)
			}
			e.Disconnect()
			ensure() // Resume a paused sandbox.
			if remote.creates != 1 || remote.resumes != 2 || remote.renewals != initialRenewals+1 {
				t.Fatalf("create=%d connect=%d renew=%d", remote.creates, remote.resumes, remote.renewals)
			}
			for _, timeout := range remote.timeouts {
				if timeout != 5*time.Minute {
					t.Fatalf("sandbox timeout = %v, want 5m", timeout)
				}
			}
		})
	}
}

func TestSandboxLeaseRenewalPreservesRunDeadline(t *testing.T) {
	s, remote, id, e := setup(t)
	remote.maxTimeout = time.Hour
	tick(t, e)
	_, run, err := s.Read(t.Context(), id.SessionID)
	if err != nil || run == nil || run.Status != session.Running || run.DeadlineAt == nil || run.ExecutionStartedAt == nil {
		t.Fatal("run did not start", run, err)
	}
	deadline := *run.DeadlineAt
	if deadline.Sub(*run.ExecutionStartedAt) != time.Hour {
		t.Fatal("sandbox lease shortened the run deadline")
	}
	e.timeoutRenewAt = time.Time{}
	tick(t, e)
	_, run, err = s.Read(t.Context(), id.SessionID)
	if err != nil || run == nil || run.Status != session.Running || run.DeadlineAt == nil || !run.DeadlineAt.Equal(deadline) {
		t.Fatal("lease renewal changed run deadline", run, err)
	}
}
