package worker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/accountlimits"
	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/diagnostic"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
)

type limitObservation struct {
	at           time.Time
	snapshot     *accountlimits.Snapshot
	code         *string
	donorFailure bool
}
type limitDonor struct {
	reader     harness.AccountLimitsReader
	ctx        context.Context
	cancel     context.CancelFunc
	readDone   chan struct{}
	failures   int
	retryUntil time.Time
	blocked    bool
}

func (d *limitDonor) observe(value limitObservation, now time.Time) {
	if value.code == nil {
		d.failures = 0
		d.retryUntil = time.Time{}
		d.blocked = false
		return
	}
	if *value.code == "unsupported" || *value.code == "authentication_unavailable" {
		d.blocked = true
		return
	}
	d.failures++
	d.retryUntil = now.Add(backoff(d.failures))
}

type limitReadState struct {
	preferred          uuid.UUID
	lastDonor          uuid.UUID
	providerRetryUntil time.Time
	providerFailures   int
	backendFailures    int
	lastBackendDonor   uuid.UUID
}

// advanceLimitReadState decides account-wide backoff and failover without I/O.
// Unsupported and unavailable authentication are local checks; transport and
// invalid-response failures may have reached the provider and count toward the cap.
func advanceLimitReadState(state limitReadState, id uuid.UUID, value limitObservation, now time.Time, hasAlternate bool) (limitReadState, bool) {
	switch {
	case value.code == nil:
		state.preferred = id
		state.providerFailures = 0
		state.providerRetryUntil = time.Time{}
		state.backendFailures = 0
		return state, false
	case !value.donorFailure:
		state.providerFailures++
		state.providerRetryUntil = now.Add(backoff(state.providerFailures))
		state.backendFailures = 0
		return state, false
	}
	state.lastDonor = id
	if state.preferred == id {
		state.preferred = uuid.Nil
	}
	if *value.code == "temporarily_unavailable" || *value.code == "invalid_response" {
		if state.backendFailures == 0 || state.lastBackendDonor != id {
			state.backendFailures++
			state.lastBackendDonor = id
		}
	}
	if state.backendFailures >= 2 {
		state.providerFailures++
		state.providerRetryUntil = now.Add(backoff(state.providerFailures))
		state.backendFailures = 0
		return state, false
	}
	if !hasAlternate {
		state.backendFailures = 0
		return state, false
	}
	return state, true
}

type limitAccount struct {
	config config.Account
	mu     sync.Mutex
	donors map[uuid.UUID]*limitDonor
	limitReadState
	wake       chan struct{}
	next       time.Time
	dirtyAt    time.Time
	retryUntil time.Time
	failures   int
	pending    *limitObservation
	fallback   *limitObservation
	generation uint64
}

func (a *limitAccount) signal() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// selected keeps the last successful donor until it becomes unavailable.
// The earliest retry wakes an account only when every donor is cooling down or blocked.
func (a *limitAccount) selected(now time.Time) (uuid.UUID, *limitDonor, time.Time) {
	if candidate := a.donors[a.preferred]; candidate != nil && !candidate.blocked && !candidate.retryUntil.After(now) {
		return a.preferred, candidate, time.Time{}
	}
	var selected, afterLast uuid.UUID
	var donor, after *limitDonor
	var retryAt time.Time
	for id, candidate := range a.donors {
		if candidate.blocked {
			continue
		}
		if candidate.retryUntil.After(now) {
			if retryAt.IsZero() || candidate.retryUntil.Before(retryAt) {
				retryAt = candidate.retryUntil
			}
			continue
		}
		if donor == nil || bytes.Compare(id[:], selected[:]) < 0 {
			selected, donor = id, candidate
		}
		if bytes.Compare(id[:], a.lastDonor[:]) > 0 && (after == nil || bytes.Compare(id[:], afterLast[:]) < 0) {
			afterLast, after = id, candidate
		}
	}
	if after != nil {
		return afterLast, after, retryAt
	}
	return selected, donor, retryAt
}
func (a *limitAccount) markDirty(now time.Time) {
	if a.dirtyAt.IsZero() {
		a.dirtyAt = now.Add(5 * time.Second)
	}
	a.signal()
}

type AccountLimitsCollector struct {
	ctx      context.Context
	save     func(context.Context, string, string, time.Time, *accountlimits.Snapshot, *string) error
	accounts map[string]*limitAccount
	sem      chan struct{}
	wg       sync.WaitGroup
}

func newAccountLimitsCollector(ctx context.Context, s *store.Store) *AccountLimitsCollector {
	c := &AccountLimitsCollector{ctx: ctx, save: s.SaveAccountLimitObservation, accounts: map[string]*limitAccount{}, sem: make(chan struct{}, 4)}
	for _, account := range s.Profiles.Accounts() {
		a := &limitAccount{config: account, donors: map[uuid.UUID]*limitDonor{}, wake: make(chan struct{}, 1)}
		c.accounts[account.ID] = a
		c.wg.Go(func() { c.run(a) })
	}
	return c
}
func (c *AccountLimitsCollector) Wait() { c.wg.Wait() }

// Register uses the immutable session credentials, never a re-resolved profile.
// The returned function must run before the driver is closed or its sandbox paused.
func (c *AccountLimitsCollector) Register(id uuid.UUID, cfg session.ResolvedConfiguration, driver harness.Driver) func() {
	if c == nil || cfg.Credentials.AccountID == "" || cfg.Credentials.Mode != "account" {
		return func() {}
	}
	a := c.accounts[cfg.Credentials.AccountID]
	if a == nil {
		return func() {}
	}
	fingerprint, err := config.SourceFingerprint(cfg.Harness, cfg.Credentials)
	if err != nil || fingerprint != a.config.Fingerprint {
		return func() {}
	}
	reader, ok := driver.(harness.AccountLimitsReader)
	if !ok {
		code := "unsupported"
		c.record(a, limitObservation{at: time.Now().UTC(), code: &code})
		return func() {}
	}
	ctx, cancel := context.WithCancel(c.ctx)
	donor := &limitDonor{reader: reader, ctx: ctx, cancel: cancel}
	a.mu.Lock()
	now := time.Now()
	selected, _, _ := a.selected(now)
	if old := a.donors[id]; old != nil {
		old.cancel()
	}
	a.donors[id] = donor
	next, _, _ := a.selected(now)
	if next != selected || selected == id {
		a.generation++
		a.next = now
	}
	a.signal()
	a.mu.Unlock()
	c.wg.Go(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-reader.AccountLimitsEvents():
				if !ok {
					return
				}
				if reader.AccountLimitsDirty() {
					a.mu.Lock()
					if a.donors[id] == donor {
						a.markDirty(time.Now())
					}
					a.mu.Unlock()
				}
			}
		}
	})
	return func() {
		a.mu.Lock()
		if a.donors[id] == donor {
			now := time.Now()
			selected, _, _ := a.selected(now)
			delete(a.donors, id)
			if a.preferred == id {
				a.preferred = uuid.Nil
			}
			next, _, _ := a.selected(now)
			if selected != next {
				a.generation++
				a.next = now
			}
			a.signal()
		}
		done := donor.readDone
		a.mu.Unlock()
		cancel()
		if done != nil {
			<-done
		}
	}
}

func (c *AccountLimitsCollector) record(a *limitAccount, value limitObservation) {
	a.mu.Lock()
	if len(a.donors) > 0 {
		a.mu.Unlock()
		return
	}
	a.pending = &value
	a.next = time.Now()
	a.signal()
	a.mu.Unlock()
}

func (c *AccountLimitsCollector) run(a *limitAccount) {
	for c.ctx.Err() == nil {
		a.mu.Lock()
		id, donor, donorRetryAt := a.selected(time.Now())
		pending := a.pending
		if donor == nil && pending == nil && a.fallback != nil {
			a.pending = a.fallback
			pending = a.pending
			a.fallback = nil
		}
		generation := a.generation
		dirtyAt := a.dirtyAt
		if donor == nil && pending == nil {
			a.mu.Unlock()
			retryDelay := time.Duration(0)
			if !donorRetryAt.IsZero() {
				retryDelay = max(time.Until(donorRetryAt), time.Nanosecond)
			}
			if !c.wait(a, retryDelay) {
				return
			}
			continue
		}
		due := a.next
		if !a.dirtyAt.IsZero() && (due.IsZero() || a.dirtyAt.Before(due)) && pending == nil {
			due = a.dirtyAt
		}
		if a.retryUntil.After(due) {
			due = a.retryUntil
		}
		if pending == nil && a.providerRetryUntil.After(due) {
			due = a.providerRetryUntil
		}
		if due.IsZero() {
			due = time.Now()
		}
		wait := time.Until(due)
		if wait > 0 {
			a.mu.Unlock()
			if !c.wait(a, wait) {
				return
			}
			continue
		}
		if pending == nil {
			donor.readDone = make(chan struct{})
		}
		a.mu.Unlock()
		if pending == nil {
			value := c.read(donor)
			a.mu.Lock()
			if a.donors[id] != donor || a.generation != generation {
				close(donor.readDone)
				donor.readDone = nil
				a.mu.Unlock()
				continue
			}
			now := time.Now()
			hasAlternate := false
			if value.code == nil || value.donorFailure {
				donor.observe(value, now)
				if value.donorFailure {
					nextID, next, _ := a.selected(now)
					hasAlternate = next != nil && nextID != id
				}
			}
			state, failover := advanceLimitReadState(a.limitReadState, id, value, now, hasAlternate)
			a.limitReadState = state
			if value.donorFailure {
				a.fallback = &value
				if failover {
					a.generation++
					a.next = now
					if a.dirtyAt.Equal(dirtyAt) {
						a.dirtyAt = time.Time{}
					}
					close(donor.readDone)
					donor.readDone = nil
					a.mu.Unlock()
					continue
				}
			}
			a.fallback = nil
			a.pending = &value
			pending = &value
			if a.dirtyAt.Equal(dirtyAt) {
				a.dirtyAt = time.Time{}
			}
			close(donor.readDone)
			donor.readDone = nil
			a.mu.Unlock()
		}
		err := c.save(c.ctx, a.config.ID, a.config.Fingerprint, pending.at, pending.snapshot, pending.code)
		a.mu.Lock()
		if err == nil && a.pending == pending {
			a.pending = nil
		}
		switch {
		case err != nil:
			a.failures++
			a.retryUntil = time.Now().Add(backoff(a.failures))
			a.next = a.retryUntil
		case pending.code != nil:
			a.failures = 0
			a.retryUntil = time.Time{}
			if a.providerRetryUntil.IsZero() {
				a.next = time.Now()
			} else {
				a.next = a.providerRetryUntil
			}
		default:
			a.failures = 0
			a.retryUntil = time.Time{}
			a.next = time.Now().Add(time.Duration(54+rand.IntN(13)) * time.Second)
		}
		if a.generation != generation || a.pending != nil && a.pending != pending {
			a.next = time.Now()
		}
		a.mu.Unlock()
		if err != nil && c.ctx.Err() == nil {
			slog.WarnContext(c.ctx, "Account limits observation failed", "account_id", a.config.ID, "error_type", diagnostic.Describe(err))
		}
	}
}
func (c *AccountLimitsCollector) wait(a *limitAccount, delay time.Duration) bool {
	if delay <= 0 {
		select {
		case <-c.ctx.Done():
			return false
		case <-a.wake:
			return true
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-c.ctx.Done():
		return false
	case <-a.wake:
		return true
	case <-timer.C:
		return true
	}
}
func backoff(failures int) time.Duration {
	switch failures {
	case 1:
		return time.Minute
	case 2:
		return 2 * time.Minute
	case 3:
		return 4 * time.Minute
	default:
		return 5 * time.Minute
	}
}
func (c *AccountLimitsCollector) read(donor *limitDonor) limitObservation {
	select {
	case c.sem <- struct{}{}:
	case <-donor.ctx.Done():
		return limitObservation{at: time.Now().UTC(), code: new("temporarily_unavailable"), donorFailure: true}
	}
	defer func() { <-c.sem }()
	ctx, cancel := context.WithTimeout(donor.ctx, 3*time.Second)
	defer cancel()
	snapshot, err := donor.reader.ReadAccountLimits(ctx)
	at := time.Now().UTC()
	if err == nil {
		return limitObservation{at: at, snapshot: &snapshot}
	}
	code := "temporarily_unavailable"
	donorFailure := errors.Is(err, harness.ErrUncertain) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil
	switch {
	case errors.Is(err, accountlimits.ErrUnsupported):
		code = "unsupported"
		donorFailure = true
	case errors.Is(err, accountlimits.ErrNoData):
		code = "no_data"
	case errors.Is(err, accountlimits.ErrInvalidResponse):
		code = "invalid_response"
		donorFailure = true
	case errors.Is(err, accountlimits.ErrAuthenticationUnavailable):
		code = "authentication_unavailable"
		donorFailure = true
	}
	return limitObservation{at: at, code: &code, donorFailure: donorFailure}
}
