package session

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func validAccessContext() AccessContext {
	return AccessContext{InvocationID: uuid.New(), Subject: AccessSubject{Namespace: "scheduler", ID: "nightly"}, ScopeContext: AccessScopeContext{}, ContextObservedAt: time.Now()}
}

func TestAccessContextAcceptsGenericPrincipalsAndOpaqueScopes(t *testing.T) {
	for _, test := range []struct {
		name    string
		subject AccessSubject
		scope   string
	}{
		{"cron", AccessSubject{Namespace: "scheduler", ID: "nightly"}, `{"schedule_id":"nightly","repository":{"name":"support","number":9007199254740993}}`},
		{"merge request", AccessSubject{Namespace: "gitlab", ID: "review-bot"}, `{"project_id":123,"merge_request_iid":42,"labels":["review"]}`},
		{"ticket", AccessSubject{Namespace: "helpdesk", ID: "customer"}, `{"ticket_id":9007199254740993,"queue":"support","private":true}`},
		{"empty scope", AccessSubject{Namespace: "service", ID: "worker"}, `{}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			access := validAccessContext()
			access.Subject = test.subject
			if err := json.Unmarshal([]byte(test.scope), &access.ScopeContext); err != nil {
				t.Fatal(err)
			}
			access.AllowedInputSubjects = []AccessSubject{{Namespace: "users", ID: "reviewer"}}
			if err := access.Validate(); err != nil {
				t.Fatal("generic context rejected", err)
			}
			if access.Allows(access.Subject) || !access.Allows(access.AllowedInputSubjects[0]) {
				t.Fatal("initiator and future input authority were conflated")
			}
		})
	}
}

func TestAccessContextValidation(t *testing.T) {
	var missing *AccessContext
	if missing.Validate() == nil {
		t.Fatal("missing context accepted")
	}
	for _, test := range []struct {
		name   string
		change func(*AccessContext)
	}{
		{"missing invocation", func(a *AccessContext) { a.InvocationID = uuid.Nil }},
		{"missing observation", func(a *AccessContext) { a.ContextObservedAt = time.Time{} }},
		{"missing namespace", func(a *AccessContext) { a.Subject.Namespace = "" }},
		{"missing id", func(a *AccessContext) { a.Subject.ID = "" }},
		{"long namespace", func(a *AccessContext) { a.Subject.Namespace = strings.Repeat("x", 129) }},
		{"long id", func(a *AccessContext) { a.Subject.ID = strings.Repeat("x", 129) }},
		{"whitespace id", func(a *AccessContext) { a.Subject.ID = " x" }},
		{"control id", func(a *AccessContext) { a.Subject.ID = "x\ny" }},
		{"missing scope", func(a *AccessContext) { a.ScopeContext = nil }},
		{"invalid json", func(a *AccessContext) { a.ScopeContext["value"] = json.RawMessage(`{broken`) }},
		{"large scope", func(a *AccessContext) {
			a.ScopeContext["value"] = json.RawMessage(`"` + strings.Repeat("x", 1<<20) + `"`)
		}},
		{"too many allowed subjects", func(a *AccessContext) { a.AllowedInputSubjects = make([]AccessSubject, 4097) }},
		{"invalid allowed namespace", func(a *AccessContext) { a.AllowedInputSubjects = []AccessSubject{{ID: "reviewer"}} }},
		{"invalid allowed id", func(a *AccessContext) {
			a.AllowedInputSubjects = []AccessSubject{{Namespace: "users", ID: "bad\x00id"}}
		}},
		{"duplicate allowed subject", func(a *AccessContext) {
			a.AllowedInputSubjects = []AccessSubject{{Namespace: "users", ID: "reviewer"}, {Namespace: "users", ID: "reviewer"}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			access := validAccessContext()
			test.change(&access)
			if access.Validate() == nil {
				t.Fatal("invalid context accepted")
			}
		})
	}
}

func TestAccessContextInputAuthorityIsExplicitAndNamespaced(t *testing.T) {
	access := validAccessContext()
	// A legacy audience field is opaque, including for previously saved contexts.
	access.ScopeContext["audience_subjects"] = json.RawMessage(`["reviewer"]`)
	access.Subject = AccessSubject{Namespace: "users", ID: "reviewer"}
	for _, allowed := range [][]AccessSubject{nil, {}} {
		access.AllowedInputSubjects = allowed
		if err := access.Validate(); err != nil || access.Allows(access.Subject) {
			t.Fatal("empty allowlist should deny every input", err)
		}
	}
	access.AllowedInputSubjects = []AccessSubject{{Namespace: "users", ID: "reviewer"}, {Namespace: "gitlab", ID: "reviewer"}}
	if err := access.Validate(); err != nil {
		t.Fatal("same id in distinct namespaces rejected", err)
	}
	for _, subject := range access.AllowedInputSubjects {
		if !access.Allows(subject) {
			t.Fatal("explicit subject denied")
		}
	}
	if access.Allows(AccessSubject{Namespace: "other", ID: "reviewer"}) || access.Allows(AccessSubject{Namespace: "users", ID: "outsider"}) {
		t.Fatal("unlisted subject accepted")
	}
	var missing *AccessContext
	if missing.Allows(access.Subject) {
		t.Fatal("missing context grants authority")
	}
}
