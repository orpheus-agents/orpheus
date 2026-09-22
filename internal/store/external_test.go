//go:build integration

package store

import (
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skillum-ai/orpheus/internal/harness"
	"github.com/skillum-ai/orpheus/internal/session"
)

func externalAdmission() Admission {
	a := request()
	a.Create.Namespace = new("redmine")
	a.Create.ExternalKey = new("prod:issue:7")
	a.Create.InputFingerprint = new("v1")
	a.Create.Message.ExternalKey = new("journal:1")
	return a
}

func TestExternalPagination(t *testing.T) {
	s := fixture(t)
	var accepted []session.Acceptance
	for i := range 6 {
		req := externalAdmission()
		if i == 5 {
			req.Create.Namespace = new("other")
		}
		a, err := s.Accept(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		accepted = append(accepted, a)
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, table := range []string{"sessions", "runs"} {
		if _, err := s.Pool.Exec(t.Context(), "UPDATE "+table+" SET created_at=$1", at); err != nil {
			t.Fatal(err)
		}
	}
	var sid, rid []string
	for _, a := range accepted[:5] {
		sid = append(sid, a.SessionID.String())
		rid = append(rid, a.RunID.String())
	}
	slices.Sort(sid)
	slices.Sort(rid)
	for _, order := range []string{"asc", "desc"} {
		t.Run(order, func(t *testing.T) {
			wantS, wantR := slices.Clone(sid), slices.Clone(rid)
			if order == "desc" {
				slices.Reverse(wantS)
				slices.Reverse(wantR)
			}
			filter := ListFilter{Namespace: new("redmine"), ExternalKey: new("prod:issue:7"), Order: order}
			var gotS, gotR []string
			cursor := ""
			for i := 0; i < 5; i++ {
				page, err := s.ListSessions(t.Context(), 1+i, cursor, filter)
				if err != nil {
					t.Fatal(err)
				}
				for _, v := range page.Items {
					gotS = append(gotS, v.ID.String())
				}
				if page.NextCursor == nil {
					break
				}
				cursor = *page.NextCursor
			}
			cursor = ""
			for i := 0; i < 5; i++ {
				page, err := s.ListAllRuns(t.Context(), 1+i, cursor, filter)
				if err != nil {
					t.Fatal(err)
				}
				for _, v := range page.Items {
					gotR = append(gotR, v.ID.String())
				}
				if page.NextCursor == nil {
					break
				}
				cursor = *page.NextCursor
			}
			if !slices.Equal(gotS, wantS) || !slices.Equal(gotR, wantR) {
				t.Fatal(gotS, wantS, gotR, wantR)
			}
		})
	}
	filter := ListFilter{Namespace: new("redmine")}
	first, err := s.ListAllRuns(t.Context(), 1, "", filter)
	if err != nil || first.NextCursor == nil {
		t.Fatal(first, err)
	}
	for _, changed := range []ListFilter{{Namespace: new("other")}, {Namespace: new("redmine"), Order: "desc"}, {Namespace: new("redmine"), Status: new("accepted")}, {Namespace: new("redmine"), InputFingerprint: new("v1")}} {
		_, err := s.ListAllRuns(t.Context(), 1, *first.NextCursor, changed)
		requireCode(t, err, "invalid_cursor")
	}
	_, err = s.ListSessions(t.Context(), 1, *first.NextCursor, filter)
	requireCode(t, err, "invalid_cursor")
	filter.Order = "asc"
	if _, err := s.ListAllRuns(t.Context(), 2, *first.NextCursor, filter); err != nil {
		t.Fatal(err)
	}
	for _, f := range []ListFilter{{Namespace: new("REDMINE")}, {Namespace: new(" redmine")}, {ExternalKey: new("prod:issue:8")}, {InputFingerprint: new("V1")}, {Status: new("completed")}} {
		p, err := s.ListAllRuns(t.Context(), 50, "", f)
		if err != nil || len(p.Items) != 0 {
			t.Fatal(p, err)
		}
	}
	// Latest session status must not match an older, completed run.
	a := accepted[0]
	if _, err := s.Cancel(t.Context(), a.SessionID, a.RunID); err != nil {
		t.Fatal(err)
	}
	next, err := s.Accept(t.Context(), Admission{Key: uuid.New(), SessionID: a.SessionID, Text: "next", InputFingerprint: new("v1")})
	if err != nil {
		t.Fatal(err)
	}
	last, err := s.ListRuns(t.Context(), a.SessionID, 1, "", ListFilter{Order: "desc"})
	if err != nil || last.Items[0].ID != next.RunID || last.NextCursor == nil {
		t.Fatal(last, err)
	}
	prev, err := s.ListRuns(t.Context(), a.SessionID, 2, *last.NextCursor, ListFilter{Order: "desc"})
	if err != nil || len(prev.Items) != 1 || prev.Items[0].ID != a.RunID {
		t.Fatal(prev, err)
	}
	_, err = s.ListRuns(t.Context(), accepted[1].SessionID, 2, *last.NextCursor, ListFilter{Order: "desc"})
	requireCode(t, err, "invalid_cursor")
	p, err := s.ListSessions(t.Context(), 50, "", ListFilter{Status: new("cancelled")})
	if err != nil || len(p.Items) != 0 {
		t.Fatal(p, err)
	}
	r, err := s.ListAllRuns(t.Context(), 50, "", ListFilter{Status: new("cancelled")})
	if err != nil || len(r.Items) != 1 || r.Items[0].ID != a.RunID {
		t.Fatal(r, err)
	}
}

func TestExternalHistorySnapshotAndRecovery(t *testing.T) {
	s := fixture(t)
	req := externalAdmission()
	a, err := s.Accept(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, rec *SessionRecord) error {
		r, e := GetRun(t.Context(), tx, a.SessionID, a.RunID)
		if e != nil {
			return e
		}
		r.Status = session.Running
		r.NativeTurnID = new("turn")
		return PublishRun(t.Context(), tx, rec, &r)
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Accept(t.Context(), Admission{Key: uuid.New(), SessionID: a.SessionID, RunID: a.RunID, Text: "second", MessageExternalKey: new("journal:1")})
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.History(t.Context(), a.SessionID, nil, 1, "", new("journal:1"))
	if err != nil || old.NextCursor == nil {
		t.Fatal(old, err)
	}
	err = s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, rec *SessionRecord) error {
		for _, id := range []uuid.UUID{a.MessageID, b.MessageID} {
			m, e := GetMessage(t.Context(), tx, id)
			if e != nil {
				return e
			}
			m.NativeKey = new("thread:turn:" + id.String())
			m.DeliveryStatus = new("sending")
			if e := PublishMessage(t.Context(), tx, rec, &m); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Accept(t.Context(), Admission{Key: uuid.New(), SessionID: a.SessionID, RunID: a.RunID, Text: "third", MessageExternalKey: new("journal:1")})
	if err != nil {
		t.Fatal(err)
	}
	tail, err := s.History(t.Context(), a.SessionID, nil, 10, *old.NextCursor, new("journal:1"))
	if err != nil || len(tail.Items) != 1 || tail.EventCursor != old.EventCursor || *tail.Items[0].Message.DeliveryStatus != "pending" || tail.Items[0].Message.ID != b.MessageID {
		t.Fatal(tail, err)
	}
	_, err = s.History(t.Context(), a.SessionID, nil, 10, *old.NextCursor, new("journal:2"))
	requireCode(t, err, "invalid_cursor")
	_, err = s.History(t.Context(), a.SessionID, &a.RunID, 10, *old.NextCursor, new("journal:1"))
	requireCode(t, err, "invalid_cursor")
	snapshot := harness.Snapshot{Turns: []harness.Turn{{NativeID: "turn", Status: session.Completed, Items: []harness.Item{
		{NativeID: a.MessageID.String(), Type: "user", Text: "Hello", Index: 0},
		{NativeID: b.MessageID.String(), Type: "user", Text: "second", Index: 1},
		{NativeID: "answer", Type: "assistant", Text: "done", Kind: new("answer"), Index: 2},
	}}}}
	for range 2 {
		if err := s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, rec *SessionRecord) error {
			rec.ThreadID = new("thread")
			return Reconcile(t.Context(), tx, rec, snapshot)
		}); err != nil {
			t.Fatal(err)
		}
	}
	events, err := s.Events(t.Context(), a.SessionID, "0", 200)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, rec *SessionRecord) error {
		rec.ThreadID = new("thread")
		return Reconcile(t.Context(), tx, rec, snapshot)
	}); err != nil {
		t.Fatal(err)
	}
	unchanged, err := s.Events(t.Context(), a.SessionID, events.NextCursor, 200)
	if err != nil || len(unchanged.Items) != 0 {
		t.Fatal(unchanged, err)
	}

	// The two SQL paths must expose identical positions and delivery snapshots.
	unfiltered, err := s.History(t.Context(), a.SessionID, nil, 50, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := s.History(t.Context(), a.SessionID, nil, 50, "", new("journal:1"))
	if err != nil {
		t.Fatal(err)
	}
	var matching []session.HistoryItem
	for _, item := range unfiltered.Items {
		if item.Message != nil && item.Message.ExternalKey != nil && *item.Message.ExternalKey == "journal:1" {
			matching = append(matching, item)
		}
	}
	if !reflect.DeepEqual(matching, filtered.Items) || unfiltered.EventCursor != filtered.EventCursor {
		t.Fatal("history query paths disagree")
	}
	// Recreate Store to demonstrate that metadata is durable, not adapter-local.
	restored := *s
	all, err := restored.History(t.Context(), a.SessionID, &a.RunID, 50, "", new("journal:1"))
	if err != nil || len(all.Items) != 3 {
		t.Fatal(all, err)
	}
	for _, item := range all.Items {
		if item.Message == nil || item.Message.Role != "user" || item.Message.ExternalKey == nil || *item.Message.ExternalKey != "journal:1" {
			t.Fatal(item)
		}
		if item.Message.ID == c.MessageID && *item.Message.DeliveryStatus != "rejected" {
			t.Fatal(item)
		}
	}
	v, err := restored.Run(t.Context(), a.SessionID, a.RunID)
	if err != nil || v.InputFingerprint == nil || *v.InputFingerprint != "v1" || v.FinalMessage == nil || v.FinalMessage.ExternalKey != nil {
		t.Fatal(v, err)
	}
	// Accepted replays precede capacity and busy checks; omission of metadata conflicts.
	s.Settings.MaxConcurrentSessions = 0
	if got, err := s.Accept(t.Context(), req); err != nil || got != a {
		t.Fatal(got, err)
	}
	req.Create.InputFingerprint = nil
	_, err = s.Accept(t.Context(), req)
	requireCode(t, err, "idempotency_conflict")
}

func TestExternalCursorComponents(t *testing.T) {
	s := fixture(t)
	scope, err := (ListFilter{}).scope("sessions")
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`null`, `{"Date":null,"ID":"00000000-0000-0000-0000-000000000001"}`, `{"Date":"2026-01-01T00:00:00Z","ID":"00000000-0000-0000-0000-000000000000"}`, `{"Date":"invalid","ID":"invalid"}`} {
		_, err := s.ListSessions(t.Context(), 50, encodeCursor(scope, json.RawMessage(raw)), ListFilter{})
		requireCode(t, err, "invalid_cursor")
	}
	for _, f := range []ListFilter{{Order: "other"}, {Status: new("finalizing")}, {Namespace: new("")}, {ExternalKey: new("\x00")}, {InputFingerprint: new(" ")}} {
		_, err := s.ListAllRuns(t.Context(), 50, "", f)
		requireCode(t, err, "validation_error")
	}
}

func TestEventPaginationUsesNumericSequence(t *testing.T) {
	s := fixture(t)
	a, err := s.Accept(t.Context(), externalAdmission())
	if err != nil {
		t.Fatal(err)
	}
	err = s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, rec *SessionRecord) error {
		for range 20 {
			if err := Emit(t.Context(), tx, rec, "sandbox.updated", rec.Sandbox()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	after := "0"
	next := 1
	for {
		page, err := s.Events(t.Context(), a.SessionID, after, 3)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range page.Items {
			if event.ID != strconv.Itoa(next) {
				t.Fatalf("expected sequence %d, got %s", next, event.ID)
			}
			next++
		}
		if !page.HasMore {
			break
		}
		after = page.NextCursor
	}
	if next != 23 {
		t.Fatal("events skipped", next)
	}
}
