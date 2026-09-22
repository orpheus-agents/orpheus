//go:build integration

package worker

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skillum-ai/orpheus/internal/harness"
	"github.com/skillum-ai/orpheus/internal/session"
	"github.com/skillum-ai/orpheus/internal/store"
)

func TestPreparationRejectionsReleaseCapacity(t *testing.T) {
	for _, stage := range []string{"create", "prepare", "initialize", "exited"} {
		t.Run(stage, func(t *testing.T) {
			s, r, a, e := setup(t)
			code := "environment_unavailable"
			switch stage {
			case "create":
				r.createError = harness.ErrEnvironmentRejected
			case "prepare":
				r.prepareError = harness.ErrEnvironmentRejected
			case "initialize":
				r.initializeError = harness.ErrRejected
				code = "harness_failed"
			case "exited":
				r.initializeError = harness.ErrUncertain
				if err := e.Tick(t.Context()); !errors.Is(err, harness.ErrUncertain) {
					t.Fatal(err)
				}
				e.Disconnect()
				r.processes = nil
				r.initializeError = nil
				code = "harness_failed"
			}
			tick(t, e)
			tick(t, e)
			view, err := s.Run(t.Context(), a.SessionID, a.RunID)
			if err != nil {
				t.Fatal(err)
			}
			record, _, err := s.Read(t.Context(), a.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			if view.Status != session.Failed || view.Error == nil || view.Error.Code != code || *view.Error.Phase != "preparation" || view.ExecutionStartedAt != nil || record.SlotReserved {
				t.Fatalf("%+v %+v", view, record)
			}
		})
	}
}

func TestDeliveryRejectionsResolveMessage(t *testing.T) {
	for _, stage := range []string{"start", "steer"} {
		for _, platform := range []bool{false, true} {
			t.Run(stage+map[bool]string{false: "/native", true: "/platform"}[platform], func(t *testing.T) {
				s, r, a, e := setup(t)
				rejection := harness.ErrRejected
				code := "harness_failed"
				if platform {
					rejection = harness.ErrEnvironmentRejected
					code = "environment_unavailable"
				}
				mid := a.MessageID
				if stage == "start" {
					r.startError = rejection
				} else {
					tick(t, e)
					message, err := s.Accept(t.Context(), store.Admission{SessionID: a.SessionID, RunID: a.RunID, Key: uuid.New(), Text: "steer"})
					if err != nil {
						t.Fatal(err)
					}
					mid = message.MessageID
					r.steerError = rejection
					if !platform {
						code = "run_finished_before_delivery"
					}
				}
				tick(t, e)
				message, err := store.GetMessage(t.Context(), s.Pool, mid)
				if err != nil {
					t.Fatal(err)
				}
				if message.DeliveryStatus == nil || *message.DeliveryStatus != "rejected" || message.Error == nil || message.Error.Code != code {
					t.Fatalf("%+v", message)
				}
				view, err := s.Run(t.Context(), a.SessionID, a.RunID)
				if err != nil {
					t.Fatal(err)
				}
				if stage == "start" && view.Status != session.Failed || stage == "steer" && view.Status != session.Running {
					t.Fatal(view)
				}
			})
		}
	}
}

func TestObservationRejectionKeepsRunningTask(t *testing.T) {
	for _, stage := range []string{"lease", "snapshot"} {
		t.Run(stage, func(t *testing.T) {
			s, r, a, e := setup(t)
			tick(t, e)
			if stage == "lease" {
				r.leaseError = harness.ErrEnvironmentRejected
				e.timeoutRenewAt = time.Time{}
			} else {
				r.snapshotError = harness.ErrRejected
			}
			if err := e.Tick(t.Context()); !errors.Is(err, harness.ErrRejected) {
				t.Fatal(err)
			}
			view, err := s.Run(t.Context(), a.SessionID, a.RunID)
			if err != nil || view.Status != session.Running || view.Error != nil {
				t.Fatal(view, err)
			}
		})
	}
}

func TestContextLossStaysUnavailableAfterPause(t *testing.T) {
	s, r, a, e := setup(t)
	tick(t, e)
	r.snapshotError = harness.Failure("context_lost", "Context unavailable.")
	tick(t, e)
	tick(t, e)
	record, _, err := s.Read(t.Context(), a.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if record.SandboxState != "unavailable" || record.SlotReserved || r.state != "paused" {
		t.Fatal(record.SandboxState, record.SlotReserved, r.state)
	}
	_, err = s.Accept(t.Context(), store.Admission{SessionID: a.SessionID, Key: uuid.New(), Text: "next"})
	problem, ok := errors.AsType[*session.APIError](err)
	if !ok || problem.Problem.Code != "session_unavailable" {
		t.Fatal(err)
	}
}

func TestReplacementReconnectCompletesPreparation(t *testing.T) {
	for _, stage := range []string{"initialize", "resume"} {
		t.Run(stage, func(t *testing.T) {
			s, r, a, e := setup(t)
			tick(t, e)
			complete(r)
			tick(t, e)
			tick(t, e)
			r.processes = nil
			if _, err := s.Accept(t.Context(), store.Admission{SessionID: a.SessionID, Key: uuid.New(), Text: "next"}); err != nil {
				t.Fatal(err)
			}
			if stage == "initialize" {
				r.initializeError = harness.ErrUncertain
			} else {
				r.contextError = harness.ErrUncertain
			}
			if err := e.Tick(t.Context()); !errors.Is(err, harness.ErrUncertain) {
				t.Fatal(err)
			}
			pid := r.processes[0].PID
			if r.starts != 1 {
				t.Fatal("dispatched before initialization")
			}
			e.Disconnect()
			r.initializeError = nil
			r.contextError = nil
			restarted := executor(a.SessionID, s, r)
			defer restarted.Disconnect()
			opens := r.opens
			tick(t, restarted)
			if r.processes[0].PID != pid || r.launches != 2 || r.starts != 2 || r.opens != opens+1 || !r.logins[len(r.logins)-1] {
				t.Fatal("replacement was not resumed")
			}
			restarted.Disconnect()
			tick(t, restarted)
			if r.opens != opens+1 || r.starts != 2 || r.logins[len(r.logins)-1] {
				t.Fatal("initialized process was prepared again")
			}
		})
	}
}

func TestCancelAfterLostCreateFindsAndPausesSandbox(t *testing.T) {
	s, r, a, e := setup(t)
	r.lostCreate = true
	if err := e.Tick(t.Context()); !errors.Is(err, harness.ErrUncertain) {
		t.Fatal(err)
	}
	if _, err := s.Cancel(t.Context(), a.SessionID, a.RunID); err != nil {
		t.Fatal(err)
	}
	e.Disconnect()
	tick(t, e)
	tick(t, e)
	record, _, err := s.Read(t.Context(), a.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if r.creates != 1 || r.starts != 0 || r.pauses != 1 || record.SlotReserved {
		t.Fatalf("create=%d start=%d pause=%d reserved=%v", r.creates, r.starts, r.pauses, record.SlotReserved)
	}
}
