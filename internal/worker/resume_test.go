//go:build integration

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

func pausedContinuation(t *testing.T) (*store.Store, *provisionRemote, session.Acceptance, *Executor) {
	t.Helper()
	s, r, first, e := setup(t)
	tick(t, e)
	complete(r)
	tick(t, e)
	tick(t, e)
	if r.state != "paused" || e.sandbox != nil {
		t.Fatal("first run did not settle", r.state)
	}
	next, err := s.Accept(t.Context(), store.Admission{SessionID: first.SessionID, Key: uuid.New(), Messages: []session.TextMessage{{Text: "continue"}}})
	if err != nil {
		t.Fatal(err)
	}
	p := &provisionRemote{remote: r}
	e.Platform = p
	return s, p, next, e
}

func resumeOperation(t *testing.T, s *store.Store, a session.Acceptance) store.Operation {
	t.Helper()
	o, err := db.New(s.Pool).FindOperation(t.Context(), db.FindOperationParams{SessionID: a.SessionID, RunID: &a.RunID, Kind: "resume"})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

type resumeUnavailable struct{}

func (resumeUnavailable) Error() string   { return "upstream failed: secret detail" }
func (resumeUnavailable) HTTPStatus() int { return 503 }

func TestResumeRetrySurvivesRestart(t *testing.T) {
	for _, recover := range []bool{true, false} {
		t.Run(map[bool]string{true: "recover", false: "deadline"}[recover], func(t *testing.T) {
			s, p, a, e := pausedContinuation(t)
			now := time.Now().UTC()
			e.clock = func() time.Time { return now }
			p.connectError = resumeUnavailable{}
			tick(t, e)
			o := resumeOperation(t, s, a)
			var first sandboxFailure
			if err := json.Unmarshal(o.FirstError, &first); err != nil || first != (sandboxFailure{Code: "platform_error", Method: "Connect", HTTPStatus: 503}) {
				t.Fatal(first, err)
			}
			if o.CheckAttempts != 1 || o.NextCheckAt == nil || o.DeadlineAt == nil || !o.DeadlineAt.Equal(o.CreatedAt.Add(120*time.Second)) {
				t.Fatal(o)
			}
			e.Disconnect()
			e = executor(a.SessionID, s, p.remote)
			defer e.Disconnect()
			e.Platform, e.clock = p, func() time.Time { return now }
			tick(t, e)
			if p.infoCalls != 1 {
				t.Fatal("restart bypassed backoff")
			}
			now = *o.NextCheckAt
			tick(t, e)
			again := resumeOperation(t, s, a)
			if p.infoCalls != 2 || again.ID != o.ID || !again.DeadlineAt.Equal(*o.DeadlineAt) || again.CheckAttempts != 2 || string(again.FirstError) != string(o.FirstError) {
				t.Fatal(again)
			}
			if recover {
				now = *again.NextCheckAt
				p.connectError = nil
			} else {
				now = *o.DeadlineAt
			}
			tick(t, e)
			v, err := s.Run(t.Context(), a.SessionID, a.RunID)
			if err != nil || p.creates != 1 || p.launches != 1 || p.deletes != 0 {
				t.Fatal(v, err)
			}
			if recover {
				if v.Status != session.Running || p.starts != 2 || resumeOperation(t, s, a).Status != "confirmed" {
					t.Fatal(v)
				}
				now = o.DeadlineAt.Add(time.Hour)
				e.Disconnect()
				tick(t, e)
				v, err = s.Run(t.Context(), a.SessionID, a.RunID)
				if err != nil || v.Status != session.Running {
					t.Fatal("deadline leaked into execution", v, err)
				}
			} else {
				if v.Status != session.Failed || v.Error == nil || v.Error.Code != "sandbox_resume_timeout" || v.ExecutionStartedAt != nil || p.starts != 1 || p.infoCalls != 2 {
					t.Fatal(v, p.infoCalls)
				}
				tick(t, e)
				record, active, err := s.Read(t.Context(), a.SessionID)
				if err != nil || active != nil || record.SlotReserved || record.SandboxState != "paused" || record.SandboxID == nil || *record.SandboxID != "sandbox" || record.Workspace == nil {
					t.Fatal(record.Sandbox(), err)
				}
			}
		})
	}
}

func TestResumeLateResponse(t *testing.T) {
	for _, method := range []string{"Info", "Connect"} {
		for _, stop := range []string{"cancel", "timeout"} {
			t.Run(method+"/"+stop, func(t *testing.T) {
				s, p, a, e := pausedContinuation(t)
				if err := db.New(s.Pool).InsertHookExecution(t.Context(), db.InsertHookExecutionParams{ID: uuid.New(), SessionID: a.SessionID, RunID: a.RunID, Name: "before_run", Status: "pending"}); err != nil {
					t.Fatal(err)
				}
				now := time.Now().UTC()
				e.clock = func() time.Time { return now }
				halt := func() {
					now = now.Add(3 * time.Minute)
					if stop == "cancel" {
						if _, err := s.Cancel(t.Context(), a.SessionID, a.RunID); err != nil {
							t.Fatal(err)
						}
					}
				}
				if method == "Info" {
					p.afterInfo = halt
				} else {
					p.afterConnect = halt
				}
				tick(t, e)
				v, err := s.Run(t.Context(), a.SessionID, a.RunID)
				if err != nil || v.ExecutionStartedAt != nil || p.starts != 1 || p.creates != 1 || p.launches != 1 || len(v.Hooks) != 1 || v.Hooks[0].Status != "skipped" || v.Hooks[0].StartedAt != nil {
					t.Fatal(v, err)
				}
				if stop == "cancel" && (v.Status != session.Cancelled || v.Error != nil) {
					t.Fatal(v)
				}
				if stop == "timeout" && (v.Status != session.Failed || v.Error == nil || v.Error.Code != "sandbox_resume_timeout") {
					t.Fatal(v)
				}
				if method == "Info" && p.resumes != 0 {
					t.Fatal("Connect dispatched after stop")
				}
				p.afterInfo, p.afterConnect = nil, nil
				tick(t, e)
				record, active, err := s.Read(t.Context(), a.SessionID)
				if err != nil || active != nil || record.SlotReserved || record.SandboxState != "paused" || record.SandboxID == nil || p.state != "paused" || p.deletes != 0 {
					t.Fatal(record.Sandbox(), err)
				}
			})
		}
	}
}

func TestResumePendingUpgradeAndProviderFailures(t *testing.T) {
	for _, scenario := range []string{"old", "rejected", "lost", "info_retry", "cancel_backoff"} {
		t.Run(scenario, func(t *testing.T) {
			s, p, a, e := pausedContinuation(t)
			switch scenario {
			case "old":
				o, err := e.operation(t.Context(), "resume", &a.RunID, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.Pool.Exec(t.Context(), "UPDATE operations SET created_at=now()-interval '3 hours',status='sending' WHERE id=$1", o.ID); err != nil {
					t.Fatal(err)
				}
			case "rejected":
				p.connectError = harness.ErrEnvironmentRejected
			case "lost":
				p.infoError = harness.ErrNotFound
			case "info_retry", "cancel_backoff":
				p.infoError = errors.New("network offline")
			}
			tick(t, e)
			switch scenario {
			case "info_retry", "cancel_backoff":
				o := resumeOperation(t, s, a)
				if o.CheckAttempts != 1 || o.NextCheckAt == nil || o.Status != "uncertain" {
					t.Fatal(o)
				}
				if scenario == "info_retry" {
					p.infoError = nil
					e.clock = func() time.Time { return *o.NextCheckAt }
				} else if _, err := s.Cancel(t.Context(), a.SessionID, a.RunID); err != nil {
					t.Fatal(err)
				}
				tick(t, e)
			}
			v, err := s.Run(t.Context(), a.SessionID, a.RunID)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "old":
				if v.Error == nil || v.Error.Code != "sandbox_resume_timeout" || p.infoCalls != 0 {
					t.Fatal(v, p.infoCalls)
				}
			case "rejected":
				if v.Error == nil || v.Error.Code != "environment_unavailable" {
					t.Fatal(v)
				}
			case "lost":
				record, _, err := s.Read(t.Context(), a.SessionID)
				if v.Error == nil || v.Error.Code != "sandbox_lost" || err != nil || record.SandboxState != "unavailable" || record.SlotReserved {
					t.Fatal(v, err)
				}
			case "info_retry":
				if v.Status != session.Running || p.infoCalls != 2 {
					t.Fatal(v)
				}
			case "cancel_backoff":
				if v.Status != session.Cancelled || p.infoCalls != 1 || resumeOperation(t, s, a).Status != "failed" {
					t.Fatal(v)
				}
			}
		})
	}
}

// Neither a slow Info nor a subsequent Connect may outlive the remaining budget.
type resumeRPCProbe struct {
	*provisionRemote
	budgets []time.Duration
}

func (p *resumeRPCProbe) Info(ctx context.Context, id string) (string, error) {
	d, _ := ctx.Deadline()
	p.budgets = append(p.budgets, time.Until(d))
	return p.provisionRemote.Info(ctx, id)
}
func (p *resumeRPCProbe) Connect(ctx context.Context, id string, lease time.Duration) (harness.Sandbox, error) {
	d, _ := ctx.Deadline()
	p.budgets = append(p.budgets, time.Until(d))
	return p.provisionRemote.Connect(ctx, id, lease)
}
func TestResumeLimitsEachRPC(t *testing.T) {
	s, p, a, e := pausedContinuation(t)
	probe := &resumeRPCProbe{provisionRemote: p}
	e.Platform = probe
	o, err := e.operation(t.Context(), "resume", &a.RunID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := o.CreatedAt
	e.clock = func() time.Time { return now }
	p.afterInfo = func() { now = o.CreatedAt.Add(s.Settings.SandboxProvisionTimeout - 2*time.Second) }
	tick(t, e)
	if len(probe.budgets) != 2 || probe.budgets[0] > s.Settings.RPCTimeout || probe.budgets[0] < s.Settings.RPCTimeout-time.Second || probe.budgets[1] > 2*time.Second || probe.budgets[1] < time.Second {
		t.Fatal(probe.budgets)
	}
}
