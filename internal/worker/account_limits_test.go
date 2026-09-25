//go:build integration

package worker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/accountlimits"
	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
)

type limitTestDriver struct {
	harness.Driver
	mu      sync.Mutex
	calls   int
	events  chan struct{}
	started chan struct{}
	block   bool
	release <-chan struct{}
	results []error
}

type lifecycleLimitDriver struct {
	*fakeDriver
	initialized atomic.Bool
	started     chan struct{}
	once        sync.Once
}

func (d *lifecycleLimitDriver) Initialize(ctx context.Context, source session.Credentials, login bool) error {
	if err := d.fakeDriver.Initialize(ctx, source, login); err != nil {
		return err
	}
	d.initialized.Store(true)
	return nil
}
func (d *lifecycleLimitDriver) ReadAccountLimits(ctx context.Context) (accountlimits.Snapshot, error) {
	d.once.Do(func() { close(d.started) })
	<-ctx.Done()
	return accountlimits.Snapshot{}, ctx.Err()
}
func (d *lifecycleLimitDriver) AccountLimitsEvents() <-chan struct{} { return nil }
func (d *lifecycleLimitDriver) AccountLimitsDirty() bool             { return false }

func (d *limitTestDriver) ReadAccountLimits(ctx context.Context) (accountlimits.Snapshot, error) {
	d.mu.Lock()
	d.calls++
	call := d.calls
	var result error
	if call <= len(d.results) {
		result = d.results[call-1]
	}
	d.mu.Unlock()
	if d.started != nil {
		select {
		case d.started <- struct{}{}:
		default:
		}
	}
	if d.block {
		<-ctx.Done()
		return accountlimits.Snapshot{}, ctx.Err()
	}
	if d.release != nil {
		select {
		case <-d.release:
		case <-ctx.Done():
			return accountlimits.Snapshot{}, ctx.Err()
		}
	}
	if result != nil {
		return accountlimits.Snapshot{}, result
	}
	return accountlimits.Snapshot{Buckets: []accountlimits.Bucket{{LimitID: "codex", Primary: &accountlimits.Window{UsedPercent: 25, RemainingPercent: 75}}}}, nil
}
func (d *limitTestDriver) AccountLimitsEvents() <-chan struct{} { return d.events }
func (d *limitTestDriver) AccountLimitsDirty() bool             { return true }
func (d *limitTestDriver) count() int                           { d.mu.Lock(); defer d.mu.Unlock(); return d.calls }

func TestCollectorSingleDonorFailoverAndUnregister(t *testing.T) {
	s, _, _, _ := setup(t)
	p, err := config.ReadProfiles(strings.NewReader(`[credential_stores.s]
bucket="test-credentials"
[profiles.a]
harness="codex"
[profiles.a.auth]
mode="account"
account_id="team-main"
store="s"
key="auth.json"
[profiles.b]
harness="codex"
[profiles.b.auth]
mode="account"
account_id="team-main"
store="s"
key="auth.json"
`))
	if err != nil {
		t.Fatal(err)
	}
	s.Profiles = p
	account := p.Accounts()[0]
	cfg := session.ResolvedConfiguration{Harness: "codex", Credentials: session.Credentials{Mode: "account", AccountID: account.ID, Store: new(p.CredentialStores["s"]), Key: "auth.json"}}
	ctx, cancel := context.WithCancel(t.Context())
	c := newAccountLimitsCollector(ctx, s)
	defer func() { cancel(); c.Wait() }()
	a := &limitTestDriver{events: make(chan struct{}, 1), started: make(chan struct{}, 1)}
	b := &limitTestDriver{events: make(chan struct{}, 1), started: make(chan struct{}, 1)}
	unregisterA := c.Register(uuid.MustParse("00000000-0000-0000-0000-000000000001"), cfg, a)
	defer unregisterA()
	select {
	case <-a.started:
	case <-time.After(time.Second):
		t.Fatal("initial read did not start")
	}
	until := time.Now().Add(time.Second)
	for {
		response, err := s.AccountLimits(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Items) == 1 && response.Items[0].State == "fresh" {
			break
		}
		if time.Now().After(until) {
			t.Fatal("sample not published", response)
		}
		time.Sleep(time.Millisecond)
	}
	unregisterB := c.Register(uuid.MustParse("00000000-0000-0000-0000-000000000002"), cfg, b)
	defer unregisterB()
	time.Sleep(20 * time.Millisecond)
	if a.count() != 1 || b.count() != 0 {
		t.Fatal("reserve donor caused duplicate read", a.count(), b.count())
	}
	unregisterA()
	select {
	case <-b.started:
	case <-time.After(time.Second):
		t.Fatal("failover read did not start")
	}
	if b.count() != 1 {
		t.Fatal("failover", b.count())
	}
}

func TestCollectorUnregisterCancelsInFlightRPC(t *testing.T) {
	s, _, _, _ := setup(t)
	p, err := config.ReadProfiles(strings.NewReader("[credential_stores.s]\nbucket='b'\n[profiles.a]\nharness='codex'\n[profiles.a.auth]\nmode='account'\naccount_id='a'\nstore='s'\nkey='auth'\n"))
	if err != nil {
		t.Fatal(err)
	}
	s.Profiles = p
	cfg := session.ResolvedConfiguration{Harness: "codex", Credentials: session.Credentials{Mode: "account", AccountID: "a", Store: new(p.CredentialStores["s"]), Key: "auth"}}
	ctx, cancel := context.WithCancel(t.Context())
	c := newAccountLimitsCollector(ctx, s)
	defer func() { cancel(); c.Wait() }()
	d := &limitTestDriver{events: make(chan struct{}, 1), started: make(chan struct{}, 1), block: true}
	unregister := c.Register(uuid.New(), cfg, d)
	select {
	case <-d.started:
	case <-time.After(time.Second):
		t.Fatal("read did not start")
	}
	done := make(chan struct{})
	go func() { unregister(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("unregister blocked on RPC")
	}
}

func TestCollectorUnregisterDoesNotWaitForStorage(t *testing.T) {
	s, _, _, _ := setup(t)
	p, err := config.ReadProfiles(strings.NewReader("[credential_stores.s]\nbucket='b'\n[profiles.a]\nharness='codex'\n[profiles.a.auth]\nmode='account'\naccount_id='a'\nstore='s'\nkey='auth'\n"))
	if err != nil {
		t.Fatal(err)
	}
	s.Profiles = p
	cfg := session.ResolvedConfiguration{Harness: "codex", Credentials: session.Credentials{Mode: "account", AccountID: "a", Store: new(p.CredentialStores["s"]), Key: "auth"}}
	ctx, cancel := context.WithCancel(t.Context())
	c := newAccountLimitsCollector(ctx, s)
	release := make(chan struct{})
	defer func() { close(release); cancel(); c.Wait() }()
	started := make(chan struct{})
	c.save = func(context.Context, string, string, time.Time, *accountlimits.Snapshot, *string) error {
		close(started)
		<-release
		return nil
	}
	d := &limitTestDriver{events: make(chan struct{}, 1)}
	unregister := c.Register(uuid.New(), cfg, d)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("storage write did not start")
	}
	done := make(chan struct{})
	go func() { unregister(); close(done) }()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("unregister waited for storage write")
	}
}

func waitForLimit(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatal("account limits condition was not reached")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestCollectorFailoverAfterDonorErrorAndReconnect(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"unsupported", accountlimits.ErrUnsupported},
		{"authentication unavailable", accountlimits.ErrAuthenticationUnavailable},
		{"invalid response", accountlimits.ErrInvalidResponse},
		{"temporary connection failure", harness.ErrUncertain},
		{"connection timeout", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _, _ := setup(t)
			p, err := config.ReadProfiles(strings.NewReader("[credential_stores.s]\nbucket='b'\n[profiles.a]\nharness='codex'\n[profiles.a.auth]\nmode='account'\naccount_id='a'\nstore='s'\nkey='auth'\n"))
			if err != nil {
				t.Fatal(err)
			}
			s.Profiles = p
			cfg := session.ResolvedConfiguration{Harness: "codex", Credentials: session.Credentials{Mode: "account", AccountID: "a", Store: new(p.CredentialStores["s"]), Key: "auth"}}
			ctx, cancel := context.WithCancel(t.Context())
			c := newAccountLimitsCollector(ctx, s)
			defer func() { cancel(); c.Wait() }()
			var errorWrites atomic.Int64
			save := c.save
			c.save = func(ctx context.Context, id, fingerprint string, at time.Time, snapshot *accountlimits.Snapshot, code *string) error {
				if code != nil {
					errorWrites.Add(1)
				}
				return save(ctx, id, fingerprint, at, snapshot, code)
			}
			primaryID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
			reserveID := uuid.MustParse("00000000-0000-0000-0000-000000000002")
			primary := &limitTestDriver{events: make(chan struct{}, 1), results: []error{nil, tc.err}}
			unregisterPrimary := c.Register(primaryID, cfg, primary)
			defer unregisterPrimary()
			a := c.accounts["a"]
			waitForLimit(t, func() bool {
				response, err := s.AccountLimits(t.Context())
				a.mu.Lock()
				defer a.mu.Unlock()
				return err == nil && len(response.Items) == 1 && response.Items[0].State == "fresh" && a.pending == nil
			})
			reserve := &limitTestDriver{events: make(chan struct{}, 1)}
			unregisterReserve := c.Register(reserveID, cfg, reserve)
			defer unregisterReserve()
			a.mu.Lock()
			a.next = time.Now()
			a.signal()
			a.mu.Unlock()
			waitForLimit(t, func() bool {
				response, err := s.AccountLimits(t.Context())
				a.mu.Lock()
				defer a.mu.Unlock()
				donor := a.donors[primaryID]
				return err == nil && len(response.Items) == 1 && response.Items[0].State == "fresh" && donor != nil && (donor.blocked || donor.retryUntil.After(time.Now())) && a.preferred == reserveID && a.pending == nil && reserve.count() == 1
			})
			if primary.count() != 2 || reserve.count() != 1 || errorWrites.Load() != 0 {
				t.Fatal("failed donor prevented failover", primary.count(), reserve.count())
			}
			a.mu.Lock()
			a.donors[primaryID].retryUntil = time.Now().Add(-time.Second)
			a.next = time.Now()
			a.signal()
			a.mu.Unlock()
			waitForLimit(t, func() bool { return reserve.count() == 2 })
			if primary.count() != 2 || errorWrites.Load() != 0 {
				t.Fatal("recovered lower-ID donor displaced the healthy reserve", primary.count(), errorWrites.Load())
			}
			reconnected := &limitTestDriver{events: make(chan struct{}, 1), started: make(chan struct{}, 1)}
			unregisterReconnected := c.Register(primaryID, cfg, reconnected)
			defer unregisterReconnected()
			unregisterReserve()
			select {
			case <-reconnected.started:
			case <-time.After(time.Second):
				t.Fatal("reconnected primary was not retried")
			}
		})
	}
}

func TestCollectorProviderFailureBacksOffWholeAccount(t *testing.T) {
	for _, failure := range []struct {
		name string
		err  error
	}{
		{"no data", accountlimits.ErrNoData},
		{"provider RPC error", errors.Join(harness.ErrRejected, errors.New("provider failure"))},
	} {
		t.Run(failure.name, func(t *testing.T) {
			s, _, _, _ := setup(t)
			p, err := config.ReadProfiles(strings.NewReader("[credential_stores.s]\nbucket='b'\n[profiles.a]\nharness='codex'\n[profiles.a.auth]\nmode='account'\naccount_id='a'\nstore='s'\nkey='auth'\n"))
			if err != nil {
				t.Fatal(err)
			}
			s.Profiles = p
			cfg := session.ResolvedConfiguration{Harness: "codex", Credentials: session.Credentials{Mode: "account", AccountID: "a", Store: new(p.CredentialStores["s"]), Key: "auth"}}
			ctx, cancel := context.WithCancel(t.Context())
			c := newAccountLimitsCollector(ctx, s)
			defer func() { cancel(); c.Wait() }()
			var writes atomic.Int64
			save := c.save
			c.save = func(ctx context.Context, id, fingerprint string, at time.Time, snapshot *accountlimits.Snapshot, code *string) error {
				writes.Add(1)
				return save(ctx, id, fingerprint, at, snapshot, code)
			}
			primary := &limitTestDriver{results: []error{failure.err}}
			reserve := make([]*limitTestDriver, 0, 49)
			unregister := []func(){c.Register(uuid.MustParse("00000000-0000-0000-0000-000000000001"), cfg, primary)}
			defer func() {
				for _, stop := range unregister {
					stop()
				}
			}()
			a := c.accounts["a"]
			waitForLimit(t, func() bool {
				a.mu.Lock()
				defer a.mu.Unlock()
				return a.pending == nil && a.providerFailures == 1 && a.providerRetryUntil.After(time.Now()) && writes.Load() == 1
			})
			for range 49 {
				donor := &limitTestDriver{}
				reserve = append(reserve, donor)
				unregister = append(unregister, c.Register(uuid.New(), cfg, donor))
			}
			a.mu.Lock()
			a.next = time.Now()
			a.signal()
			a.mu.Unlock()
			time.Sleep(30 * time.Millisecond)
			if primary.count() != 1 || writes.Load() != 1 {
				t.Fatal("provider failure triggered a read from another donor", primary.count(), writes.Load())
			}
			for _, donor := range reserve {
				if donor.count() != 0 {
					t.Fatal("provider failure triggered a reserve read", donor.count())
				}
			}
		})
	}
}

func TestCollectorStopsAfterTwoTransportFailures(t *testing.T) {
	s, _, _, _ := setup(t)
	p, err := config.ReadProfiles(strings.NewReader("[credential_stores.s]\nbucket='b'\n[profiles.a]\nharness='codex'\n[profiles.a.auth]\nmode='account'\naccount_id='a'\nstore='s'\nkey='auth'\n"))
	if err != nil {
		t.Fatal(err)
	}
	s.Profiles = p
	cfg := session.ResolvedConfiguration{Harness: "codex", Credentials: session.Credentials{Mode: "account", AccountID: "a", Store: new(p.CredentialStores["s"]), Key: "auth"}}
	ctx, cancel := context.WithCancel(t.Context())
	c := newAccountLimitsCollector(ctx, s)
	defer func() { cancel(); c.Wait() }()
	var writes atomic.Int64
	save := c.save
	c.save = func(ctx context.Context, id, fingerprint string, at time.Time, snapshot *accountlimits.Snapshot, code *string) error {
		writes.Add(1)
		return save(ctx, id, fingerprint, at, snapshot, code)
	}
	release := make(chan struct{})
	drivers := make([]*limitTestDriver, 50)
	drivers[0] = &limitTestDriver{started: make(chan struct{}, 1), release: release, results: []error{harness.ErrUncertain}}
	drivers[1] = &limitTestDriver{results: []error{context.DeadlineExceeded}}
	unregister := make([]func(), 0, len(drivers))
	defer func() {
		for _, stop := range unregister {
			stop()
		}
	}()
	for i := range drivers {
		if drivers[i] == nil {
			drivers[i] = &limitTestDriver{}
		}
		unregister = append(unregister, c.Register(uuid.UUID{15: byte(i + 1)}, cfg, drivers[i]))
		if i == 0 {
			select {
			case <-drivers[0].started:
			case <-time.After(time.Second):
				t.Fatal("first donor did not start")
			}
		}
	}
	close(release)
	a := c.accounts["a"]
	waitForLimit(t, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.pending == nil && a.providerFailures == 1 && a.providerRetryUntil.After(time.Now()) && writes.Load() == 1
	})
	if drivers[0].count() != 1 || drivers[1].count() != 1 {
		t.Fatal("transport failover did not stop after two donors", drivers[0].count(), drivers[1].count())
	}
	for _, driver := range drivers[2:] {
		if driver.count() != 0 {
			t.Fatal("third donor read before account backoff", driver.count())
		}
	}
	a.mu.Lock()
	a.providerRetryUntil = time.Now().Add(-time.Second)
	a.donors[uuid.UUID{15: 1}].retryUntil = time.Now().Add(-time.Second)
	a.donors[uuid.UUID{15: 2}].retryUntil = time.Now().Add(-time.Second)
	a.next = time.Now()
	a.signal()
	a.mu.Unlock()
	waitForLimit(t, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.preferred == (uuid.UUID{15: 3}) && a.pending == nil
	})
	if drivers[2].count() != 1 || writes.Load() != 2 {
		t.Fatal("account did not continue with the next donor after backoff", drivers[2].count(), writes.Load())
	}
}

func TestCollectorLimitsInvalidDonorChain(t *testing.T) {
	s, _, _, _ := setup(t)
	p, err := config.ReadProfiles(strings.NewReader("[credential_stores.s]\nbucket='b'\n[profiles.a]\nharness='codex'\n[profiles.a.auth]\nmode='account'\naccount_id='a'\nstore='s'\nkey='auth'\n"))
	if err != nil {
		t.Fatal(err)
	}
	s.Profiles = p
	cfg := session.ResolvedConfiguration{Harness: "codex", Credentials: session.Credentials{Mode: "account", AccountID: "a", Store: new(p.CredentialStores["s"]), Key: "auth"}}
	ctx, cancel := context.WithCancel(t.Context())
	c := newAccountLimitsCollector(ctx, s)
	defer func() { cancel(); c.Wait() }()
	var errorWrites atomic.Int64
	save := c.save
	c.save = func(ctx context.Context, id, fingerprint string, at time.Time, snapshot *accountlimits.Snapshot, code *string) error {
		if code != nil {
			errorWrites.Add(1)
		}
		return save(ctx, id, fingerprint, at, snapshot, code)
	}
	release := make(chan struct{})
	first := &limitTestDriver{started: make(chan struct{}, 1), release: release, results: []error{accountlimits.ErrInvalidResponse}}
	unregisterFirst := c.Register(uuid.UUID{15: 1}, cfg, first)
	defer unregisterFirst()
	select {
	case <-first.started:
	case <-time.After(time.Second):
		t.Fatal("first donor did not start")
	}
	second := &limitTestDriver{results: []error{accountlimits.ErrInvalidResponse}}
	unregisterSecond := c.Register(uuid.UUID{15: 2}, cfg, second)
	defer unregisterSecond()
	third := &limitTestDriver{}
	unregisterThird := c.Register(uuid.UUID{15: 3}, cfg, third)
	defer unregisterThird()
	close(release)
	a := c.accounts["a"]
	waitForLimit(t, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.providerFailures == 1 && a.providerRetryUntil.After(time.Now()) && a.pending == nil
	})
	if first.count() != 1 || second.count() != 1 || third.count() != 0 || errorWrites.Load() != 1 {
		t.Fatal("invalid response chain exceeded two donors", first.count(), second.count(), third.count(), errorWrites.Load())
	}
	a.mu.Lock()
	a.providerRetryUntil = time.Now().Add(-time.Second)
	a.donors[uuid.UUID{15: 1}].retryUntil = time.Now().Add(-time.Second)
	a.donors[uuid.UUID{15: 2}].retryUntil = time.Now().Add(-time.Second)
	a.next = time.Now()
	a.signal()
	a.mu.Unlock()
	waitForLimit(t, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.preferred == (uuid.UUID{15: 3}) && a.pending == nil
	})
	if third.count() != 1 || errorWrites.Load() != 1 {
		t.Fatal("account did not try the next donor after backoff", third.count(), errorWrites.Load())
	}
}

func TestCollectorAcceptsReserveReadWhenPrimaryRetryExpires(t *testing.T) {
	s, _, _, _ := setup(t)
	p, err := config.ReadProfiles(strings.NewReader("[credential_stores.s]\nbucket='b'\n[profiles.a]\nharness='codex'\n[profiles.a.auth]\nmode='account'\naccount_id='a'\nstore='s'\nkey='auth'\n"))
	if err != nil {
		t.Fatal(err)
	}
	s.Profiles = p
	cfg := session.ResolvedConfiguration{Harness: "codex", Credentials: session.Credentials{Mode: "account", AccountID: "a", Store: new(p.CredentialStores["s"]), Key: "auth"}}
	ctx, cancel := context.WithCancel(t.Context())
	c := newAccountLimitsCollector(ctx, s)
	defer func() { cancel(); c.Wait() }()
	primaryID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	primary := &limitTestDriver{results: []error{harness.ErrUncertain}}
	unregisterPrimary := c.Register(primaryID, cfg, primary)
	defer unregisterPrimary()
	a := c.accounts["a"]
	waitForLimit(t, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.pending == nil && a.donors[primaryID].retryUntil.After(time.Now())
	})
	a.mu.Lock()
	a.donors[primaryID].retryUntil = time.Now().Add(500 * time.Millisecond)
	a.mu.Unlock()
	release := make(chan struct{})
	reserve := &limitTestDriver{started: make(chan struct{}, 1), release: release}
	unregisterReserve := c.Register(uuid.MustParse("00000000-0000-0000-0000-000000000002"), cfg, reserve)
	defer unregisterReserve()
	select {
	case <-reserve.started:
	case <-time.After(time.Second):
		t.Fatal("reserve read did not start")
	}
	time.Sleep(550 * time.Millisecond)
	close(release)
	waitForLimit(t, func() bool {
		response, err := s.AccountLimits(t.Context())
		return err == nil && len(response.Items) == 1 && response.Items[0].State == "fresh"
	})
	if primary.count() != 1 || reserve.count() != 1 {
		t.Fatal("reserve result was discarded after primary retry expired", primary.count(), reserve.count())
	}
}

func TestCollectorBackoffAndLeadingEdgeDebounce(t *testing.T) {
	donor := &limitDonor{}
	now := time.Now()
	for attempt, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
		donor.observe(limitObservation{code: new("temporarily_unavailable")}, now)
		if got := donor.retryUntil.Sub(now); got != want || donor.failures != attempt+1 {
			t.Fatal("backoff", attempt+1, got, want, donor.failures)
		}
	}
	donor.observe(limitObservation{snapshot: &accountlimits.Snapshot{}}, now)
	if donor.failures != 0 || !donor.retryUntil.IsZero() {
		t.Fatal("success did not reset backoff")
	}
	donor.observe(limitObservation{code: new("unsupported")}, now)
	if !donor.blocked {
		t.Fatal("unsupported donor remained eligible")
	}
	a := &limitAccount{wake: make(chan struct{}, 1)}
	a.markDirty(now)
	a.markDirty(now.Add(4 * time.Second))
	if !a.dirtyAt.Equal(now.Add(5 * time.Second)) {
		t.Fatal("notification flood postponed the read", a.dirtyAt)
	}
}

func TestExecutorRunsWithBlockedAccountLimitRead(t *testing.T) {
	s, _, _, _ := setup(t)
	p, err := config.ReadProfiles(strings.NewReader("[credential_stores.s]\nbucket='b'\n[profiles.a]\nharness='codex'\nmodel='model'\n[profiles.a.auth]\nmode='account'\naccount_id='a'\nstore='s'\nkey='auth'\n"))
	if err != nil {
		t.Fatal(err)
	}
	s.Profiles = p
	accepted, err := s.Accept(t.Context(), store.Admission{Key: uuid.New(), Create: &session.CreateSession{
		Configuration: session.ConfigurationInput{Agent: session.AgentInput{Profile: "a"}, Sandbox: session.SandboxInput{Template: "codex"}, Limits: session.Limits{RunTimeoutSeconds: 3600}},
		Message:       session.TextMessage{Text: "task"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	r := &remote{}
	e := executor(accepted.SessionID, s, r)
	driver := &lifecycleLimitDriver{fakeDriver: &fakeDriver{r: r}, started: make(chan struct{})}
	e.NewDriver = func(harness.Sandbox) harness.Driver { return driver }
	ctx, cancel := context.WithCancel(t.Context())
	collector := newAccountLimitsCollector(ctx, s)
	e.Limits = collector
	defer func() { e.Disconnect(); cancel(); collector.Wait() }()
	tick(t, e)
	select {
	case <-driver.started:
	case <-time.After(time.Second):
		t.Fatal("initialized driver was not registered for limit reads")
	}
	if !driver.initialized.Load() {
		t.Fatal("limit read started before driver initialization")
	}
	before := r.renewals
	e.timeoutRenewAt = time.Time{}
	tick(t, e)
	if r.renewals <= before {
		t.Fatal("blocked limit read delayed sandbox lease renewal")
	}
	if _, err := s.Cancel(t.Context(), accepted.SessionID, accepted.RunID); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	tick(t, e)
	if r.cancels != 1 {
		t.Fatal("blocked limit read delayed cancellation", r.cancels)
	}
}
