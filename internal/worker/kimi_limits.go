package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/orpheus-agents/orpheus/internal/accountlimits"
	"github.com/orpheus-agents/orpheus/internal/store"
)

type keyLimitsStore interface {
	KimiLimitCredentials(context.Context) ([]store.KimiLimitCredential, error)
	SaveAccountLimitObservation(context.Context, string, string, time.Time, *accountlimits.Snapshot, *string) error
}

// This poller is owned by the executor lock. Account discovery is independent of
// running sandboxes; replacing a key schedules the new source on the next tick.
func pollKimiLimits(ctx context.Context, s keyLimitsStore, read func(context.Context, string) (accountlimits.Snapshot, error)) {
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	next := map[string]time.Time{}
	for ctx.Err() == nil {
		keys, err := s.KimiLimitCredentials(ctx)
		if err != nil {
			slog.WarnContext(ctx, "Kimi quota catalog unavailable")
		} else {
			tracked := make(map[string]time.Time, len(keys))
			for _, k := range keys {
				if ctx.Err() != nil {
					return
				}
				retry := next[k.Fingerprint]
				if time.Now().Before(retry) {
					tracked[k.Fingerprint] = retry
					continue
				}
				readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				var v accountlimits.Snapshot
				var readErr error
				if k.Key == "" {
					readErr = accountlimits.ErrAuthenticationUnavailable
				} else {
					v, readErr = read(readCtx, k.Key)
				}
				cancel()
				at := time.Now().UTC()
				o := observedLimit(ctx, at, v, readErr)
				if saveErr := s.SaveAccountLimitObservation(ctx, k.AccountID, k.Fingerprint, at, o.snapshot, o.code); saveErr != nil && ctx.Err() == nil {
					slog.WarnContext(ctx, "Kimi quota observation was not saved", "account_id", k.AccountID)
				}
				tracked[k.Fingerprint] = at.Add(2 * time.Minute)
			}
			next = tracked
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
