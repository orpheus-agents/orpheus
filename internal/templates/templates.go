// Package templates defines the registered template catalog independently of execution snapshots.
package templates

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/session"
)

type Template struct {
	ID           uuid.UUID `json:"id"`
	Name         string    `json:"name"`
	TemplateName string    `json:"template_name"`
	Tag          *string   `json:"tag"`
	Description  *string   `json:"description"`
	Active       bool      `json:"active"`
}

type Input struct {
	TemplateName string
	Tag          *string
	Description  *string
	Active       bool
}

type Patch struct {
	Active         *bool
	Description    *string
	SetDescription bool
}

type Filter struct {
	Active       *bool
	Name         *string
	NameContains *string
}

type Page struct {
	Items      []Template `json:"items"`
	NextCursor *string    `json:"next_cursor"`
	PrevCursor *string    `json:"prev_cursor"`
}

var namePattern = regexp.MustCompile(`^[a-z0-9_-]+(/[a-z0-9_-]+)?$`)
var tagPattern = regexp.MustCompile(`^[a-z0-9_.-]+$`)

func Invalid(location, field, code, message string) *session.APIError {
	err := session.Problem(422, "validation_error", message)
	path := []any{location}
	if field != "" {
		path = append(path, field)
	}
	err.Problem.Details = []session.Detail{{Path: path, Code: code}}
	return err
}

func Normalize(in Input) (Input, error) {
	in.TemplateName = strings.ToLower(in.TemplateName)
	if len(in.TemplateName) > 255 || !namePattern.MatchString(in.TemplateName) {
		return in, Invalid("body", "template_name", "invalid_value", "Use an AgentBox alias, optionally prefixed by its team slug.")
	}
	if in.Tag != nil {
		tag := strings.ToLower(*in.Tag)
		_, uuidErr := uuid.Parse(tag)
		if len(tag) > 128 || !tagPattern.MatchString(tag) || uuidErr == nil {
			return in, Invalid("body", "tag", "invalid_value", "Use a tag, not a build UUID.")
		}
		in.Tag = &tag
	}
	var err error
	in.Description, err = normalizeDescription(in.Description)
	return in, err
}

func normalizeDescription(value *string) (*string, error) {
	if value == nil {
		return nil, nil
	}
	if !utf8.ValidString(*value) || utf8.RuneCountInString(*value) > 4096 || strings.ContainsRune(*value, 0) {
		return nil, Invalid("body", "description", "invalid_value", "Description must contain at most 4096 characters without NUL.")
	}
	if strings.TrimSpace(*value) == "" {
		return nil, nil
	}
	return value, nil
}

func ParsePatch(raw []byte) (Patch, error) {
	var fields map[string]json.RawMessage
	var out Patch
	if json.Unmarshal(raw, &fields) != nil || len(fields) == 0 {
		return out, Invalid("body", "", "invalid_value", "Provide active or description.")
	}
	for key, value := range fields {
		switch key {
		case "active":
			if json.Unmarshal(value, &out.Active) != nil || out.Active == nil {
				return out, Invalid("body", key, "invalid_type", "Active must be a boolean.")
			}
		case "description":
			out.SetDescription = true
			if json.Unmarshal(value, &out.Description) != nil {
				return out, Invalid("body", key, "invalid_type", "Description must be a string or null.")
			}
			var err error
			out.Description, err = normalizeDescription(out.Description)
			if err != nil {
				return out, err
			}
		default:
			return out, Invalid("body", key, "unknown_field", "Only active and description can be changed.")
		}
	}
	return out, nil
}

func (f Filter) Validate() error {
	if f.Name != nil && f.NameContains != nil {
		return Invalid("query", "name_contains", "invalid_value", "name and name_contains cannot be combined.")
	}
	for key, value := range map[string]*string{"name": f.Name, "name_contains": f.NameContains} {
		if value != nil && (strings.TrimSpace(*value) == "" || strings.ContainsRune(*value, 0) || !utf8.ValidString(*value)) {
			return Invalid("query", key, "invalid_value", "Search filters must be nonempty text without NUL.")
		}
	}
	return nil
}
