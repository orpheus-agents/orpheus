//go:build integration

package httpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/session"
)

func TestSingleRunHTTP(t *testing.T) {
	server, s := testServer(t)
	base := strings.Replace(validBody, `"allow_multiple_runs":true,`, "", 1)
	for _, value := range []string{"", "false", "true"} {
		t.Run(value, func(t *testing.T) {
			body := base
			if value != "" {
				body = `{"allow_multiple_runs":` + value + `,` + base[1:]
			}
			key := uuid.NewString()
			status, _, raw := requestHTTP(t, server, "POST", "/api/v1/sessions", body, "key", key)
			if status != 202 {
				t.Fatal(status, string(raw))
			}
			var a session.Acceptance
			if err := json.Unmarshal(raw, &a); err != nil {
				t.Fatal(err)
			}
			path := "/api/v1/sessions/" + a.SessionID.String()
			_, _, raw = requestHTTP(t, server, "GET", path, "", "key", "")
			var view session.Session
			if err := json.Unmarshal(raw, &view); err != nil || view.AllowMultipleRuns != (value == "true") {
				t.Fatal(view, err)
			}
			_, _, raw = requestHTTP(t, server, "GET", "/api/v1/sessions", "", "key", "")
			var page session.Page[session.Session]
			if err := json.Unmarshal(raw, &page); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, item := range page.Items {
				if item.ID == a.SessionID {
					found = true
					if item.AllowMultipleRuns != view.AllowMultipleRuns {
						t.Fatal("list lost policy")
					}
				}
			}
			if !found {
				t.Fatal("session missing from list")
			}
			if _, err := s.Cancel(t.Context(), a.SessionID, a.RunID); err != nil {
				t.Fatal(err)
			}
			status, _, raw = requestHTTP(t, server, "POST", path+"/runs", `{"messages":[{"text":"again"}]}`, "key", uuid.NewString())
			if value == "true" {
				if status != 202 {
					t.Fatal(status, string(raw))
				}
			} else if status != 409 || !strings.Contains(string(raw), `"code":"multiple_runs_not_allowed"`) || !strings.Contains(string(raw), "Create a new session") {
				t.Fatal(status, string(raw))
			}
			if value == "" {
				body = `{"allow_multiple_runs":false,` + base[1:]
			}
			status, _, raw = requestHTTP(t, server, "POST", "/api/v1/sessions", body, "key", key)
			var replay session.Acceptance
			if err := json.Unmarshal(raw, &replay); err != nil || status != 202 || replay != a {
				t.Fatal("replay", status, string(raw), err)
			}
		})
	}
	for _, value := range []string{"null", `"false"`, "0", "1", "[]", "{}"} {
		status, _, raw := requestHTTP(t, server, "POST", "/api/v1/sessions", `{"allow_multiple_runs":`+value+`,`+base[1:], "key", uuid.NewString())
		if status != 422 {
			t.Fatalf("%s: %d %s", value, status, raw)
		}
	}
}
