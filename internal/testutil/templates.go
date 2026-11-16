//go:build integration || live

package testutil

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

// RegisterTemplate supplies an active catalog entry without calling AgentBox.
func RegisterTemplate(t *testing.T, pool *pgxpool.Pool, name string) uuid.UUID {
	t.Helper()
	templateName, tag, tagged := strings.Cut(name, ":")
	input := db.CreateTemplateParams{ID: uuid.New(), TemplateName: templateName, AgentboxTemplateID: templateName, Active: true}
	if tagged {
		input.Tag = &tag
	}
	row, err := db.New(pool).CreateTemplate(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	return row.ID
}
