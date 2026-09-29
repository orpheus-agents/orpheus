//go:build integration

package worker

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus/internal/credentials"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
)

type cleanupPlatform struct {
	harness.Platform
	infos, connects         int
	infoError, connectError error
}

func (p *cleanupPlatform) Info(ctx context.Context, id string) (string, error) {
	p.infos++
	if p.infoError != nil {
		return "", p.infoError
	}
	return p.Platform.Info(ctx, id)
}

func (p *cleanupPlatform) Connect(ctx context.Context, id string, timeout time.Duration) (harness.Sandbox, error) {
	p.connects++
	if p.connectError != nil {
		return nil, p.connectError
	}
	return p.Platform.Connect(ctx, id, timeout)
}

func cleanupAccount(t *testing.T, s *store.Store, a session.Acceptance, home *string) {
	t.Helper()
	record, _, err := s.Read(t.Context(), a.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	record.Configuration.Credentials = session.Credentials{Mode: "account"}
	if _, err := s.Pool.Exec(t.Context(), "UPDATE sessions SET configuration=$2, harness_home=$3 WHERE id=$1", a.SessionID, record.Configuration, home); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteAuthPreparationIsBestEffort(t *testing.T) {
	unavailable := errors.New("unavailable")
	for _, tc := range []struct {
		name                     string
		account, home, paused    bool
		info, connect, auth      error
		infos, connects, deletes int
	}{
		{name: "api_key", info: unavailable, connect: unavailable, deletes: 1},
		{name: "account_without_home", account: true, info: unavailable, connects: 0, deletes: 1},
		{name: "account_paused", account: true, home: true, paused: true, infos: 1, deletes: 1},
		{name: "info_not_found", account: true, home: true, info: harness.ErrNotFound, infos: 1},
		{name: "connect_not_found", account: true, home: true, connect: harness.ErrNotFound, infos: 1, connects: 1},
		{name: "info_unavailable", account: true, home: true, info: unavailable, infos: 1, deletes: 1},
		{name: "connect_unavailable", account: true, home: true, connect: unavailable, infos: 1, connects: 1, deletes: 1},
		{name: "auth_factory_unavailable", account: true, home: true, auth: unavailable, infos: 1, connects: 1, deletes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, box, a, e := setupSession(t, false, session.SandboxInput{Template: "codex"}, nil)
			tick(t, e)
			complete(box)
			tick(t, e)
			e.Disconnect()
			if tc.account {
				var home *string
				if tc.home {
					home = new("/home/user/.codex")
				}
				cleanupAccount(t, s, a, home)
			}
			if tc.paused {
				box.state = "paused"
			}
			p := &cleanupPlatform{Platform: box, infoError: tc.info, connectError: tc.connect}
			e = executor(a.SessionID, s, box)
			t.Cleanup(e.Disconnect)
			e.Platform = p
			e.NewAccount = func(context.Context, harness.Sandbox, string, session.Credentials) (credentials.Sync, error) {
				return &fakeAccount{box}, tc.auth
			}
			tick(t, e)
			record, _, err := s.Read(t.Context(), a.SessionID)
			if err != nil || record.SandboxState != "deleted" || record.SandboxError != nil || record.SlotReserved {
				t.Fatal("cleanup was blocked or classified as sandbox loss", record, err)
			}
			op, err := s.LatestAttempt(t.Context(), a.SessionID, "sandbox_delete")
			if err != nil || op == nil || op.Status != "confirmed" {
				t.Fatal("deletion was not confirmed", op, err)
			}
			if p.infos != tc.infos || p.connects != tc.connects || box.deletes != tc.deletes {
				t.Fatalf("info=%d connect=%d delete=%d", p.infos, p.connects, box.deletes)
			}
		})
	}
}

func TestDeleteRetriesDoNotRenewLease(t *testing.T) {
	for _, account := range []bool{false, true} {
		t.Run(map[bool]string{false: "api_key", true: "account"}[account], func(t *testing.T) {
			s, box, a, e := setupSession(t, false, session.SandboxInput{Template: "codex"}, nil)
			tick(t, e)
			complete(box)
			tick(t, e)
			e.Disconnect()
			if account {
				cleanupAccount(t, s, a, new("/home/user/.codex"))
			}
			p := &cleanupPlatform{Platform: box}
			box.deleteError = errors.New("delete unavailable")
			for range 4 {
				// A fresh executor cannot rely on in-memory settleAuthSynced.
				e = executor(a.SessionID, s, box)
				e.Platform = p
				tick(t, e)
				e.Disconnect()
				record, _, err := s.Read(t.Context(), a.SessionID)
				if err != nil || !record.SlotReserved || record.SandboxState != "deleting" {
					t.Fatal(record, err)
				}
			}
			want := 0
			if account {
				want = 1
			}
			if p.connects != want || p.infos != want || box.syncs != want || box.deletes != 4 {
				t.Fatalf("retry repeated auth preparation: info=%d connect=%d sync=%d delete=%d", p.infos, p.connects, box.syncs, box.deletes)
			}
			box.deleteError = nil
			e = executor(a.SessionID, s, box)
			e.Platform = p
			defer e.Disconnect()
			tick(t, e)
			view, err := s.Session(t.Context(), a.SessionID)
			if err != nil || view.Sandbox.State != "deleted" || view.Status != session.Completed {
				t.Fatal(view, err)
			}
		})
	}
}

func TestUnneededPauseDoesNotCreateOperation(t *testing.T) {
	for _, state := range []string{"paused", "lost"} {
		t.Run(state, func(t *testing.T) {
			s, box, a, e := setup(t)
			tick(t, e)
			complete(box)
			tick(t, e)
			e.Disconnect()
			box.state = state
			tick(t, e)
			var count int
			if err := s.Pool.QueryRow(t.Context(), "SELECT count(*) FROM operations WHERE session_id=$1 AND kind='pause'", a.SessionID).Scan(&count); err != nil || count != 0 {
				t.Fatal("unnecessary pending pause operation", count, err)
			}
		})
	}
}

func TestReleasedSandboxLossIsNotSettledAgain(t *testing.T) {
	for _, multiple := range []bool{false, true} {
		t.Run(map[bool]string{false: "single_run", true: "multiple_runs"}[multiple], func(t *testing.T) {
			s, box, a, e := setupSession(t, multiple, session.SandboxInput{Template: "codex"}, nil)
			tick(t, e)
			box.state = "lost"
			box.snapshotError = harness.ErrNotFound
			tick(t, e)
			e.Disconnect()
			before, _, err := s.Read(t.Context(), a.SessionID)
			if err != nil || before.SlotReserved || before.SandboxState != "unavailable" || before.SandboxError == nil || before.SandboxError.Code != "sandbox_lost" {
				t.Fatal("sandbox loss did not release the session", before, err)
			}
			// Simulate an executor started from a stale Reserved result.
			p := &cleanupPlatform{Platform: box}
			e = executor(a.SessionID, s, box)
			e.Platform = p
			t.Cleanup(e.Disconnect)
			if err := e.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			e.Run(ctx)
			if ctx.Err() != nil {
				t.Fatal("executor did not exit after slot release", ctx.Err())
			}
			after, _, err := s.Read(t.Context(), a.SessionID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("extra tick changed the released session or emitted events", before, after, err)
			}
			if p.infos != 0 || p.connects != 0 || box.deletes != 0 || box.pauses != 0 {
				t.Fatalf("extra tick contacted sandbox: info=%d connect=%d delete=%d pause=%d", p.infos, p.connects, box.deletes, box.pauses)
			}
		})
	}
}
