//go:build integration

package httpserver

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skillum-ai/orpheus/internal/api"
	"github.com/skillum-ai/orpheus/internal/session"
	"github.com/skillum-ai/orpheus/internal/store"
)

const externalBody = `{"namespace":"redmine","external_key":"prod:issue:7","input_fingerprint":"v1:456","configuration":{"agent":{"profile":"default"},"sandbox":{"template":"codex"}},"message":{"text":"hello","external_key":"journal:456"}}`

func decodeHTTP[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func externalRequest(t *testing.T, server *httptest.Server, method, path, body, key string, want int) []byte {
	t.Helper()
	code, _, raw := requestHTTP(t, server, method, path, body, "key", key)
	if code != want {
		t.Fatalf("%s %s: want %d got %d: %s", method, path, want, code, raw)
	}
	return raw
}

func TestExternalInputsHTTP(t *testing.T) {
	server, s := testServer(t)
	key := uuid.NewString()
	raw := externalRequest(t, server, "POST", "/api/v1/sessions", externalBody, key, 202)
	a := decodeHTTP[session.Acceptance](t, raw)
	base := "/api/v1/sessions/" + a.SessionID.String()
	again := externalRequest(t, server, "POST", "/api/v1/sessions", externalBody, key, 202)
	if decodeHTTP[session.Acceptance](t, again) != a {
		t.Fatal("duplicate acceptance")
	}
	for _, pair := range [][2]string{{`"redmine"`, `"different"`}, {`"prod:issue:7"`, `"different"`}, {`"v1:456"`, `"different"`}, {`"journal:456"`, `"different"`}} {
		raw := externalRequest(t, server, "POST", "/api/v1/sessions", strings.Replace(externalBody, pair[0], pair[1], 1), key, 409)
		if !strings.Contains(string(raw), "idempotency_conflict") {
			t.Fatal(string(raw))
		}
	}
	v := decodeHTTP[session.Session](t, externalRequest(t, server, "GET", base, "", "", 200))
	if v.Namespace == nil || *v.Namespace != "redmine" || v.ExternalKey == nil || *v.ExternalKey != "prod:issue:7" {
		t.Fatal(v)
	}
	run := decodeHTTP[session.Run](t, externalRequest(t, server, "GET", base+"/runs/"+a.RunID.String(), "", "", 200))
	if run.InputFingerprint == nil || *run.InputFingerprint != "v1:456" {
		t.Fatal(run)
	}
	page := decodeHTTP[session.Page[session.Run]](t, externalRequest(t, server, "GET", "/api/v1/runs?namespace=redmine&external_key=prod%3Aissue%3A7&input_fingerprint=v1%3A456&status=accepted&order=desc", "", "", 200))
	if len(page.Items) != 1 || page.Items[0].ID != a.RunID {
		t.Fatal(page)
	}
	history := decodeHTTP[session.HistoryPage](t, externalRequest(t, server, "GET", base+"/history?message_external_key=journal%3A456", "", "", 200))
	if len(history.Items) != 1 || *history.Items[0].Message.ExternalKey != "journal:456" {
		t.Fatal(history)
	}
	// A second key is a distinct session, despite identical external bindings.
	other := decodeHTTP[session.Acceptance](t, externalRequest(t, server, "POST", "/api/v1/sessions", externalBody, uuid.NewString(), 202))
	if other.SessionID == a.SessionID {
		t.Fatal("unexpected external-key deduplication")
	}
	sessions := decodeHTTP[session.Page[session.Session]](t, externalRequest(t, server, "GET", "/api/v1/sessions?namespace=redmine&external_key=prod%3Aissue%3A7&status=accepted", "", "", 200))
	if len(sessions.Items) != 2 {
		t.Fatal(sessions)
	}
	// Steering retains the run's original fingerprint and accepts only message metadata.
	err := s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, rec *store.SessionRecord) error {
		r, err := store.GetRun(t.Context(), tx, a.SessionID, a.RunID)
		if err != nil {
			return err
		}
		r.Status = session.Running
		return store.PublishRun(t.Context(), tx, rec, &r)
	})
	if err != nil {
		t.Fatal(err)
	}
	steerPath := base + "/runs/" + a.RunID.String() + "/messages"
	steerBody := `{"message":{"text":"clarify","external_key":"journal:457"}}`
	steerKey := uuid.NewString()
	steer := decodeHTTP[session.Acceptance](t, externalRequest(t, server, "POST", steerPath, steerBody, steerKey, 202))
	if decodeHTTP[session.Acceptance](t, externalRequest(t, server, "POST", steerPath, steerBody, steerKey, 202)) != steer {
		t.Fatal("steer replay")
	}
	externalRequest(t, server, "POST", steerPath, strings.Replace(steerBody, "457", "458", 1), steerKey, 409)
	for _, field := range []string{`"input_fingerprint":"new",`, `"namespace":"new",`, `"external_key":"new",`} {
		externalRequest(t, server, "POST", steerPath, "{"+field+steerBody[1:], uuid.NewString(), 422)
	}
	// Finish the first run and accept an explicitly retried snapshot in the same session.
	err = s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, rec *store.SessionRecord) error {
		r, e := store.GetRun(t.Context(), tx, a.SessionID, a.RunID)
		if e != nil {
			return e
		}
		return store.Finish(t.Context(), tx, rec, &r, session.Completed, nil, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	nextBody := `{"message":{"text":"again","external_key":"journal:456"},"input_fingerprint":"v1:456"}`
	nextKey := uuid.NewString()
	next := decodeHTTP[session.Acceptance](t, externalRequest(t, server, "POST", base+"/runs", nextBody, nextKey, 202))
	if next.RunID == a.RunID {
		t.Fatal("fingerprint suppressed retry")
	}
	externalRequest(t, server, "POST", base+"/runs", nextBody, nextKey, 202)
	externalRequest(t, server, "POST", base+"/runs", strings.Replace(nextBody, "v1:456", "v2", 1), nextKey, 409)
	page = decodeHTTP[session.Page[session.Run]](t, externalRequest(t, server, "GET", base+"/runs?input_fingerprint=v1%3A456&order=desc", "", "", 200))
	if len(page.Items) != 2 || page.Items[0].ID != next.RunID {
		t.Fatal(page)
	}
	events := decodeHTTP[session.EventPage](t, externalRequest(t, server, "GET", base+"/events", "", "", 200))
	for _, event := range events.Items {
		var data map[string]json.RawMessage
		if err := json.Unmarshal(event.Data, &data); err != nil {
			t.Fatal(err)
		}
		switch event.Type {
		case "message.updated":
			if _, ok := data["external_key"]; !ok {
				t.Fatal("missing message metadata")
			}
		case "run.updated":
			if _, ok := data["input_fingerprint"]; !ok {
				t.Fatal("missing run metadata")
			}
		}
	}
}

func TestExternalValidationHTTP(t *testing.T) {
	server, _ := testServer(t)
	for _, field := range []struct {
		name string
		max  int
	}{{"namespace", 128}, {"external_key", 512}, {"input_fingerprint", 256}, {"message.external_key", 512}} {
		t.Run(field.name, func(t *testing.T) {
			for _, value := range []any{nil, 42, "", "\u2003\n", "x\x00", strings.Repeat("é", field.max/2) + "a"} {
				body := decodeHTTP[map[string]any](t, []byte(validBody))
				if field.name == "message.external_key" {
					body["message"].(map[string]any)["external_key"] = value
				} else {
					body[field.name] = value
				}
				raw, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				response := externalRequest(t, server, "POST", "/api/v1/sessions", string(raw), uuid.NewString(), 422)
				if !strings.Contains(string(response), "validation_error") {
					t.Fatal(string(response))
				}
			}
			body := decodeHTTP[map[string]any](t, []byte(validBody))
			exact := strings.Repeat("é", field.max/2)
			if field.name == "message.external_key" {
				body["message"].(map[string]any)["external_key"] = exact
			} else {
				body[field.name] = exact
			}
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			externalRequest(t, server, "POST", "/api/v1/sessions", string(raw), uuid.NewString(), 202)
		})
	}
	a := decodeHTTP[session.Acceptance](t, externalRequest(t, server, "POST", "/api/v1/sessions", validBody, uuid.NewString(), 202))
	for _, target := range []struct {
		path, param string
		max         int
	}{
		{"/api/v1/sessions", "namespace", 128}, {"/api/v1/sessions", "external_key", 512}, {"/api/v1/runs", "namespace", 128}, {"/api/v1/runs", "external_key", 512}, {"/api/v1/runs", "input_fingerprint", 256},
		{"/api/v1/sessions/" + a.SessionID.String() + "/runs", "input_fingerprint", 256},
		{"/api/v1/sessions/" + a.SessionID.String() + "/history", "message_external_key", 512},
	} {
		for _, value := range []string{"", "\u2003", "x\x00", strings.Repeat("é", target.max/2) + "a"} {
			externalRequest(t, server, "GET", target.path+"?"+target.param+"="+url.QueryEscape(value), "", "", 422)
		}
		externalRequest(t, server, "GET", target.path+"?"+target.param+"=a&"+target.param+"=b", "", "", 422)
		externalRequest(t, server, "GET", target.path+"?"+target.param+"=null", "", "", 200)
	}
	for _, query := range []string{"status=bogus", "status=finalizing", "order=bogus", "order=", "status=accepted&status=accepted"} {
		externalRequest(t, server, "GET", "/api/v1/runs?"+query, "", "", 422)
	}
	externalRequest(t, server, "GET", "/api/v1/sessions/"+uuid.NewString()+"/history?message_external_key=x", "", "", 404)
	externalRequest(t, server, "GET", "/api/v1/sessions/"+a.SessionID.String()+"/history?run_id="+uuid.NewString()+"&message_external_key=x", "", "", 404)
	// No metadata means explicit null in responses, not omitted properties.
	raw := externalRequest(t, server, "GET", "/api/v1/sessions/"+a.SessionID.String(), "", "", 200)
	obj := decodeHTTP[map[string]json.RawMessage](t, raw)
	if string(obj["namespace"]) != "null" || string(obj["external_key"]) != "null" {
		t.Fatal(string(raw))
	}
}

func TestExternalOpaqueKeysAndCursorHTTP(t *testing.T) {
	server, _ := testServer(t)
	keys := []string{" key ", "key", "é", "e\u0301"}
	var accepted []session.Acceptance
	for _, key := range keys {
		body := decodeHTTP[map[string]any](t, []byte(externalBody))
		body["external_key"] = key
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		accepted = append(accepted, decodeHTTP[session.Acceptance](t, externalRequest(t, server, "POST", "/api/v1/sessions", string(raw), uuid.NewString(), 202)))
	}
	for i, key := range keys {
		page := decodeHTTP[session.Page[session.Session]](t, externalRequest(t, server, "GET", "/api/v1/sessions?namespace=redmine&external_key="+url.QueryEscape(key), "", "", 200))
		if len(page.Items) != 1 || page.Items[0].ID != accepted[i].SessionID || *page.Items[0].ExternalKey != key {
			t.Fatal(page)
		}
	}
	first := decodeHTTP[session.Page[session.Run]](t, externalRequest(t, server, "GET", "/api/v1/runs?namespace=redmine&limit=1", "", "", 200))
	if first.NextCursor == nil {
		t.Fatal(first)
	}
	cursor := url.QueryEscape(*first.NextCursor)
	page := decodeHTTP[session.Page[session.Run]](t, externalRequest(t, server, "GET", "/api/v1/runs?cursor="+cursor+"&limit=3&order=asc&namespace=redmine", "", "", 200))
	if len(page.Items) != 3 {
		t.Fatal(page)
	}
	externalRequest(t, server, "GET", "/api/v1/runs?cursor="+cursor+"&namespace=redmine&order=desc", "", "", 422)
	externalRequest(t, server, "GET", "/api/v1/runs?cursor="+cursor, "", "", 422)
}

func TestRunStatusContractMatchesDomain(t *testing.T) {
	spec, err := api.GetSpec()
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range spec.Components.Schemas["RunStatus"].Value.Enum {
		if !session.Status(value.(string)).Valid() {
			t.Fatalf("OpenAPI status rejected by domain: %v", value)
		}
	}
	for _, value := range []session.Status{session.Accepted, session.Starting, session.Running, session.Cancelling, session.Completed, session.Failed, session.Cancelled} {
		if !slices.Contains(spec.Components.Schemas["RunStatus"].Value.Enum, any(string(value))) {
			t.Fatalf("domain status missing from OpenAPI: %s", value)
		}
	}
	if session.Status("unknown").Valid() {
		t.Fatal("unknown status accepted")
	}
}
