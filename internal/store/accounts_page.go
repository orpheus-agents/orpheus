package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/llm"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store/db"
	"github.com/orpheus-agents/orpheus/internal/templates"
)

func (s *Store) Accounts(ctx context.Context, filter llm.Filter, limit int, cursor string) (llm.Page[llm.Account], error) {
	out := llm.Page[llm.Account]{Items: []llm.Account{}}
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
	scope := fmt.Sprintf("accounts:%x", sha256.Sum256(raw))
	var pos catalogPosition
	if cursor != "" {
		if err := decodeCursor(cursor, scope, &pos); err != nil || pos.Name == "" || strings.ContainsRune(pos.Name, 0) || pos.ID == uuid.Nil || (pos.Direction != "next" && pos.Direction != "prev") {
			err := session.Problem(422, "invalid_cursor", "Invalid catalog page cursor.")
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
		rows, err := q.ListLLMAccounts(ctx, db.ListLLMAccountsParams{Active: filter.Active, ExactName: filter.Name, NamePattern: pattern, FirstPage: cursor == "", Backward: pos.Direction == "prev", PositionName: pos.Name, PositionID: pos.ID, Inclusive: pos.Inclusive, PageLimit: int32(limit)})
		if err != nil {
			return err
		}
		if pos.Direction == "prev" {
			slices.Reverse(rows)
		}
		for _, row := range rows {
			out.Items = append(out.Items, accountView(db.GetLLMAccountRow(row)))
		}
		if len(rows) == 0 && cursor == "" {
			return nil
		}
		for _, direction := range []string{"prev", "next"} {
			boundary := catalogPosition{Name: pos.Name, ID: pos.ID, Direction: direction, Inclusive: direction != pos.Direction}
			if len(rows) > 0 {
				row := rows[0]
				if direction == "next" {
					row = rows[len(rows)-1]
				}
				boundary.Name, boundary.ID, boundary.Inclusive = row.Name, row.ID, false
			}
			exists, err := q.LLMAccountsPageBoundary(ctx, db.LLMAccountsPageBoundaryParams{Active: filter.Active, ExactName: filter.Name, NamePattern: pattern, PositionName: boundary.Name, PositionID: boundary.ID, Backward: direction == "prev", Inclusive: boundary.Inclusive})
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
