//go:build integration

package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/client"
	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
)

// Exercise the public generated client against the real HTTP boundary and store,
// so a spec that generates compilable but incompatible wire types fails CI.
func TestGeneratedClientAgainstServer(t *testing.T) {
	server, s := testServer(t)
	profile := s.Profiles.Profiles["default"]
	profile.Codex.Effort = "high"
	profile.Codex.Summary = "auto"
	profile.Codex.Personality = "friendly"
	profile.Codex.ServiceTier = "default"
	s.Profiles.Profiles["default"] = profile
	c, err := client.NewClientWithResponses(server.URL, client.WithHTTPClient(server.Client()), client.WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer key")
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range []func() (int, error){
		func() (int, error) {
			res, err := c.HealthWithResponse(t.Context())
			if err != nil {
				return 0, err
			}
			return res.StatusCode(), nil
		},
		func() (int, error) {
			res, err := c.ReadyWithResponse(t.Context())
			if err != nil {
				return 0, err
			}
			return res.StatusCode(), nil
		},
	} {
		if status, err := call(); err != nil || status != 200 {
			t.Fatalf("health: %d %v", status, err)
		}
	}
	params := &client.CreateSessionParams{IdempotencyKey: new(uuid.NewString())}
	body := client.CreateSession{
		Namespace: new("connector/test"), ExternalKey: new("source:thread"), InputFingerprint: new("revision:1"),
		Configuration: client.ConfigurationInput{Agent: client.AgentInput{Profile: "default"}, Sandbox: client.SandboxInput{Template: "codex"}, Hooks: &client.HooksInput{BeforeRun: new("#!/bin/sh\ntrue\n"), AfterRun: new("#!/bin/sh\ntrue\n")}},
		Messages:      []client.TextMessage{{Text: "hello", ExternalKey: new("post:1"), Metadata: new(json.RawMessage(`{"big":9007199254740993}`))}}, Env: &map[string]string{"RUN_INPUT": "fixture"},
	}
	created, err := c.CreateSessionWithResponse(t.Context(), params, body)
	if err != nil {
		t.Fatal(err)
	}
	if created.JSON202 == nil || created.Headers202 == nil {
		t.Fatalf("create: %d %s", created.StatusCode(), created.Body)
	}
	a := *created.JSON202
	// The worker must retain the accepted options even after deployment profiles change.
	profile.Codex = config.CodexProfile{}
	s.Profiles.Profiles["default"] = profile
	record, err := store.GetSession(t.Context(), s.Pool, a.SessionID, false)
	wantCodex := session.CodexConfiguration{Effort: "high", Summary: "auto", Personality: "friendly", ServiceTier: "default"}
	if err != nil || record.Configuration.Public.Agent.Codex != wantCodex {
		t.Fatal("Codex options did not survive persistence", record.Configuration, err)
	}
	replay, err := c.CreateSessionWithResponse(t.Context(), params, body)
	if err != nil || replay.JSON202 == nil || *replay.JSON202 != a {
		t.Fatalf("idempotent replay: %#v %v", replay, err)
	}
	body.Messages[0].Text = "different input"
	conflict, err := c.CreateSessionWithResponse(t.Context(), params, body)
	if err != nil || conflict.JSON409 == nil || conflict.JSON409.Error.Code != "idempotency_conflict" {
		t.Fatalf("conflict: %#v %v", conflict, err)
	}

	got, err := c.GetSessionWithResponse(t.Context(), a.SessionID)
	if err != nil || got.JSON200 == nil || got.JSON200.ID != a.SessionID || got.JSON200.Configuration.Agent.Instructions != "profile instruction" {
		t.Fatalf("session: %#v %v", got, err)
	}
	if got.JSON200.LastRunCreatedAt.IsZero() {
		t.Fatal("session lacks latest run creation time")
	}
	for _, key := range []string{"codex", "effort", "summary", "personality", "service_tier"} {
		if bytes.Contains(got.Body, []byte(`"`+key+`":`)) {
			t.Fatalf("internal Codex option leaked into API: %s", got.Body)
		}
	}
	listed, err := c.ListSessionsWithResponse(t.Context(), &client.ListSessionsParams{Namespace: body.Namespace, ExternalKey: body.ExternalKey})
	if err != nil || listed.JSON200 == nil || len(listed.JSON200.Items) != 1 || listed.JSON200.Items[0].ID != a.SessionID {
		t.Fatalf("sessions: %#v %v", listed, err)
	}
	active := client.ListSessionsParams{Activity: new(client.ListSessionsParamsActivity("active")), Sort: new(client.ListSessionsParamsSort("last_run_created_at")), Order: new(client.ListSessionsParamsOrder("desc"))}
	listedActive, err := c.ListSessionsWithResponse(t.Context(), &active)
	if err != nil || listedActive.JSON200 == nil || len(listedActive.JSON200.Items) != 1 || listedActive.JSON200.Items[0].LastRunCreatedAt.IsZero() {
		t.Fatalf("active sessions: %#v %v", listedActive, err)
	}
	analytics, err := c.GetAnalyticsOverviewWithResponse(t.Context(), &client.GetAnalyticsOverviewParams{Namespace: body.Namespace})
	if err != nil || analytics.JSON200 == nil || analytics.JSON200.Period.RunsCount != 1 || analytics.JSON200.Current.ActiveSessions != 1 {
		t.Fatalf("analytics: %#v %v", analytics, err)
	}
	runs, err := c.ListRunsWithResponse(t.Context(), a.SessionID, &client.ListRunsParams{InputFingerprint: body.InputFingerprint})
	if err != nil || runs.JSON200 == nil || len(runs.JSON200.Items) != 1 || runs.JSON200.Items[0].ID != a.RunID {
		t.Fatalf("runs: %#v %v", runs, err)
	}
	all, err := c.ListAllRunsWithResponse(t.Context(), &client.ListAllRunsParams{Namespace: body.Namespace, ExternalKey: body.ExternalKey})
	if err != nil || all.JSON200 == nil || len(all.JSON200.Items) != 1 {
		t.Fatalf("all runs: %#v %v", all, err)
	}
	run, err := c.GetRunWithResponse(t.Context(), a.SessionID, a.RunID)
	if err != nil || run.JSON200 == nil || run.JSON200.AgentStatus != nil || run.JSON200.Status != client.RunStatusAccepted || len(run.JSON200.EnvNames) != 1 || run.JSON200.EnvNames[0] != "RUN_INPUT" {
		t.Fatalf("run: %#v %v", run, err)
	}
	history, err := c.GetHistoryWithResponse(t.Context(), a.SessionID, &client.GetHistoryParams{MessageExternalKey: new("post:1"), RunID: &a.RunID})
	if err != nil || history.JSON200 == nil || len(history.JSON200.Items) != 1 {
		t.Fatalf("history: %#v %v", history, err)
	}
	item, err := history.JSON200.Items[0].AsMessageItem()
	if err != nil || item.Message.ID != a.MessageID || item.Message.Text != "hello" || item.Message.Metadata == nil || string(*item.Message.Metadata) != `{"big":9007199254740993}` {
		t.Fatalf("message union: %#v %v", item, err)
	}
	events, err := c.GetEventsWithResponse(t.Context(), a.SessionID, &client.GetEventsParams{After: new("0"), Limit: new(1)})
	if err != nil || events.JSON200 == nil || len(events.JSON200.Items) != 1 || !events.JSON200.HasMore || events.JSON200.NextCursor == "0" {
		t.Fatalf("events: %#v %v", events, err)
	}
	clarification, err := c.SendMessageWithResponse(t.Context(), a.SessionID, a.RunID, &client.SendMessageParams{IdempotencyKey: new(uuid.NewString())}, client.SendMessage{Messages: []client.TextMessage{{Text: "next"}}})
	if err != nil || clarification.JSON409 == nil {
		t.Fatalf("message to accepted run: %#v %v", clarification, err)
	}
	cancelled, err := c.CancelRunWithResponse(t.Context(), a.SessionID, a.RunID)
	if err != nil || cancelled.JSON200 == nil || cancelled.JSON200.RunID != a.RunID {
		t.Fatalf("cancel: %#v %v", cancelled, err)
	}
}
