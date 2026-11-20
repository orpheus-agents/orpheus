package worker

import (
	"context"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus/internal/accountlimits"
	"github.com/orpheus-agents/orpheus/internal/store"
)

type kimiLimitStore struct {
	credentials []store.KimiLimitCredential
	seen        []string
	failure     string
	cancel      context.CancelFunc
}

func (s *kimiLimitStore) KimiLimitCredentials(context.Context) ([]store.KimiLimitCredential, error) {
	return s.credentials, nil
}
func (s *kimiLimitStore) SaveAccountLimitObservation(_ context.Context, id, version string, at time.Time, snapshot *accountlimits.Snapshot, code *string) error {
	s.seen = append(s.seen, id+"/"+version)
	if code != nil {
		s.failure = *code
	}
	if len(s.seen) == len(s.credentials) {
		s.cancel()
	}
	return nil
}
func TestKimiPollsWithoutActiveSandbox(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s := &kimiLimitStore{credentials: []store.KimiLimitCredential{{AccountID: "unreadable", Fingerprint: "v1"}, {AccountID: "active", Fingerprint: "v2", Key: "test-key"}}, cancel: cancel}
	reads := 0
	pollKimiLimits(ctx, s, func(_ context.Context, key string) (accountlimits.Snapshot, error) {
		reads++
		if key != "test-key" {
			t.Fatal("unexpected key")
		}
		return accountlimits.Snapshot{}, nil
	})
	if reads != 1 || len(s.seen) != 2 || s.seen[0] != "unreadable/v1" || s.seen[1] != "active/v2" || s.failure != "authentication_unavailable" {
		t.Fatal(reads, s.seen, s.failure)
	}
}
