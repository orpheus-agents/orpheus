package session

import (
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// AccessSubject identifies a human or service principal in a caller's namespace.
type AccessSubject struct {
	Namespace string `json:"namespace"`
	ID        string `json:"id"`
}

// AccessScopeContext is a provider-owned JSON object. Orpheus neither interprets
// its keys nor derives input permissions from it. Raw values preserve numbers.
type AccessScopeContext = map[string]json.RawMessage

type AccessContext struct {
	InvocationID         uuid.UUID          `json:"invocation_id"`
	Subject              AccessSubject      `json:"subject"`
	ScopeContext         AccessScopeContext `json:"scope_context"`
	ContextObservedAt    time.Time          `json:"context_observed_at"`
	AllowedInputSubjects []AccessSubject    `json:"allowed_input_subjects,omitzero"`
}

func (a *AccessContext) Validate() error {
	valid := func(s string) bool {
		return len(s) > 0 && len(s) <= 128 && strings.TrimSpace(s) == s && !strings.ContainsAny(s, "\x00\r\n")
	}
	validSubject := func(subject AccessSubject) bool {
		return valid(subject.Namespace) && valid(subject.ID)
	}
	if a == nil || a.InvocationID == uuid.Nil || a.ContextObservedAt.IsZero() {
		return Problem(422, "access_context_required", "A trusted access context is required.")
	}
	if !validSubject(a.Subject) || a.ScopeContext == nil || len(a.AllowedInputSubjects) > 4096 {
		return Problem(422, "invalid_access_context", "Invalid access context.")
	}
	scope, err := json.Marshal(a.ScopeContext)
	if err != nil || len(scope) > 1<<20 {
		return Problem(422, "invalid_access_context", "Scope context must be a JSON object of at most 1 MiB.")
	}
	seen := map[AccessSubject]bool{}
	for _, subject := range a.AllowedInputSubjects {
		if !validSubject(subject) || seen[subject] {
			return Problem(422, "invalid_access_context", "Invalid allowed input subjects.")
		}
		seen[subject] = true
	}
	return nil
}

func (a *AccessContext) Allows(subject AccessSubject) bool {
	return a != nil && slices.Contains(a.AllowedInputSubjects, subject)
}
