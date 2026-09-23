package store

import (
	"encoding/json"

	"github.com/orpheus-agents/orpheus/internal/session"
)

func (a Admission) validateExternal() error {
	if a.Create != nil {
		if err := session.ValidateExternal(a.Create.Namespace, session.NamespaceMaxBytes, "body", "namespace"); err != nil {
			return err
		}
		if err := session.ValidateExternal(a.Create.ExternalKey, session.ExternalKeyMaxBytes, "body", "external_key"); err != nil {
			return err
		}
	}
	if err := session.ValidateExternal(a.InputFingerprint, session.InputFingerprintMaxBytes, "body", "input_fingerprint"); err != nil {
		return err
	}
	return session.ValidateExternal(a.MessageExternalKey, session.ExternalKeyMaxBytes, "body", "message", "external_key")
}

// ListFilter contains exact matches. Nil means no filter, never SQL NULL matching.
type ListFilter struct {
	Namespace        *string `json:"namespace"`
	ExternalKey      *string `json:"external_key"`
	InputFingerprint *string `json:"input_fingerprint"`
	Status           *string `json:"status"`
	Order            string  `json:"order"`
}

func (f ListFilter) scope(resource string) (string, error) {
	for _, field := range []struct {
		name  string
		value *string
		limit int
	}{
		{"namespace", f.Namespace, session.NamespaceMaxBytes}, {"external_key", f.ExternalKey, session.ExternalKeyMaxBytes}, {"input_fingerprint", f.InputFingerprint, session.InputFingerprintMaxBytes},
	} {
		if err := session.ValidateExternal(field.value, field.limit, "query", field.name); err != nil {
			return "", err
		}
	}
	if f.Order == "" {
		f.Order = "asc"
	}
	if f.Order != "asc" && f.Order != "desc" {
		return "", invalidFilter("order")
	}
	if f.Status != nil && !session.Status(*f.Status).Valid() {
		return "", invalidFilter("status")
	}
	// Structured encoding avoids collisions when opaque values contain separators.
	raw, err := json.Marshal(struct {
		Resource string
		Filter   ListFilter
	}{resource, f})
	return string(raw), err
}

func invalidFilter(name string) error {
	p := session.Problem(422, "validation_error", "Invalid query parameter.")
	p.Problem.Details = []session.Detail{{Path: []any{"query", name}, Code: "invalid_value"}}
	return p
}
