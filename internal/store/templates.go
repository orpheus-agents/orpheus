package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store/db"
	"github.com/orpheus-agents/orpheus/internal/templates"
)

func templateView(row db.Template) templates.Template {
	return templates.Template{ID: row.ID, Name: row.Name, TemplateName: row.TemplateName, Tag: row.Tag, Description: row.Description, Active: row.Active}
}

func templateError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return session.Problem(404, "template_not_found", "Template not found.")
	}
	if failure, ok := errors.AsType[*pgconn.PgError](err); ok && failure.Code == "23505" {
		return session.Problem(409, "template_exists", "This template and tag are already registered.")
	}
	return err
}

func (s *Store) CreateTemplate(ctx context.Context, in templates.Input, providerID string) (templates.Template, error) {
	in, err := templates.Normalize(in)
	if err != nil {
		return templates.Template{}, err
	}
	if providerID == "" {
		return templates.Template{}, errors.New("missing verified AgentBox template ID")
	}
	row, err := db.New(s.Pool).CreateTemplate(ctx, db.CreateTemplateParams{ID: uuid.New(), TemplateName: in.TemplateName, Tag: in.Tag, AgentboxTemplateID: providerID, Description: in.Description, Active: in.Active})
	return templateView(row), templateError(err)
}

func (s *Store) Template(ctx context.Context, id uuid.UUID) (templates.Template, error) {
	row, err := db.New(s.Pool).GetTemplate(ctx, id)
	return templateView(row), templateError(err)
}

func (s *Store) UpdateTemplate(ctx context.Context, id uuid.UUID, patch templates.Patch) (templates.Template, error) {
	row, err := db.New(s.Pool).UpdateTemplate(ctx, db.UpdateTemplateParams{ID: id, Active: patch.Active, Description: patch.Description, SetDescription: patch.SetDescription})
	return templateView(row), templateError(err)
}

func (s *Store) DeleteTemplate(ctx context.Context, id uuid.UUID) error {
	n, err := db.New(s.Pool).DeleteTemplate(ctx, id)
	if err == nil && n == 0 {
		err = pgx.ErrNoRows
	}
	return templateError(err)
}

type templatePosition struct {
	Name      string
	ID        uuid.UUID
	Direction string
	Inclusive bool
}

func (s *Store) Templates(ctx context.Context, filter templates.Filter, limit int, cursor string) (templates.Page, error) {
	out := templates.Page{Items: []templates.Template{}}
	if limit < 1 || limit > 200 {
		return out, templates.Invalid("query", "limit", "invalid_value", "Page size must be between 1 and 200.")
	}
	if err := filter.Validate(); err != nil {
		return out, err
	}
	if filter.NameContains != nil {
		filter.NameContains = new(strings.ToLower(*filter.NameContains))
	}
	raw, _ := json.Marshal(filter)
	scope := fmt.Sprintf("templates:%x", sha256.Sum256(raw))
	var pos templatePosition
	if cursor != "" {
		if err := decodeCursor(cursor, scope, &pos); err != nil || pos.Name == "" || strings.ContainsRune(pos.Name, 0) || pos.ID == uuid.Nil || (pos.Direction != "next" && pos.Direction != "prev") {
			err := session.Problem(422, "invalid_cursor", "Invalid template page cursor.")
			err.Problem.Details = []session.Detail{{Path: []any{"query", "cursor"}, Code: "invalid_value"}}
			return out, err
		}
	}
	var pattern *string
	if filter.NameContains != nil {
		escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(*filter.NameContains)
		pattern = new("%" + escaped + "%")
	}
	err := s.Snapshot(ctx, func(tx pgx.Tx) error {
		q := db.New(tx)
		rows, err := q.ListTemplates(ctx, db.ListTemplatesParams{Active: filter.Active, ExactName: filter.Name, NamePattern: pattern, FirstPage: cursor == "", Backward: pos.Direction == "prev", PositionName: pos.Name, PositionID: pos.ID, Inclusive: pos.Inclusive, PageLimit: int32(limit)})
		if err != nil {
			return err
		}
		if pos.Direction == "prev" {
			slices.Reverse(rows)
		}
		for _, row := range rows {
			out.Items = append(out.Items, templateView(row))
		}
		if len(rows) == 0 && cursor == "" {
			return nil
		}
		for _, direction := range []string{"prev", "next"} {
			boundary := templatePosition{Name: pos.Name, ID: pos.ID, Direction: direction, Inclusive: direction != pos.Direction}
			if len(rows) > 0 {
				row := rows[0]
				if direction == "next" {
					row = rows[len(rows)-1]
				}
				boundary.Name, boundary.ID, boundary.Inclusive = row.Name, row.ID, false
			}
			exists, err := q.TemplatePageBoundary(ctx, db.TemplatePageBoundaryParams{Active: filter.Active, ExactName: filter.Name, NamePattern: pattern, PositionName: boundary.Name, PositionID: boundary.ID, Backward: direction == "prev", Inclusive: boundary.Inclusive})
			if err != nil {
				return err
			}
			if exists {
				encoded := encodeCursor(scope, boundary)
				if direction == "prev" {
					out.PrevCursor = &encoded
				} else {
					out.NextCursor = &encoded
				}
			}
		}
		return nil
	})
	return out, err
}
