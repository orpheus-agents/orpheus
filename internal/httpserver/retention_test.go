//go:build integration

package httpserver

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
)

func TestUnavailableSandboxRunAdmissionHTTP(t *testing.T) {
	for _, state := range []string{"unavailable", "deleting", "deleted"} {
		t.Run(state, func(t *testing.T) {
			server, s := testServer(t)
			key := uuid.NewString()
			body := validBody
			a := decodeHTTP[session.Acceptance](t, externalRequest(t, server, "POST", "/api/v1/sessions", body, key, 202))
			if err := s.Mutate(t.Context(), a.SessionID, true, func(tx pgx.Tx, r *store.SessionRecord) error {
				run, err := store.GetRun(t.Context(), tx, a.SessionID, a.RunID)
				if err != nil {
					return err
				}
				if err := store.Finish(t.Context(), tx, r, &run, session.Completed, nil, nil); err != nil {
					return err
				}
				r.SandboxState, r.SlotReserved = "paused", false
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			runKey := uuid.NewString()
			runBody := `{"messages":[{"text":"second"}]}`
			path := "/api/v1/sessions/" + a.SessionID.String() + "/runs"
			second := decodeHTTP[session.Acceptance](t, externalRequest(t, server, "POST", path, runBody, runKey, 202))
			if err := s.Mutate(t.Context(), a.SessionID, true, func(tx pgx.Tx, r *store.SessionRecord) error {
				run, err := store.GetRun(t.Context(), tx, a.SessionID, second.RunID)
				if err != nil {
					return err
				}
				if err := store.Finish(t.Context(), tx, r, &run, session.Completed, nil, nil); err != nil {
					return err
				}
				r.SandboxState, r.SlotReserved = state, false
				r.TotalTokens = r.Configuration.Public.Limits.MaxSessionTokens
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			replayRun := decodeHTTP[session.Acceptance](t, externalRequest(t, server, "POST", path, runBody, runKey, 202))
			if replayRun != second {
				t.Fatal(replayRun, second)
			}
			result := externalRequest(t, server, "POST", "/api/v1/sessions/"+a.SessionID.String()+"/runs", `{"messages":[{"text":"continue"}]}`, uuid.NewString(), 409)
			if !strings.Contains(string(result), `"code":"session_unavailable"`) {
				t.Fatal(string(result))
			}
			replay := decodeHTTP[session.Acceptance](t, externalRequest(t, server, "POST", "/api/v1/sessions", body, key, 202))
			if replay != a {
				t.Fatal(replay, a)
			}
			var count int
			if err := s.Pool.QueryRow(t.Context(), "SELECT count(*) FROM runs WHERE session_id=$1", a.SessionID).Scan(&count); err != nil || count != 2 {
				t.Fatal(count, err)
			}
			r, _, err := s.Read(t.Context(), a.SessionID)
			if err != nil || r.SlotReserved {
				t.Fatal(r, err)
			}
		})
	}
}
