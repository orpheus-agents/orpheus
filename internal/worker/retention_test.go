//go:build integration

package worker

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

func retentionFixture(t *testing.T) (*store.Store, *remote, session.Acceptance, *Executor, time.Time) {
	t.Helper()
	s, box, a, e := setup(t)
	tick(t, e)
	complete(box)
	tick(t, e)
	tick(t, e)
	e.Disconnect()
	now := time.Now().UTC().Truncate(time.Microsecond)
	ageRetentionRun(t, s, a.RunID, now.Add(-15*24*time.Hour))
	e.clock = func() time.Time { return now }
	return s, box, a, e, now
}
func ageRetentionRun(t *testing.T, s *store.Store, rid uuid.UUID, at time.Time) {
	t.Helper()
	if _, err := s.Pool.Exec(t.Context(), "UPDATE runs SET finished_at=$2 WHERE id=$1", rid, at); err != nil {
		t.Fatal(err)
	}
}
func retentionCandidates(t *testing.T, s *store.Store, now time.Time) []uuid.UUID {
	t.Helper()
	ids, err := db.New(s.Pool).SandboxCleanupCandidates(t.Context(), db.SandboxCleanupCandidatesParams{NowAt: now, Cutoff: now.Add(-time.Duration(s.Settings.SandboxPausedRetentionDays) * 24 * time.Hour), BatchSize: s.Settings.SandboxCleanupBatchSize})
	if err != nil {
		t.Fatal(err)
	}
	return ids
}
func admitRetentionRun(sid uuid.UUID) store.Admission {
	return store.Admission{SessionID: sid, Key: uuid.New(), Messages: []session.TextMessage{{Text: "continue"}}}
}
func assertRetentionBlocked(t *testing.T, s *store.Store, sid uuid.UUID) {
	t.Helper()
	_, err := s.Accept(t.Context(), admitRetentionRun(sid))
	problem, ok := errors.AsType[*session.APIError](err)
	if !ok || problem.Status != 409 || problem.Problem.Code != "session_unavailable" {
		t.Fatal(err)
	}
}

func TestRetentionPreservesHistoryAndNeverResumes(t *testing.T) {
	s, box, a, e, now := retentionFixture(t)
	before, err := s.Run(t.Context(), a.SessionID, a.RunID)
	if err != nil {
		t.Fatal(err)
	}
	ids := retentionCandidates(t, s, now)
	if len(ids) != 1 || ids[0] != a.SessionID {
		t.Fatal(ids)
	}
	resumes, pauses := box.resumes, box.pauses
	box.deleteHook = func() { assertRetentionBlocked(t, s, a.SessionID) }
	result, err := e.cleanupRetainedSandbox(t.Context(), e.now().Add(sandboxCleanupInterval))
	if err != nil || result != "deleted" {
		t.Fatal(result, err)
	}
	after, err := s.Run(t.Context(), a.SessionID, a.RunID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("run history changed", before, after, err)
	}
	r, active, err := s.Read(t.Context(), a.SessionID)
	if err != nil || active != nil || r.SlotReserved || r.SandboxState != "deleted" || r.SandboxID == nil || r.SandboxError == nil || r.SandboxError.Code != "sandbox_expired" {
		t.Fatal(r, err)
	}
	if box.deletes != 1 || box.resumes != resumes || box.pauses != pauses {
		t.Fatal("cleanup resumed or paused", box)
	}
	assertRetentionBlocked(t, s, a.SessionID)
	if ids := retentionCandidates(t, s, now.Add(48*time.Hour)); len(ids) != 0 {
		t.Fatal(ids)
	}
}

func TestRetentionEligibilityAndFreshRunRecheck(t *testing.T) {
	for _, kind := range []string{"boundary", "fresh", "single", "reserved", "missing_id", "missing_finished", "active", "new_cancelled_run", "unknown_create"} {
		t.Run(kind, func(t *testing.T) {
			s, box, a, e, now := retentionFixture(t)
			switch kind {
			case "boundary":
				ageRetentionRun(t, s, a.RunID, now.Add(-14*24*time.Hour))
			case "fresh":
				ageRetentionRun(t, s, a.RunID, now.Add(-time.Hour))
			case "single", "reserved", "missing_id":
				column := map[string]string{"single": "allow_multiple_runs=false", "reserved": "slot_reserved=true", "missing_id": "sandbox_id=NULL"}[kind]
				if _, err := s.Pool.Exec(t.Context(), "UPDATE sessions SET "+column+" WHERE id=$1", a.SessionID); err != nil {
					t.Fatal(err)
				}
			case "missing_finished":
				if _, err := s.Pool.Exec(t.Context(), "UPDATE runs SET finished_at=NULL WHERE id=$1", a.RunID); err != nil {
					t.Fatal(err)
				}
			case "active", "new_cancelled_run":
				accepted, err := s.Accept(t.Context(), admitRetentionRun(a.SessionID))
				if err != nil {
					t.Fatal(err)
				}
				if kind == "new_cancelled_run" {
					if _, err := s.Cancel(t.Context(), a.SessionID, accepted.RunID); err != nil {
						t.Fatal(err)
					}
				}
			case "unknown_create":
				if _, err := s.Pool.Exec(t.Context(), `INSERT INTO operations(id,session_id,kind,status) VALUES($1,$2,'sandbox_create','uncertain')`, uuid.New(), a.SessionID); err != nil {
					t.Fatal(err)
				}
			}
			if ids := retentionCandidates(t, s, now); len(ids) != 0 {
				t.Fatal(ids)
			}
			result, err := e.cleanupRetainedSandbox(t.Context(), e.now().Add(sandboxCleanupInterval)) // Includes stale candidate selections.
			if err != nil || result != "skipped" || box.deletes != 0 {
				t.Fatal(result, err, box.deletes)
			}
		})
	}
}

func TestRetentionRetryAfterRestartAndLostResponse(t *testing.T) {
	for _, kind := range []string{"failed", "lost_response", "already_absent", "crash_before_delete", "crash_after_delete"} {
		t.Run(kind, func(t *testing.T) {
			s, box, a, e, now := retentionFixture(t)
			wantDeletes := 2
			switch kind {
			case "failed":
				box.deleteError = errors.New("provider offline")
			case "lost_response":
				box.lostDelete = true
			case "already_absent":
				box.deleteError = harness.ErrNotFound
				wantDeletes = 1
			case "crash_before_delete", "crash_after_delete":
				op, err := e.claimRetention(t.Context(), e.now().Add(sandboxCleanupInterval))
				if err != nil || op == nil {
					t.Fatal(op, err)
				}
				if kind == "crash_after_delete" {
					if err := box.Delete(t.Context(), "sandbox"); err != nil {
						t.Fatal(err)
					}
				} else {
					wantDeletes = 1
				}
			}
			if kind != "crash_before_delete" && kind != "crash_after_delete" {
				result, err := e.cleanupRetainedSandbox(t.Context(), e.now().Add(sandboxCleanupInterval))
				if kind == "already_absent" {
					if err != nil || result != "deleted" {
						t.Fatal(result, err)
					}
					if box.deletes != wantDeletes {
						t.Fatal(box.deletes)
					}
					return
				}
				if err == nil || result != "failed" {
					t.Fatal(result, err)
				}
			}
			assertRetentionBlocked(t, s, a.SessionID)
			if ids := retentionCandidates(t, s, now.Add(time.Hour)); len(ids) != 0 {
				t.Fatal("retried too early", ids)
			}
			box.deleteError = nil
			restarted := NewExecutor(a.SessionID, s, box)
			restarted.clock = func() time.Time { return now.Add(sandboxCleanupInterval) }
			s.Settings.SandboxPausedRetentionDays = 100 // An accepted deletion survives configuration changes.
			if ids := retentionCandidates(t, s, restarted.now()); len(ids) != 1 {
				t.Fatal(ids)
			}
			result, err := restarted.cleanupRetainedSandbox(t.Context(), restarted.now().Add(sandboxCleanupInterval))
			if err != nil || result != "deleted" || box.deletes != wantDeletes {
				t.Fatal(result, err, box.deletes)
			}
		})
	}
}

// A queued item can start later than its pass. It must still be eligible in the next pass.
func TestRetentionDelayedClaimRetriesInNextPass(t *testing.T) {
	s, box, a, e, started := retentionFixture(t)
	e.clock = func() time.Time { return started.Add(time.Minute) }
	retryAt := started.Add(sandboxCleanupInterval)
	box.deleteError = errors.New("offline")
	if _, err := e.cleanupRetainedSandbox(t.Context(), retryAt); err == nil {
		t.Fatal("expected failure")
	}
	if got := retentionCandidates(t, s, retryAt.Add(-time.Microsecond)); len(got) != 0 {
		t.Fatal(got)
	}
	if got := retentionCandidates(t, s, retryAt); !slices.Equal(got, []uuid.UUID{a.SessionID}) {
		t.Fatal(got)
	}
}

func TestRetentionLostSandboxOnNextRun(t *testing.T) {
	s, box, a, e, _ := retentionFixture(t)
	request := admitRetentionRun(a.SessionID)
	accepted, err := s.Accept(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	box.state = "lost"
	tick(t, e)
	got, err := s.Run(t.Context(), a.SessionID, accepted.RunID)
	if err != nil || got.Status != session.Failed || got.Error == nil || got.Error.Code != "sandbox_lost" {
		t.Fatal(got, err)
	}
	replay, err := s.Accept(t.Context(), request)
	if err != nil || replay != accepted {
		t.Fatal("acceptance not replayed", replay, err)
	}
	assertRetentionBlocked(t, s, a.SessionID)
	if box.creates != 1 {
		t.Fatal("created replacement", box.creates)
	}
}

func seedRetentionSession(t *testing.T, s *store.Store, at time.Time) uuid.UUID {
	t.Helper()
	a, err := s.Accept(t.Context(), store.Admission{Key: uuid.New(), Create: &session.CreateSession{AllowMultipleRuns: true, Configuration: session.ConfigurationInput{Agent: session.AgentInput{Profile: "p"}, Sandbox: session.SandboxInput{Template: "codex"}, Limits: session.Limits{RunTimeoutSeconds: 3600}}, Messages: []session.TextMessage{{Text: "task"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Mutate(t.Context(), a.SessionID, true, func(tx pgx.Tx, r *store.SessionRecord) error {
		run, err := store.GetRun(t.Context(), tx, a.SessionID, a.RunID)
		if err != nil {
			return err
		}
		if err := store.Finish(t.Context(), tx, r, &run, session.Completed, nil, nil); err != nil {
			return err
		}
		r.SandboxState, r.SandboxID, r.SlotReserved = "paused", new(a.SessionID.String()), false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ageRetentionRun(t, s, a.RunID, at)
	return a.SessionID
}

type blockingRetentionPlatform struct {
	harness.Platform
	entered chan string
	release chan struct{}
	calls   atomic.Int32
}

func (p *blockingRetentionPlatform) Delete(ctx context.Context, id string) error {
	p.calls.Add(1)
	p.entered <- id
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.release:
		return nil
	}
}

func TestRetentionBatchConcurrencyAndCapacity(t *testing.T) {
	s, _, _, _, now := retentionFixture(t)
	for i := range 6 {
		seedRetentionSession(t, s, now.Add(-16*24*time.Hour-time.Duration(i)*time.Hour))
	}
	s.Settings.SandboxCleanupBatchSize = 5
	p := &blockingRetentionPlatform{entered: make(chan string, 10), release: make(chan struct{})}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	tasks := map[uuid.UUID]workerTask{}
	var pass sandboxCleanupPass
	advance := func() {
		t.Helper()
		if err := pass.advance(ctx, s, p, tasks, &wg, now); err != nil {
			t.Fatal(err)
		}
	}
	advance()
	for range 3 {
		select {
		case <-p.entered:
		case <-ctx.Done():
			t.Fatal("three calls did not start")
		}
	}
	advance()
	if p.calls.Load() != 3 || len(tasks) != 3 {
		t.Fatal("concurrency exceeded", p.calls.Load(), len(tasks))
	}
	reserved, err := s.Reserved(ctx)
	if err != nil || len(reserved) != 0 {
		t.Fatal("cleanup reserves run capacity", reserved, err)
	}
	close(p.release)
	for pass.running {
		for id, task := range tasks {
			select {
			case <-task.done:
				pass.finished(*task.cleanupResult)
				delete(tasks, id)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		advance()
	}
	if p.calls.Load() != 5 || pass.deleted != 5 || pass.selected != 5 {
		t.Fatal(p.calls.Load(), pass)
	}
	advance()
	if p.calls.Load() != 5 {
		t.Fatal("drained next batch immediately")
	}
	if ids := retentionCandidates(t, s, now); len(ids) != 2 {
		t.Fatal(ids)
	}
}

func TestRetentionFairOrderingAndBatchLimit(t *testing.T) {
	s, box, old, e, now := retentionFixture(t)
	newer := seedRetentionSession(t, s, now.Add(-14*24*time.Hour-time.Hour))
	s.Settings.SandboxCleanupBatchSize = 1
	if got := retentionCandidates(t, s, now); !slices.Equal(got, []uuid.UUID{old.SessionID}) {
		t.Fatal(got)
	}
	box.deleteError = errors.New("offline")
	if _, err := e.cleanupRetainedSandbox(t.Context(), e.now().Add(sandboxCleanupInterval)); err == nil {
		t.Fatal("expected failure")
	}
	if got := retentionCandidates(t, s, now.Add(sandboxCleanupInterval)); !slices.Equal(got, []uuid.UUID{newer}) {
		t.Fatal("failed deletion starves unattempted sessions", got)
	}
}

func TestRetentionTimeoutAndOwnershipLoss(t *testing.T) {
	for _, ownerLoss := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "ownership_loss"}[ownerLoss], func(t *testing.T) {
			s, _, a, e, _ := retentionFixture(t)
			p := &blockingRetentionPlatform{entered: make(chan string, 1), release: make(chan struct{})}
			if !ownerLoss {
				s.Settings.RPCTimeout = 10 * time.Millisecond
				e.Platform = p
				result, err := e.cleanupRetainedSandbox(t.Context(), e.now().Add(sandboxCleanupInterval))
				if result != "failed" || !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(result, err)
				}
			} else {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- run(ctx, s, p, func(context.Context, *db.Queries) {}) }()
				select {
				case <-p.entered:
				case <-time.After(3 * time.Second):
					t.Fatal("retention did not start")
				}
				var killed bool
				if err := s.Pool.QueryRow(t.Context(), `SELECT pg_terminate_backend(pid) FROM pg_locks WHERE locktype='advisory' AND classid=($1::bigint >> 32)::oid AND objid=($1::bigint & 4294967295)::oid AND granted`, store.WorkerLock).Scan(&killed); err != nil || !killed {
					t.Fatal(killed, err)
				}
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("ownership loss not reported")
					}
				case <-time.After(3 * time.Second):
					t.Fatal("cleanup outlived worker")
				}
			}
			r, _, err := s.Read(t.Context(), a.SessionID)
			if err != nil || r.SandboxState != "deleting" || r.SlotReserved {
				t.Fatal(r, err)
			}
			op, err := db.New(s.Pool).SandboxRetentionOperation(t.Context(), a.SessionID)
			if err != nil || op.NextCheckAt == nil || op.Status == "confirmed" {
				t.Fatal(op, err)
			}
		})
	}
}

func TestRetentionDefaultBatchWithLargeBacklog(t *testing.T) {
	s, _, _, _, now := retentionFixture(t)
	if _, err := s.Pool.Exec(t.Context(), `WITH seeded AS (
 INSERT INTO sessions(id,configuration,allow_multiple_runs,sandbox_state,sandbox_id,slot_reserved)
 SELECT gen_random_uuid(),'{}',true,'paused',i::text,false FROM generate_series(1,1000) i RETURNING id
 ) INSERT INTO runs(id,session_id,number,status,finished_at)
 SELECT gen_random_uuid(),id,1,'completed',$1 FROM seeded`, now.Add(-16*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := retentionCandidates(t, s, now); len(got) != 1000 {
		t.Fatal(len(got))
	}
	s.Settings.SandboxCleanupBatchSize = 1001
	if got := retentionCandidates(t, s, now); len(got) != 1001 {
		t.Fatal(len(got))
	}
}

// New runs must start even while all cleanup workers wait on the provider.
type retentionAdmissionPlatform struct {
	*blockingRetentionPlatform
	created chan struct{}
}

func (p *retentionAdmissionPlatform) Create(ctx context.Context, _ string, _ time.Duration, _ map[string]string) (harness.Sandbox, error) {
	close(p.created)
	<-ctx.Done()
	return nil, ctx.Err()
}
func TestRetentionDoesNotBlockOrdinaryRun(t *testing.T) {
	s, _, _, _, now := retentionFixture(t)
	for range 2 {
		seedRetentionSession(t, s, now.Add(-15*24*time.Hour))
	}
	p := &retentionAdmissionPlatform{blockingRetentionPlatform: &blockingRetentionPlatform{entered: make(chan string, 3), release: make(chan struct{})}, created: make(chan struct{})}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	done := make(chan error, 1)
	go func() { done <- run(ctx, s, p, func(context.Context, *db.Queries) {}) }()
	defer func() { cancel(); <-done }()
	for range 3 {
		select {
		case <-p.entered:
		case <-ctx.Done():
			t.Fatal("cleanup did not start")
		}
	}
	if _, err := s.Accept(ctx, store.Admission{Key: uuid.New(), Create: &session.CreateSession{AllowMultipleRuns: true, Configuration: session.ConfigurationInput{Agent: session.AgentInput{Profile: "p"}, Sandbox: session.SandboxInput{Template: "codex"}, Limits: session.Limits{RunTimeoutSeconds: 3600}}, Messages: []session.TextMessage{{Text: "task"}}}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.created:
	case <-ctx.Done():
		t.Fatal("cleanup blocked an ordinary run")
	}
}
