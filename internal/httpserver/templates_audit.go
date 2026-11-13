package httpserver

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/browserauth"
)

type templateAuditKey struct{}

type templateAudit struct {
	action string
	id     string
	actor  string
	fields []string
}

func newTemplateAudit(operation string, r *http.Request) *templateAudit {
	audit := &templateAudit{action: operation, actor: "unauthenticated", fields: []string{}}
	if id, err := uuid.Parse(strings.TrimPrefix(r.URL.Path, "/api/v1/templates/")); err == nil {
		audit.id = id.String()
	}
	return audit
}

func (a *templateAudit) identify(r *http.Request, mode string, identity *browserauth.Identity, authErr error) {
	switch {
	case identity != nil:
		a.actor = "saml:" + identity.Subject
	case authErr == nil && r.Header.Get("Authorization") != "":
		_, key, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		a.actor = fmt.Sprintf("api_key:%x", sha256.Sum256([]byte(key)))
	case mode == "anonymous" && r.Header.Get("Authorization") == "":
		a.actor = "anonymous"
	}
}

func (a *templateAudit) setFields(raw []byte) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return
	}
	for _, name := range []string{"template_name", "tag", "description", "active"} {
		if _, ok := fields[name]; ok {
			a.fields = append(a.fields, name)
		}
	}
	slices.Sort(a.fields)
}

func (a *templateAudit) log(ctx context.Context, status int) {
	slog.InfoContext(ctx, "template mutation", "action", a.action, "template_id", a.id, "actor", a.actor, "fields", a.fields, "status", status)
}
