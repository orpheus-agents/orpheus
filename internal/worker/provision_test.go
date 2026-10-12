//go:build integration

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/migrate"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

type provisionRemote struct {
	*remote
	findCalls, infoCalls                            int
	found                                           []string
	findError, infoError, connectError              error
	afterCreate, afterFind, afterInfo, afterConnect func()
	seen                                            []map[string]string
}

func (p *provisionRemote) Create(ctx context.Context, template string, timeout time.Duration, meta map[string]string) (harness.Sandbox, error) {
	p.seen = append(p.seen, maps.Clone(meta))
	box, err := p.remote.Create(ctx, template, timeout, meta)
	if p.afterCreate != nil {
		p.afterCreate()
	}
	return box, err
}
func (p *provisionRemote) Find(_ context.Context, meta map[string]string) ([]string, error) {
	p.findCalls++
	p.seen = append(p.seen, maps.Clone(meta))
	if p.afterFind != nil {
		p.afterFind()
	}
	return p.found, p.findError
}
func (p *provisionRemote) Info(ctx context.Context, id string) (string, error) {
	p.infoCalls++
	if p.afterInfo != nil {
		p.afterInfo()
	}
	if p.infoError != nil {
		return "", p.infoError
	}
	return p.remote.Info(ctx, id)
}
func (p *provisionRemote) Connect(ctx context.Context, id string, timeout time.Duration) (harness.Sandbox, error) {
	if p.afterConnect != nil {
		p.afterConnect()
	}
	if p.connectError != nil {
		return nil, p.connectError
	}
	return p.remote.Connect(ctx, id, timeout)
}
func provisionOperation(t *testing.T, s *store.Store, a session.Acceptance) store.Operation {
	t.Helper()
	o, err := db.New(s.Pool).CurrentSandboxCreate(t.Context(), db.CurrentSandboxCreateParams{SessionID: a.SessionID, ID: a.RunID})
	if err != nil {
		t.Fatal(err)
	}
	return o
}
func assertProvisionTerminal(t *testing.T, s *store.Store, a session.Acceptance, status session.Status, reserved bool) {
	t.Helper()
	r, err := s.Run(t.Context(), a.SessionID, a.RunID)
	if err != nil {
		t.Fatal(err)
	}
	record, active, err := s.Read(t.Context(), a.SessionID)
	if err != nil || active != nil || r.Status != status || record.SlotReserved != reserved || r.ExecutionStartedAt != nil {
		t.Fatalf("run=%+v active=%v reserved=%v error=%v", r, active, record.SlotReserved, err)
	}
	if status == session.Failed && (r.Error == nil || r.Error.Code != "sandbox_provision_timeout") {
		t.Fatalf("error=%+v", r.Error)
	}
	if status == session.Cancelled && (r.StopReason == nil || *r.StopReason != "user_request" || r.StopMethod != nil) {
		t.Fatalf("stop=%+v", r)
	}
	if reserved && record.SandboxState != "unavailable" {
		t.Fatal(record.SandboxState)
	}
}

func TestProvisionDeadlineAndCancellationSurviveRestart(t *testing.T) {
	for _, cancelRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "cancel"}[cancelRun], func(t *testing.T) {
			s, r, a, e := setup(t)
			r.createError = errors.New("503 secret=never-persist")
			p := &provisionRemote{remote: r}
			e.Platform = p
			now := time.Now().UTC()
			e.clock = func() time.Time { return now }
			tick(t, e)
			original := provisionOperation(t, s, a)
			if original.DeadlineAt == nil || original.FirstError == nil || original.RunID == nil || *original.RunID != a.RunID {
				t.Fatalf("operation=%+v", original)
			}
			var diagnostic map[string]any
			if err := json.Unmarshal(original.FirstError, &diagnostic); err != nil || len(diagnostic) != 1 || diagnostic["code"] != "platform_error" || strings.Contains(string(original.FirstError), "never-persist") {
				t.Fatal("unsafe diagnostic", string(original.FirstError), err)
			}
			// No requests or events between scheduled checks, including a fresh executor.
			record, _, _ := s.Read(t.Context(), a.SessionID)
			e.Disconnect()
			e = executor(a.SessionID, s, r)
			e.Platform = p
			e.clock = func() time.Time { return now }
			defer e.Disconnect()
			for range 4 {
				tick(t, e)
			}
			next, _, _ := s.Read(t.Context(), a.SessionID)
			if p.findCalls != 0 || next.NextEventSequence != record.NextEventSequence {
				t.Fatal("idle tick did work")
			}
			now = now.Add(10 * time.Second)
			tick(t, e)
			if p.findCalls != 1 {
				t.Fatal(p.findCalls)
			}
			afterFind := provisionOperation(t, s, a)
			if afterFind.CheckAttempts != 1 || !afterFind.DeadlineAt.Equal(*original.DeadlineAt) || string(afterFind.FirstError) != string(original.FirstError) {
				t.Fatal("lost deadline/first error/backoff")
			}
			want := session.Failed
			if cancelRun {
				if _, err := s.Cancel(t.Context(), a.SessionID, a.RunID); err != nil {
					t.Fatal(err)
				}
				want = session.Cancelled
			}
			// Cancellation wins even when the provisioning deadline has also expired.
			now = now.Add(2 * time.Minute)
			p.findError = errors.New("platform offline")
			tick(t, e)
			assertProvisionTerminal(t, s, a, want, true)
			if p.findCalls != 1 || r.creates != 1 || r.launches != 0 || r.starts != 0 || r.resumes != 0 {
				t.Fatal("terminal transition performed external work")
			}
			o := provisionOperation(t, s, a)
			if o.AbandonedAt == nil {
				t.Fatal("cleanup obligation lost")
			}
			e.Disconnect()
			e = executor(a.SessionID, s, r)
			e.Platform = p
			e.clock = func() time.Time { return now }
			defer e.Disconnect()
			tick(t, e)
			assertProvisionTerminal(t, s, a, want, true)
			if r.creates != 1 || !maps.Equal(p.seen[0], p.seen[len(p.seen)-1]) {
				t.Fatal("recreated or changed metadata")
			}
		})
	}
}

func TestLateProvisionResponsesCannotStartWork(t *testing.T) {
	for _, stage := range []string{"create", "find", "info", "connect"} {
		for _, stop := range []string{"cancel", "timeout"} {
			t.Run(stage+"/"+stop, func(t *testing.T) {
				s, r, a, e := setup(t)
				p := &provisionRemote{remote: r, found: []string{"sandbox"}}
				e.Platform = p
				now := time.Now().UTC()
				e.clock = func() time.Time { return now }
				halt := func() {
					if stop == "cancel" {
						if _, err := s.Cancel(t.Context(), a.SessionID, a.RunID); err != nil {
							t.Fatal(err)
						}
					} else {
						now = now.Add(3 * time.Minute)
					}
				}
				switch stage {
				case "create":
					p.afterCreate = halt
				case "find":
					r.lostCreate = true
					p.afterFind = halt
				case "info":
					r.lostCreate = true
					p.afterInfo = halt
				case "connect":
					r.lostCreate = true
					p.afterConnect = halt
				}
				tick(t, e)
				if stage != "create" {
					now = now.Add(10 * time.Second)
					tick(t, e)
				}
				want := session.Cancelled
				if stop == "timeout" {
					want = session.Failed
				}
				assertProvisionTerminal(t, s, a, want, true)
				if r.launches != 0 || r.starts != 0 || r.opens != 0 {
					t.Fatal("late result launched work")
				}
				o := provisionOperation(t, s, a)
				result, _ := decodeResult(o)
				if result.SandboxID != "sandbox" || result.Adopted {
					t.Fatal("lost late result", result)
				}
				// Cleanup deletes directly, even for reusable sessions and paused resources.
				p.afterInfo, p.afterFind, p.afterConnect = nil, nil, nil
				r.state = "paused"
				info, resumes := p.infoCalls, r.resumes
				tick(t, e)
				if r.deletes != 1 || p.infoCalls != info || r.resumes != resumes || r.pauses != 0 || r.renewals != 0 {
					t.Fatal("cleanup woke sandbox")
				}
				assertProvisionTerminal(t, s, a, want, false)
			})
		}
	}
}

func TestProvisionConflictAndLostDelete(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "lost_delete", true: "multiple_matches"}[conflict], func(t *testing.T) {
			s, r, a, e := setup(t)
			r.lostCreate = true
			p := &provisionRemote{remote: r, found: []string{"sandbox"}}
			e.Platform = p
			now := time.Now().UTC()
			e.clock = func() time.Time { return now }
			tick(t, e)
			if conflict {
				p.found = append(p.found, "other")
			} else {
				if _, err := s.Cancel(t.Context(), a.SessionID, a.RunID); err != nil {
					t.Fatal(err)
				}
			}
			now = now.Add(10 * time.Second)
			tick(t, e)
			view, err := s.Run(t.Context(), a.SessionID, a.RunID)
			if err != nil || !view.Status.Terminal() {
				t.Fatal(view, err)
			}
			if conflict {
				if view.Error == nil || view.Error.Code != "sandbox_provision_conflict" {
					t.Fatal(view.Error)
				}
				tick(t, e)
				if r.resumes != 0 || r.deletes != 0 || r.starts != 0 {
					t.Fatal("ambiguous resource used")
				}
				return
			}
			r.lostDelete = true
			tick(t, e)
			record, _, _ := s.Read(t.Context(), a.SessionID)
			if !record.SlotReserved || r.deletes != 1 {
				t.Fatal("lost delete released reservation")
			}
			e.Disconnect()
			e = executor(a.SessionID, s, r)
			e.Platform = p
			e.clock = func() time.Time { return now }
			defer e.Disconnect()
			tick(t, e)
			if r.deletes != 1 {
				t.Fatal("restart ignored backoff")
			}
			now = now.Add(time.Minute)
			tick(t, e)
			record, _, _ = s.Read(t.Context(), a.SessionID)
			if record.SlotReserved || r.deletes != 2 || r.resumes != 0 || record.SandboxState != "unavailable" || record.SandboxError != nil {
				t.Fatalf("cleanup: %+v delete=%d", record, r.deletes)
			}
		})
	}
}

func TestProvisionUpgradeOldUnknownCreation(t *testing.T) {
	for _, scenario := range []string{"cancelling", "starting", "terminal", "lost_reservation"} {
		t.Run(scenario, func(t *testing.T) {
			s, r, a, e := setup(t)
			sqlDB := stdlib.OpenDB(*s.Pool.Config().ConnConfig)
			defer func() { _ = sqlDB.Close() }()
			provider, err := migrate.Provider(sqlDB, "../../migrations")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := provider.DownTo(t.Context(), 22); err != nil {
				t.Fatal(err)
			}
			status := "cancelling"
			cancelRequested := true
			reserved := true
			if scenario == "starting" {
				status = "starting"
				cancelRequested = false
			}
			if scenario == "terminal" {
				status = "cancelled"
			}
			if scenario == "lost_reservation" {
				reserved = false
			}
			if _, err := s.Pool.Exec(t.Context(), `UPDATE runs SET created_at=now()-interval '1 hour', status=$2, phase='preparation', cancel_requested_at=CASE WHEN $3 THEN now() ELSE NULL END, stop_reason=CASE WHEN $3 THEN 'user_request' ELSE NULL END, finished_at=CASE WHEN $2='cancelled' THEN now() ELSE NULL END WHERE id=$1`, a.RunID, status, cancelRequested); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Pool.Exec(t.Context(), `UPDATE sessions SET sandbox_state='provisioning',slot_reserved=$2 WHERE id=$1`, a.SessionID, reserved); err != nil {
				t.Fatal(err)
			}
			oid := uuid.New()
			if _, err := s.Pool.Exec(t.Context(), `INSERT INTO operations(id,session_id,kind,status,created_at,attempted_at) VALUES($1,$2,'sandbox_create','uncertain',now()-interval '59 minutes',now()-interval '59 minutes')`, oid, a.SessionID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Pool.Exec(t.Context(), `INSERT INTO hook_executions(id,session_id,run_id,name,status) VALUES($1,$2,$3,'before_run','pending')`, uuid.New(), a.SessionID, a.RunID); err != nil {
				t.Fatal(err)
			}
			if _, err := provider.Up(t.Context()); err != nil {
				t.Fatal(err)
			}
			p := &provisionRemote{remote: r, findError: errors.New("offline")}
			e.Platform = p
			tick(t, e)
			want := session.Cancelled
			if scenario == "starting" {
				want = session.Failed
			}
			assertProvisionTerminal(t, s, a, want, true)
			o := provisionOperation(t, s, a)
			if o.ID != oid || o.RunID != nil || o.AbandonedAt == nil || o.DeadlineAt == nil || o.DeadlineAt.After(time.Now()) {
				t.Fatal("legacy attempt changed", o)
			}
			if p.findCalls != 0 || r.creates != 0 {
				t.Fatal("upgrade cancellation required platform")
			}
			if scenario != "terminal" {
				events, err := s.Events(t.Context(), a.SessionID, "0", 100)
				if err != nil {
					t.Fatal(err)
				}
				var terminalEvent, sandboxEvent bool
				for _, event := range events.Items {
					if event.Type == "run.updated" {
						var v session.Run
						if err := json.Unmarshal(event.Data, &v); err != nil {
							t.Fatal(err)
						}
						terminalEvent = terminalEvent || v.Status == want
					}
					if event.Type == "sandbox.updated" {
						var v session.SandboxState
						if err := json.Unmarshal(event.Data, &v); err != nil {
							t.Fatal(err)
						}
						sandboxEvent = sandboxEvent || v.State == "unavailable"
					}
				}
				if !terminalEvent || !sandboxEvent {
					t.Fatal("upgrade did not publish terminal projections", events)
				}
				h, err := s.Hook(t.Context(), a.RunID, "before_run")
				if err != nil || h.Status != "skipped" {
					t.Fatal(h, err)
				}
			}
			e.Disconnect()
			e = executor(a.SessionID, s, r)
			e.Platform = p
			defer e.Disconnect()
			tick(t, e)
			assertProvisionTerminal(t, s, a, want, true)
		})
	}
}

func TestProvisionPendingCancelAndRejectedRetry(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "rejected_new_run", true: "cancel_pending"}[pending], func(t *testing.T) {
			s, r, a, e := setup(t)
			if pending {
				if _, err := s.Pool.Exec(t.Context(), `UPDATE runs SET status='starting',phase='preparation' WHERE id=$1`, a.RunID); err != nil {
					t.Fatal(err)
				}
				if _, err := e.provision(t.Context(), a.RunID, true, nil); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Cancel(t.Context(), a.SessionID, a.RunID); err != nil {
					t.Fatal(err)
				}
				tick(t, e)
				assertProvisionTerminal(t, s, a, session.Cancelled, false)
				o := provisionOperation(t, s, a)
				if o.Status != "failed" || o.AttemptedAt != nil || o.AbandonedAt != nil || r.creates != 0 {
					t.Fatal("unsent create became cleanup", o)
				}
				return
			}
			r.createError = harness.ErrEnvironmentRejected
			tick(t, e)
			o := provisionOperation(t, s, a)
			tick(t, e)
			if r.creates != 1 || o.Status != "failed" {
				t.Fatal("retried rejected attempt")
			}
			r.createError = nil
			next, err := s.Accept(t.Context(), store.Admission{SessionID: a.SessionID, Key: uuid.New(), Messages: []session.TextMessage{{Text: "try again"}}})
			if err != nil {
				t.Fatal(err)
			}
			tick(t, e)
			view, err := s.Run(t.Context(), a.SessionID, next.RunID)
			if err != nil || view.Status != session.Running || r.creates != 2 {
				t.Fatal("new run inherited rejected creation", view, err)
			}
		})
	}
}

func TestProvisionDatabaseFailureRecoversSameCreate(t *testing.T) {
	s, r, a, e := setup(t)
	p := &provisionRemote{remote: r, found: []string{"sandbox"}}
	e.Platform = p
	if _, err := s.Pool.Exec(t.Context(), `CREATE FUNCTION reject_create_result() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.kind='sandbox_create' AND NEW.status='confirmed' THEN RAISE EXCEPTION 'fixture'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_create_result BEFORE UPDATE ON operations FOR EACH ROW EXECUTE FUNCTION reject_create_result()`); err != nil {
		t.Fatal(err)
	}
	if err := e.Tick(t.Context()); err == nil {
		t.Fatal("database failure ignored")
	}
	if r.creates != 1 || r.starts != 0 || r.launches != 0 || e.sandbox != nil {
		t.Fatal("work continued without journal")
	}
	o := provisionOperation(t, s, a)
	if o.Status != "sending" {
		t.Fatal(o.Status)
	}
	if _, err := s.Pool.Exec(t.Context(), `DROP TRIGGER reject_create_result ON operations; DROP FUNCTION reject_create_result()`); err != nil {
		t.Fatal(err)
	}
	e.Disconnect()
	e = executor(a.SessionID, s, r)
	e.Platform = p
	defer e.Disconnect()
	tick(t, e)
	recovered := provisionOperation(t, s, a)
	if r.creates != 1 || r.starts != 1 || recovered.ID != o.ID || !recovered.DeadlineAt.Equal(*o.DeadlineAt) {
		t.Fatal("failed commit duplicated create")
	}
}

func TestProvisionConnectBackoffAndAdoption(t *testing.T) {
	s, r, a, e := setup(t)
	r.lostCreate = true
	p := &provisionRemote{remote: r, found: []string{"sandbox"}, connectError: errors.New("offline")}
	e.Platform = p
	now := time.Now().UTC()
	e.clock = func() time.Time { return now }
	tick(t, e)
	now = now.Add(10 * time.Second)
	tick(t, e)
	before := p.infoCalls
	e.Disconnect()
	e = executor(a.SessionID, s, r)
	e.Platform = p
	e.clock = func() time.Time { return now }
	defer e.Disconnect()
	tick(t, e)
	if p.infoCalls != before {
		t.Fatal("connection backoff lost")
	}
	p.connectError = nil
	now = now.Add(time.Minute)
	tick(t, e)
	o := provisionOperation(t, s, a)
	result, _ := decodeResult(o)
	if !result.Adopted || r.starts != 1 {
		t.Fatal("not adopted", result)
	}
	now = now.Add(time.Hour)
	e.Disconnect()
	tick(t, e)
	view, err := s.Run(t.Context(), a.SessionID, a.RunID)
	if err != nil || view.Status != session.Running || r.creates != 1 {
		t.Fatal("provision deadline applied to running sandbox", view, err)
	}
}

func TestCancelBeforeLaunchIntentPreventsDispatch(t *testing.T) {
	s, r, a, e := setup(t)
	// A preparation operation obtained before cancellation must still recheck the
	// persisted run when committing its sending intent.
	o, err := e.operation(t.Context(), "launch", &a.RunID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cancel(t.Context(), a.SessionID, a.RunID); err != nil {
		t.Fatal(err)
	}
	called := false
	_, err = e.invoke(t.Context(), o, func() (operationResult, error) { called = true; return operationResult{}, nil })
	if !errors.Is(err, errProvisionWait) || called || r.launches != 0 {
		t.Fatal("cancelled launch dispatched", err)
	}
	saved, err := s.GetOperation(t.Context(), o.ID)
	if err != nil || saved.Status != "pending" || saved.AttemptedAt != nil {
		t.Fatal(saved, err)
	}
}

func TestProvisionRecoversOldWorkerWritesAfterMigration(t *testing.T) {
	for _, scenario := range []string{"released_unknown", "late_create", "adopted_paused", "adopted_starting", "adopted_resuming"} {
		t.Run(scenario, func(t *testing.T) {
			s, r, a, e := setup(t)
			adopted := strings.HasPrefix(scenario, "adopted_")
			terminal := scenario == "released_unknown" || scenario == "adopted_paused"
			state := "provisioning"
			opStatus := "uncertain"
			runStatus := "starting"
			var result, boxID any
			if terminal {
				runStatus = "cancelled"
			}
			if adopted {
				state = "ready"
				opStatus = "confirmed"
				boxID = "sandbox"
				result = `{"sandbox_id":"sandbox"}`
			}
			if scenario == "adopted_paused" {
				state = "paused"
				r.state = "paused"
			}
			if scenario == "adopted_resuming" {
				state = "resuming"
				if _, err := s.Pool.Exec(t.Context(), `UPDATE sessions SET sandbox_last_known_state='paused' WHERE id=$1`, a.SessionID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.Pool.Exec(t.Context(), `UPDATE runs SET created_at=now()-interval '1 hour',status=$2,phase='preparation',finished_at=CASE WHEN $3 THEN now() ELSE NULL END WHERE id=$1`, a.RunID, runStatus, terminal); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Pool.Exec(t.Context(), `UPDATE sessions SET sandbox_state=$2,sandbox_id=$3,slot_reserved=false WHERE id=$1`, a.SessionID, state, boxID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Pool.Exec(t.Context(), `INSERT INTO operations(id,session_id,kind,status,result,created_at,attempted_at) VALUES($1,$2,'sandbox_create',$3,$4,now()-interval '59 minutes',now()-interval '59 minutes')`, uuid.New(), a.SessionID, opStatus, result); err != nil {
				t.Fatal(err)
			}
			if err := s.RecoverProvisionReservations(t.Context()); err != nil {
				t.Fatal(err)
			}
			ids, err := s.Reserved(t.Context())
			if err != nil || len(ids) != 1 || ids[0] != a.SessionID {
				t.Fatal("old worker release hid unresolved create", ids, err)
			}
			tick(t, e)
			o := provisionOperation(t, s, a)
			saved, _ := decodeResult(o)
			if adopted {
				if !saved.Adopted || o.AbandonedAt != nil || r.deletes != 0 {
					t.Fatal("legacy adoption lost", o)
				}
				if terminal {
					tick(t, e)
					record, _, _ := s.Read(t.Context(), a.SessionID)
					if record.SlotReserved || record.SandboxState != "paused" {
						t.Fatal("paused environment changed", record)
					}
				} else {
					v, err := s.Run(t.Context(), a.SessionID, a.RunID)
					if err != nil || v.Status != session.Running {
						t.Fatal(v, err)
					}
				}
			} else {
				if o.AbandonedAt == nil || r.creates != 0 {
					t.Fatal("old attempt restarted", o)
				}
				if !terminal {
					assertProvisionTerminal(t, s, a, session.Failed, true)
					if o.DeadlineAt == nil || o.DeadlineAt.After(time.Now()) {
						t.Fatal("late legacy attempt has no original deadline")
					}
				}
			}
		})
	}
}

func TestProvisionRunningTickDoesNotLockCapacity(t *testing.T) {
	s, _, _, e := setup(t)
	tick(t, e)
	tx, err := s.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if err := store.Advisory(t.Context(), tx, store.CapacityLock); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := e.Tick(ctx); err != nil {
		t.Fatal("running tick waited for capacity lock", err)
	}
}

func TestProvisionLegacyStartingRecoversWithoutNewCreate(t *testing.T) {
	s, r, a, e := setup(t)
	if _, err := s.Pool.Exec(t.Context(), `UPDATE runs SET created_at=now()-interval '30 seconds' WHERE id=$1`, a.RunID); err != nil {
		t.Fatal(err)
	}
	oid := uuid.New()
	if _, err := s.Pool.Exec(t.Context(), `INSERT INTO operations(id,session_id,kind,status,created_at,attempted_at) VALUES($1,$2,'sandbox_create','uncertain',now()-interval '20 seconds',now()-interval '20 seconds')`, oid, a.SessionID); err != nil {
		t.Fatal(err)
	}
	p := &provisionRemote{remote: r, found: []string{"sandbox"}}
	e.Platform = p
	tick(t, e)
	o := provisionOperation(t, s, a)
	result, err := decodeResult(o)
	if err != nil || o.ID != oid || o.RunID != nil || !result.Adopted || o.DeadlineAt == nil || r.creates != 0 || p.findCalls != 1 {
		t.Fatal("legacy attempt was not recovered", o, err)
	}
	v, err := s.Run(t.Context(), a.SessionID, a.RunID)
	if err != nil || v.Status != session.Running {
		t.Fatal(v, err)
	}
}
