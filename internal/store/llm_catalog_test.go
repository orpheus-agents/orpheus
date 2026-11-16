//go:build integration

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/llm"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

func catalogAccount(t *testing.T, s *Store, name, key string) llm.AccountDetails {
	t.Helper()
	a, err := s.CreateAccount(t.Context(), llm.AccountInput{Name: name, Harness: "codex", AuthMode: "api_key", APIKey: &key})
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func catalogProfile(t *testing.T, s *Store, name string, account uuid.UUID) llm.Profile {
	t.Helper()
	p, err := s.CreateProfile(t.Context(), llm.ProfileInput{Name: name, AccountID: account, Model: "model", Instructions: "original", Codex: map[string]string{"effort": "high", "summary": "auto"}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func TestLLMAccountSecretsAndDeletion(t *testing.T) {
	s := fixture(t)
	ctx := t.Context()
	a := catalogAccount(t, s, "Account", "  private-original-key  ")
	if !a.Active || a.APIKeyMask == nil || *a.APIKeyMask != "priv...-key" {
		t.Fatal(a)
	}
	catalogAccount(t, s, "Account", "different") // Display names need not be unique.
	row, err := db.New(s.Pool).ReadLLMAccount(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	key, err := s.accountKey(ctx, s.Pool, row)
	if err != nil || key != "  private-original-key  " {
		t.Fatal("key was changed", err)
	}
	other := row
	other.ID = uuid.New()
	if _, err := s.accountKey(ctx, s.Pool, other); err == nil {
		t.Fatal("account ownership not authenticated")
	}
	if err := s.ReplaceAccountKey(ctx, a.ID, "replacement-key"); err != nil {
		t.Fatal(err)
	}
	key, err = s.accountKey(ctx, s.Pool, row)
	if err != nil || key != "replacement-key" {
		t.Fatal("replacement not visible", err)
	}
	p := catalogProfile(t, s, "attached", a.ID)
	requireCode(t, s.DeleteAccount(ctx, a.ID), "account_in_use")
	if err := s.DeleteProfile(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAccount(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.New(s.Pool).GetSecret(ctx, *row.ApiKeySecretID); err == nil {
		t.Fatal("deleted account left an encrypted key")
	}
	requireCode(t, s.DeleteAccount(ctx, a.ID), "account_not_found")
}
func TestLLMRebindFreezeFlagsAndReplay(t *testing.T) {
	s := fixture(t)
	ctx := t.Context()
	s.Settings.MaxConcurrentSessions = 10
	a := catalogAccount(t, s, "One", "one-key")
	b := catalogAccount(t, s, "Two", "two-key")
	p := catalogProfile(t, s, "balanced", a.ID)
	req := request()
	req.Create.Configuration.Agent.Profile = p.Name
	first, err := s.Accept(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	p, err = s.UpdateProfile(ctx, p.ID, []byte(fmt.Sprintf(`{"account_id":%q,"model":"next-model","codex":{"effort":null,"personality":"friendly"}}`, b.ID)))
	if err != nil || p.Codex["summary"] != "auto" || p.Codex["personality"] != "friendly" {
		t.Fatal(p, err)
	}
	if _, ok := p.Codex["effort"]; ok {
		t.Fatal("option was not cleared")
	}
	nextReq := req
	nextReq.Key = uuid.New()
	second, err := s.Accept(ctx, nextReq)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id, account uuid.UUID
		model, key  string
	}{{first.SessionID, a.ID, "model", "one-key"}, {second.SessionID, b.ID, "next-model", "two-key"}} {
		record, err := GetSession(ctx, s.Pool, tc.id, false)
		if err != nil || record.LlmAccountID == nil || *record.LlmAccountID != tc.account || record.Configuration.Public.Agent.Model != tc.model {
			t.Fatal(record, err)
		}
		raw, _ := json.Marshal(record.Configuration)
		if strings.Contains(string(raw), "credentials") || strings.Contains(string(raw), "one-key") {
			t.Fatal("credentials retained in session")
		}
		source, err := s.SessionCredentials(ctx, record)
		if err != nil || source.APIKey != tc.key {
			t.Fatal("session account changed", err)
		}
	}
	if _, err := s.UpdateAccount(ctx, a.ID, []byte(`{"active":false}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateAccount(ctx, b.ID, []byte(`{"active":false}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Accept(ctx, requestForProfile(p.Name)); err == nil {
		t.Fatal("inactive account admitted")
	}
	if _, err := s.UpdateAccount(ctx, b.ID, []byte(`{"active":true}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateProfile(ctx, p.ID, []byte(`{"active":false}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Accept(ctx, requestForProfile(p.Name)); err == nil {
		t.Fatal("inactive profile admitted")
	}
	if err := s.DeleteProfile(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	requireCode(t, s.DeleteAccount(ctx, a.ID), "account_in_use")
	replay, err := s.Accept(ctx, req)
	if err != nil || replay != first {
		t.Fatal(replay, err)
	}
	if _, err := s.Cancel(ctx, first.SessionID, first.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Accept(ctx, Admission{SessionID: first.SessionID, Key: uuid.New(), Messages: []session.TextMessage{{Text: "continue"}}}); err != nil {
		t.Fatal(err)
	}
}
func requestForProfile(name string) Admission {
	req := request()
	req.Create.Configuration.Agent.Profile = name
	return req
}
func TestLLMCatalogPagingFiltersAndMissingAnchor(t *testing.T) {
	s := fixture(t)
	ctx := t.Context()
	for i := range 12 {
		a := catalogAccount(t, s, fmt.Sprintf("acc_%02d", i/2), "private-key")
		p := catalogProfile(t, s, fmt.Sprintf("prof_%02d", i), a.ID)
		if i%2 == 0 {
			if _, err := s.UpdateProfile(ctx, p.ID, []byte(`{"active":false}`)); err != nil {
				t.Fatal(err)
			}
		}
	}
	first, err := s.Accounts(ctx, llm.Filter{NameContains: new("acc_")}, 5, "")
	if err != nil || len(first.Items) != 5 || first.NextCursor == nil || first.PrevCursor != nil {
		t.Fatal(first, err)
	}
	last, err := s.Accounts(ctx, llm.Filter{NameContains: new("acc_")}, 20, *first.NextCursor)
	if err != nil || len(last.Items) != 7 || last.PrevCursor == nil || last.NextCursor != nil {
		t.Fatal(last, err)
	}
	back, err := s.Accounts(ctx, llm.Filter{NameContains: new("acc_")}, 5, *last.PrevCursor)
	if err != nil || len(back.Items) != 5 || back.Items[4].ID != first.Items[4].ID {
		t.Fatal(back, err)
	}
	if _, err := s.Accounts(ctx, llm.Filter{}, 5, *first.NextCursor); err == nil {
		t.Fatal("cursor used with other filters")
	}
	profiles, err := s.ModelProfiles(ctx, llm.Filter{Available: new(true), NameContains: new("prof_")}, 5, "")
	if err != nil || len(profiles.Items) != 5 || profiles.NextCursor == nil {
		t.Fatal(profiles, err)
	}
	anchor := profiles.Items[4]
	if err := s.DeleteProfile(ctx, anchor.ID); err != nil {
		t.Fatal(err)
	}
	rest, err := s.ModelProfiles(ctx, llm.Filter{Available: new(true), NameContains: new("prof_")}, 5, *profiles.NextCursor)
	if err != nil || len(rest.Items) != 1 || rest.Items[0].Name != "prof_11" {
		t.Fatal(rest, err)
	}
	exact, err := s.ModelProfiles(ctx, llm.Filter{Name: new("prof_11")}, 1, "")
	if err != nil || len(exact.Items) != 1 {
		t.Fatal(exact, err)
	}
	literal, err := s.ModelProfiles(ctx, llm.Filter{NameContains: new("%")}, 50, "")
	if err != nil || len(literal.Items) != 0 {
		t.Fatal(literal, err)
	}
}

func TestLLMAdmissionSerializesAccountAndProfileChanges(t *testing.T) {
	for _, kind := range []string{"account", "profile"} {
		for _, admissionFirst := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/admission_first=%t", kind, admissionFirst), func(t *testing.T) {
				s := fixture(t)
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				a := catalogAccount(t, s, "Account", "private-key")
				p := catalogProfile(t, s, "concurrent", a.ID)
				barrier, err := s.Pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = barrier.Rollback(context.Background()) }()
				mutation := fmt.Sprintf("UPDATE llm_accounts SET active=false WHERE id='%s'", a.ID)
				blockedQuery := "ShareLLMAccount"
				if kind == "profile" {
					mutation = fmt.Sprintf("UPDATE model_profiles SET active=false WHERE id='%s'", p.ID)
					blockedQuery = "ModelProfileForAdmission"
				}
				query := mutation
				if admissionFirst {
					query = "LOCK TABLE sessions IN SHARE MODE"
					blockedQuery = "CreateSession"
				}
				if _, err := barrier.Exec(ctx, query); err != nil {
					t.Fatal(err)
				}
				req := requestForProfile(p.Name)
				admitted := make(chan error, 1)
				go func() { _, err := s.Accept(ctx, req); admitted <- err }()
				pid := waitForTemplateBlock(t, s, barrier.Conn().PgConn().PID(), blockedQuery)
				changed := make(chan error, 1)
				if admissionFirst {
					go func() { _, err := s.Pool.Exec(ctx, mutation); changed <- err }()
					waitForTemplateBlock(t, s, pid, mutation)
				}
				if err := barrier.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				err = <-admitted
				if admissionFirst {
					if err != nil {
						t.Fatal(err)
					}
					if err := <-changed; err != nil {
						t.Fatal(err)
					}
					if _, err := s.Accept(ctx, req); err != nil {
						t.Fatal("replay failed", err)
					}
				} else {
					requireCode(t, err, "unknown_profile")
				}
				_, err = s.Accept(ctx, requestForProfile(p.Name))
				requireCode(t, err, "unknown_profile")
			})
		}
	}
}

func TestLLMAccountDeletionDoesNotDeadlockProfileEdit(t *testing.T) {
	s := fixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	a := catalogAccount(t, s, "Account", "key")
	p := catalogProfile(t, s, "edited", a.ID)
	edit, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = edit.Rollback(context.Background()) }()
	if _, err := db.New(edit).LockModelProfile(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	// A profile edit holds this lock before checking the selected account. Delete
	// must reject its existing reference without entering a conflicting FK lock.
	requireCode(t, s.DeleteAccount(ctx, a.ID), "account_in_use")
	if _, err := db.New(edit).ShareLLMAccount(ctx, a.ID); err != nil {
		t.Fatal("account deletion blocked the edit", err)
	}
	if err := edit.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// The opposite order also rejects deletion without consuming the record.
	requireCode(t, s.DeleteAccount(ctx, a.ID), "account_in_use")
	if _, err := s.UpdateProfile(ctx, p.ID, []byte(`{"model":"changed"}`)); err != nil {
		t.Fatal(err)
	}
}
