package servicecredentials

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/session"
)

func sourceAccess() session.AccessContext {
	return session.AccessContext{InvocationID: uuid.New(), Subject: session.AccessSubject{Namespace: "service", ID: "review-bot"}, Source: &session.SourceContext{Identity: session.AccessSubject{Namespace: "gitlab", ID: "project/42"}, Revision: "head-7", Audience: []session.AccessSubject{{Namespace: "service", ID: "result-reader"}}}, ScopeContext: session.AccessScopeContext{"repository": json.RawMessage(`9007199254740993`)}, ContextObservedAt: time.Now().UTC(), AllowedInputSubjects: []session.AccessSubject{{Namespace: "users", ID: "reviewer"}}}
}

func TestSourceConfirmationLifecycle(t *testing.T) {
	t.Setenv("TEST_PROVIDER_KEY", strings.Repeat("k", 32))
	grant := testGrant()
	access := sourceAccess()
	expected, _ := json.Marshal(access)
	var revokeIDs []string
	confirms, issues := 0, 0
	changed := false
	mux := http.NewServeMux()
	mux.HandleFunc("/source", func(w http.ResponseWriter, r *http.Request) {
		confirms++
		var in struct {
			Operation     string          `json:"operation"`
			Context       json.RawMessage `json:"context"`
			ContextDigest string          `json:"context_digest"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil || string(in.Context) != string(expected) {
			t.Error("original context changed")
		}
		hash := sha256.Sum256(in.Context)
		if in.Operation != "confirm" || in.ContextDigest != hex.EncodeToString(hash[:]) {
			t.Error("invalid context binding")
		}
		_ = json.NewEncoder(w).Encode(Witness{Confirmed: !changed, ContextDigest: in.ContextDigest, ObservedAt: time.Now().UTC()})
	})
	mux.HandleFunc("/provider", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&in)
		if _, ok := in["allowed_input_subjects"]; ok {
			t.Error("input authority reached provider")
		}
		var op string
		_ = json.Unmarshal(in["operation"], &op)
		if op == "issue" {
			issues++
			if len(in["source"]) == 0 || len(in["confirmation"]) == 0 {
				t.Error("missing confirmed source projection")
			}
		}
		if op == "revoke" {
			var id string
			_ = json.Unmarshal(in["request_id"], &id)
			revokeIDs = append(revokeIDs, id)
			_, _ = w.Write([]byte(`{"revoked":true}`))
			return
		}
		_ = json.NewEncoder(w).Encode(grant)
	})
	server := httptest.NewTLSServer(mux)
	defer server.Close()
	c := Client{HTTP: server.Client()}
	cfg := testConfig(server.URL + "/provider")
	cfg.ContextFields = append(cfg.ContextFields, "source")
	cfg.Confirmation = &ConfirmationConfig{Endpoint: server.URL + "/source", SecretEnv: "TEST_PROVIDER_KEY"}
	got, err := c.Issue(t.Context(), cfg, "documents", access)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.RenewConfirmed(t.Context(), cfg, "documents", access, got); err != nil {
		t.Fatal(err)
	}
	changed = true
	if _, err = c.RenewConfirmed(t.Context(), cfg, "documents", access, got); err == nil {
		t.Fatal("renewed changed source audience")
	}
	if issues != 1 || confirms != 3 {
		t.Fatal("wrong confirmation lifecycle", issues, confirms)
	}
	for range 2 {
		if err = c.Revoke(t.Context(), cfg, "documents", access.InvocationID, got); err != nil {
			t.Fatal(err)
		}
	}
	if len(revokeIDs) != 2 || revokeIDs[0] != revokeIDs[1] {
		t.Fatal("unstable revoke id")
	}
	cfg.Confirmation = nil
	if _, err = c.Issue(t.Context(), cfg, "documents", access); err == nil {
		t.Fatal("source accepted without authority")
	}
}

func TestConfirmationRejectsInvalidWitness(t *testing.T) {
	t.Setenv("TEST_PROVIDER_KEY", strings.Repeat("k", 32))
	for _, kind := range []string{"denied", "digest", "stale", "future", "missing time", "unknown field", "trailing json", "redirect"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var in struct {
					ContextDigest string `json:"context_digest"`
				}
				_ = json.NewDecoder(r.Body).Decode(&in)
				witness := Witness{Confirmed: true, ContextDigest: in.ContextDigest, ObservedAt: time.Now().UTC()}
				switch kind {
				case "denied":
					witness.Confirmed = false
				case "digest":
					witness.ContextDigest = "other"
				case "stale":
					witness.ObservedAt = time.Now().Add(-31 * time.Second)
				case "future":
					witness.ObservedAt = time.Now().Add(6 * time.Second)
				case "missing time":
					witness.ObservedAt = time.Time{}
				case "redirect":
					w.Header().Set("Location", "https://example.invalid")
					w.WriteHeader(302)
					return
				}
				raw, _ := json.Marshal(witness)
				if kind == "unknown field" {
					raw = append(raw[:len(raw)-1], []byte(`,"secret":"hidden"}`)...)
				}
				_, _ = w.Write(raw)
				if kind == "trailing json" {
					_, _ = w.Write([]byte(` {}`))
				}
			}))
			defer server.Close()
			cfg := testConfig(server.URL)
			cfg.Confirmation = &ConfirmationConfig{Endpoint: server.URL, SecretEnv: "TEST_PROVIDER_KEY"}
			if err := (Client{HTTP: server.Client()}).Confirm(t.Context(), cfg, sourceAccess()); err == nil {
				t.Fatal("invalid witness accepted")
			}
		})
	}
}
