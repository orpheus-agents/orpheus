package store

import (
	"testing"

	"github.com/orpheus-agents/orpheus/internal/session"
)

func TestSourceContinuationBoundary(t *testing.T) {
	a := &session.SourceContext{Identity: session.AccessSubject{Namespace: "gitlab", ID: "mr-42"}, Revision: "old", Audience: []session.AccessSubject{{Namespace: "services", ID: "reader-a"}, {Namespace: "services", ID: "reader-b"}}}
	b := *a
	b.Revision = "new"
	b.Audience = []session.AccessSubject{a.Audience[1], a.Audience[0]}
	if !sameSourceBoundary(a, &b) || !sameSourceBoundary(nil, nil) {
		t.Fatal("equivalent source boundary changed")
	}
	b.Audience = []session.AccessSubject{a.Audience[0]}
	if sameSourceBoundary(a, &b) || sameSourceBoundary(nil, &b) {
		t.Fatal("changed audience inherited memory")
	}
	b = *a
	b.Identity.ID = "mr-43"
	if sameSourceBoundary(a, &b) {
		t.Fatal("another source inherited memory")
	}
}
