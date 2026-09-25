package store

import (
	"encoding/json"
	"time"

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
	for index, message := range a.Messages {
		if err := session.ValidateExternal(message.ExternalKey, session.ExternalKeyMaxBytes, "body", "messages", index, "external_key"); err != nil {
			return err
		}
	}
	return nil
}

// ListFilter contains exact matches. Nil means no filter, never SQL NULL matching.
type ListFilter struct {
	Namespace          *string    `json:"namespace"`
	ExternalKey        *string    `json:"external_key"`
	InputFingerprint   *string    `json:"input_fingerprint"`
	Status             *string    `json:"status"`
	Order              string     `json:"order"`
	Activity           string     `json:"activity,omitempty"`
	Sort               string     `json:"sort,omitempty"`
	LastRunCreatedFrom *time.Time `json:"last_run_created_from,omitempty"`
	LastRunCreatedTo   *time.Time `json:"last_run_created_to,omitempty"`
}

func (f ListFilter) sessionDefaults() ListFilter {
	if f.Activity == "" {
		f.Activity = "all"
	}
	if f.Sort == "" {
		f.Sort = "created_at"
	}
	return f
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
	if resource == "sessions" {
		f = f.sessionDefaults()
		if f.Activity != "all" && f.Activity != "active" && f.Activity != "inactive" {
			return "", invalidFilter("activity")
		}
		if f.Sort != "created_at" && f.Sort != "last_run_created_at" {
			return "", invalidFilter("sort")
		}
		if f.LastRunCreatedFrom == nil && f.LastRunCreatedTo != nil {
			return "", invalidFilter("last_run_created_from")
		}
		if f.LastRunCreatedFrom != nil && f.LastRunCreatedTo == nil {
			return "", invalidFilter("last_run_created_to")
		}
		if f.LastRunCreatedFrom != nil && (!f.LastRunCreatedFrom.Before(*f.LastRunCreatedTo) || f.LastRunCreatedTo.Sub(*f.LastRunCreatedFrom) > 31*24*time.Hour) {
			return "", invalidFilter("last_run_created_from")
		}
		// Canonical defaults do not change the scope of cursors issued before
		// these optional filters existed.
		if f.Activity == "all" {
			f.Activity = ""
		}
		if f.Sort == "created_at" {
			f.Sort = ""
		}
	} else {
		// Session-only filters are irrelevant to run cursors.
		f.Activity, f.Sort = "", ""
		f.LastRunCreatedFrom, f.LastRunCreatedTo = nil, nil
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
