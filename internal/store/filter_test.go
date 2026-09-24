package store

import (
	"testing"
	"time"
)

func TestCursorScopesOmitDefaultAndIrrelevantSessionFilters(t *testing.T) {
	const oldSessionScope = `{"Resource":"sessions","Filter":{"namespace":null,"external_key":null,"input_fingerprint":null,"status":null,"order":"asc"}}`
	const oldRunScope = `{"Resource":"runs","Filter":{"namespace":null,"external_key":null,"input_fingerprint":null,"status":null,"order":"asc"}}`
	for _, tc := range []struct {
		resource string
		filter   ListFilter
		want     string
	}{
		{"sessions", ListFilter{}, oldSessionScope},
		{"sessions", ListFilter{Activity: "all", Sort: "created_at", Order: "asc"}, oldSessionScope},
		{"runs", ListFilter{Activity: "active", Sort: "last_run_created_at", LastRunCreatedFrom: new(time.Now()), LastRunCreatedTo: new(time.Now().Add(time.Hour))}, oldRunScope},
	} {
		got, err := tc.filter.scope(tc.resource)
		if err != nil || got != tc.want {
			t.Fatalf("%s: scope %q, want %q; error %v", tc.resource, got, tc.want, err)
		}
	}
	active, err := (ListFilter{Activity: "active"}).scope("sessions")
	if err != nil || active == oldSessionScope {
		t.Fatalf("activity does not affect cursor scope: %q, %v", active, err)
	}
}
