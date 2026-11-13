//go:build integration

package store

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/templates"
	"github.com/orpheus-agents/orpheus/internal/testutil"
)

func catalogTemplate(t *testing.T, s *Store, name string, active bool) templates.Template {
	t.Helper()
	out, err := s.CreateTemplate(t.Context(), templates.Input{TemplateName: name, Active: active}, name)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTemplateCRUD(t *testing.T) {
	s := &Store{Pool: testutil.Database(t)}
	a := catalogTemplate(t, s, "my-template", true)
	for _, name := range []string{"my-template", "team/my-template"} {
		_, err := s.CreateTemplate(t.Context(), templates.Input{TemplateName: name, Active: false}, "my-template")
		failure, ok := errors.AsType[*session.APIError](err)
		if !ok || failure.Status != 409 {
			t.Fatal(err)
		}
	}
	// Explicit default and omitted tag remain different records.
	tagged, err := s.CreateTemplate(t.Context(), templates.Input{TemplateName: "my-template", Tag: new("default"), Active: true}, "my-template")
	if err != nil || tagged.Name != "my-template:default" {
		t.Fatal(tagged, err)
	}
	changed, err := s.UpdateTemplate(t.Context(), a.ID, templates.Patch{Active: new(false), SetDescription: true, Description: new("description")})
	if err != nil || changed.Active || *changed.Description != "description" {
		t.Fatal(changed, err)
	}
	changed, err = s.UpdateTemplate(t.Context(), a.ID, templates.Patch{SetDescription: true})
	if err != nil || changed.Active || changed.Description != nil {
		t.Fatal(changed, err)
	}
	_, err = s.CreateTemplate(t.Context(), templates.Input{TemplateName: "team/my-template"}, "my-template")
	if err == nil {
		t.Fatal("inactive identity allowed duplicate")
	}
	if err := s.DeleteTemplate(t.Context(), a.ID); err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{s.DeleteTemplate(t.Context(), a.ID), func() error { _, e := s.Template(t.Context(), a.ID); return e }()} {
		failure, ok := errors.AsType[*session.APIError](err)
		if !ok || failure.Status != 404 {
			t.Fatal(err)
		}
	}
	catalogTemplate(t, s, "my-template", true)
	id := uuid.New()
	_, err = s.Pool.Exec(t.Context(), `INSERT INTO templates (id,template_name,agentbox_template_id) VALUES ($1,'db-default','db-id')`, id)
	if err != nil {
		t.Fatal(err)
	}
	def, err := s.Template(t.Context(), id)
	if err != nil || !def.Active {
		t.Fatal(def, err)
	}
}

func TestTemplatePagingAndFilters(t *testing.T) {
	s := &Store{Pool: testutil.Database(t)}
	for n := range 207 {
		catalogTemplate(t, s, fmt.Sprintf("name_%03d", n), n%2 == 0)
	}
	first, err := s.Templates(t.Context(), templates.Filter{}, 200, "")
	if err != nil || len(first.Items) != 200 || first.PrevCursor != nil || first.NextCursor == nil {
		t.Fatal(first, err)
	}
	last, err := s.Templates(t.Context(), templates.Filter{}, 200, *first.NextCursor)
	if err != nil || len(last.Items) != 7 || last.Items[0].Name != "name_200" || last.PrevCursor == nil || last.NextCursor != nil {
		t.Fatal(last, err)
	}
	back, err := s.Templates(t.Context(), templates.Filter{}, 200, *last.PrevCursor)
	if err != nil || len(back.Items) != 200 || back.Items[0].ID != first.Items[0].ID || back.Items[199].ID != first.Items[199].ID {
		t.Fatal(back, err)
	}
	for _, tc := range []struct {
		filter templates.Filter
		want   int
	}{
		{templates.Filter{Active: new(true)}, 104}, {templates.Filter{Active: new(false)}, 103},
		{templates.Filter{Name: new("name_001")}, 1}, {templates.Filter{Name: new("NAME_001")}, 0},
		{templates.Filter{NameContains: new("NAME_00")}, 10}, {templates.Filter{NameContains: new("%")}, 0},
		{templates.Filter{NameContains: new("_")}, 200}, {templates.Filter{NameContains: new(`\`)}, 0},
		{templates.Filter{NameContains: new("Ю")}, 0},
	} {
		page, err := s.Templates(t.Context(), tc.filter, 200, "")
		if err != nil || len(page.Items) != tc.want {
			t.Fatal(tc.filter, len(page.Items), err)
		}
	}
	for _, cursor := range []string{"garbage", *first.NextCursor} {
		_, err := s.Templates(t.Context(), templates.Filter{Active: new(true)}, 200, cursor)
		failure, ok := errors.AsType[*session.APIError](err)
		if !ok || failure.Problem.Code != "invalid_cursor" {
			t.Fatal(err)
		}
	}
	// Removing the next page keeps a previous cursor that includes the boundary row.
	for _, item := range last.Items {
		if err := s.DeleteTemplate(t.Context(), item.ID); err != nil {
			t.Fatal(err)
		}
	}
	empty, err := s.Templates(t.Context(), templates.Filter{}, 200, *first.NextCursor)
	if err != nil || len(empty.Items) != 0 || empty.PrevCursor == nil || empty.NextCursor != nil {
		t.Fatal(empty, err)
	}
	back, err = s.Templates(t.Context(), templates.Filter{}, 200, *empty.PrevCursor)
	if err != nil || len(back.Items) != 200 || back.Items[199].ID != first.Items[199].ID {
		t.Fatal(back, err)
	}
}

func TestTemplatePagingChangesBeforePage(t *testing.T) {
	s := &Store{Pool: testutil.Database(t)}
	a := catalogTemplate(t, s, "b", true)
	b := catalogTemplate(t, s, "c", true)
	page, err := s.Templates(t.Context(), templates.Filter{}, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.Templates(t.Context(), templates.Filter{}, 1, *page.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTemplate(t.Context(), a.ID); err != nil {
		t.Fatal(err)
	}
	empty, err := s.Templates(t.Context(), templates.Filter{}, 1, *next.PrevCursor)
	if err != nil || len(empty.Items) != 0 || empty.NextCursor == nil || empty.PrevCursor != nil {
		t.Fatal(empty, err)
	}
	back, err := s.Templates(t.Context(), templates.Filter{}, 1, *empty.NextCursor)
	if err != nil || len(back.Items) != 1 || back.Items[0].ID != b.ID {
		t.Fatal(back, err)
	}
	catalogTemplate(t, s, "a", true)
	repeated, err := s.Templates(t.Context(), templates.Filter{}, 1, *page.NextCursor)
	if err != nil || repeated.PrevCursor == nil || repeated.Items[0].ID != b.ID {
		t.Fatal(repeated, err)
	}
}

func TestTemplateLiteralSearchAndConcurrentIdentity(t *testing.T) {
	s := &Store{Pool: testutil.Database(t)}
	catalogTemplate(t, s, "alpha_beta", true)
	catalogTemplate(t, s, "alphaxbeta", true)
	for term, count := range map[string]int{"_": 1, "%": 0, `\`: 0, "ALPHA_B": 1} {
		page, err := s.Templates(t.Context(), templates.Filter{NameContains: &term}, 50, "")
		if err != nil || len(page.Items) != count {
			t.Fatal(term, page, err)
		}
	}
	results := make(chan error, 8)
	for n := range 8 {
		go func() {
			_, err := s.CreateTemplate(t.Context(), templates.Input{TemplateName: fmt.Sprintf("alias-%d", n), Active: n%2 == 0}, "shared-provider-id")
			results <- err
		}()
	}
	successes := 0
	for range 8 {
		err := <-results
		if err == nil {
			successes++
			continue
		}
		failure, ok := errors.AsType[*session.APIError](err)
		if !ok || failure.Status != 409 {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatal(successes)
	}
}
