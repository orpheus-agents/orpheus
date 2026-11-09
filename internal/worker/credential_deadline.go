package worker

import (
	"time"

	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/servicecredentials"
)

// Compute once at the first dispatch. Renewal and worker recovery must keep the
// persisted deadline, even when a renewed lease would allow more execution.
func executionDeadline(now time.Time, timeout int, credentials servicecredentials.State, settings config.Settings) time.Time {
	deadline := now.Add(time.Duration(timeout) * time.Second)
	for _, entry := range credentials.Entries {
		absolute := entry.Grant.RunExpiresAt
		if absolute.IsZero() {
			continue
		}
		// Honor the full configured cancellation window. Subtract separately to
		// avoid duration overflow; an exhausted window must prevent dispatch.
		bound := absolute.Add(-max(settings.CancelGrace, 0)).Add(-max(settings.WorkerPoll, 0))
		if bound.Before(deadline) {
			deadline = bound
		}
	}
	return deadline
}
