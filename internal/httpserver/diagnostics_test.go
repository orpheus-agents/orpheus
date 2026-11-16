//go:build integration

package httpserver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/client"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
)

func TestDiagnosticIDInSnapshotAndSSE(t *testing.T) {
	server, s := testServer(t)
	_, _, raw := requestHTTP(t, server, "POST", "/api/v1/sessions", validBody, "key", uuid.NewString())
	var a session.Acceptance
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	problem := &session.Error{Code: "harness_failed", Message: "Harness rejected initialization.", Phase: new("preparation"), Details: []session.Detail{}, DiagnosticID: &id}
	if err := s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, r *store.SessionRecord) error {
		run, err := store.GetRun(t.Context(), tx, a.SessionID, a.RunID)
		if err != nil {
			return err
		}
		return store.Finish(t.Context(), tx, r, &run, session.Failed, problem, nil)
	}); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/sessions/" + a.SessionID.String()
	_, _, raw = requestHTTP(t, server, "GET", path+"/runs/"+a.RunID.String(), "", "key", "")
	var run client.Run
	if err := json.Unmarshal(raw, &run); err != nil || run.Error == nil || run.Error.DiagnosticID == nil || *run.Error.DiagnosticID != id {
		t.Fatal("snapshot lost ID", err)
	}
	req, err := http.NewRequestWithContext(t.Context(), "GET", server.URL+path+"/events/stream?after=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer key")
	res, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	scanner := bufio.NewScanner(res.Body)
	for scanner.Scan() {
		line := scanner.Bytes()
		if bytes.HasPrefix(line, []byte("data: ")) && bytes.Contains(line, []byte(id.String())) {
			if !strings.Contains(string(line), problem.Message) {
				t.Fatal("SSE lost public error")
			}
			return
		}
	}
	t.Fatal("SSE lost diagnostic ID", scanner.Err())
}
