//go:build integration

package store

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/harness/codex"
	"github.com/orpheus-agents/orpheus/internal/session"
)

func TestReconcilePreservesHarnessError(t *testing.T) {
	for _, tc := range []struct {
		name, raw, code, message string
	}{
		{"provider", `{"message":"Model request failed","additionalDetails":"HTTP 503"}`, "harness_failed", "Model request failed\nHTTP 503"},
		{"authentication", `{"message":"Authentication failed: token_expired"}`, "authentication_failed", "Authentication failed: token_expired"},
		{"fallback", `null`, "harness_failed", "Harness execution failed."},
	} {
		for _, withHook := range []bool{false, true} {
			name := tc.name
			if withHook {
				name += "/after_run"
			}
			t.Run(name, func(t *testing.T) {
				s := fixture(t)
				req := request()
				if withHook {
					req.Create.Configuration.Hooks = &session.HooksInput{AfterRun: new("#!/bin/sh\ntrue\n")}
				}
				a, err := s.Accept(t.Context(), req)
				if err != nil {
					t.Fatal(err)
				}
				bindUsageRun(t, s, a, "turn")
				snapshot := codex.Normalize(codex.Thread{Turns: []codex.NativeTurn{{ID: "turn", Status: "failed", Error: json.RawMessage(tc.raw)}}}, nil, nil, 1024, nil, nil)
				apply := func() error {
					return s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, r *SessionRecord) error {
						return Reconcile(t.Context(), tx, r, snapshot)
					})
				}
				if err := apply(); err != nil {
					t.Fatal(err)
				}
				want := &session.Error{Code: tc.code, Message: tc.message, Phase: new("execution"), Details: []session.Detail{}}
				run, err := s.Run(t.Context(), a.SessionID, a.RunID)
				if err != nil || !reflect.DeepEqual(run.Error, want) || !reflect.DeepEqual(run.AgentError, want) {
					t.Fatalf("run error: %+v, agent error: %+v, read error: %v", run.Error, run.AgentError, err)
				}
				record, _, err := s.Read(t.Context(), a.SessionID)
				if err != nil {
					t.Fatal(err)
				}
				view, err := SessionView(t.Context(), s.Pool, record)
				if err != nil || !reflect.DeepEqual(view.Error, want) {
					t.Fatalf("session error: %+v, read error: %v", view.Error, err)
				}
				events, err := s.Events(t.Context(), a.SessionID, "0", 50)
				if err != nil {
					t.Fatal(err)
				}
				var eventRun session.Run
				last := events.Items[len(events.Items)-1]
				if last.Type != "run.updated" {
					t.Fatalf("unexpected event: %+v", last)
				}
				if err := json.Unmarshal(last.Data, &eventRun); err != nil || !reflect.DeepEqual(eventRun.Error, want) {
					t.Fatalf("event error: %+v, decode error: %v", eventRun.Error, err)
				}
				if err := apply(); err != nil {
					t.Fatal(err)
				}
				after, _, err := s.Read(t.Context(), a.SessionID)
				if err != nil || after.NextEventSequence != record.NextEventSequence {
					t.Fatalf("replay changed events: %v", err)
				}
			})
		}
	}
}
