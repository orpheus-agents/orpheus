// Package llm defines account and model-profile catalog values.
package llm

import (
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/templates"
)

type Account struct {
	ID              uuid.UUID `json:"id"`
	Name            string    `json:"name"`
	Harness         string    `json:"harness"`
	Provider        string    `json:"provider"`
	AuthMode        string    `json:"auth_mode"`
	Active          bool      `json:"active"`
	CredentialStore *string   `json:"credential_store"`
	Profiles        []string  `json:"profiles"`
}
type AccountDetails struct {
	Account
	APIKeyMask *string `json:"api_key_mask"`
}
type AccountInput struct {
	Name            string          `json:"name"`
	Harness         string          `json:"harness"`
	Provider        string          `json:"provider"`
	AuthMode        string          `json:"auth_mode"`
	Active          *bool           `json:"active"`
	CredentialStore *string         `json:"credential_store"`
	AuthFile        json.RawMessage `json:"auth_file"`
	APIKey          *string         `json:"api_key"`
}
type Profile struct {
	OpenCode      session.OpenCodeConfiguration `json:"opencode,omitzero"`
	ID            uuid.UUID                     `json:"id"`
	Name          string                        `json:"name"`
	Description   *string                       `json:"description"`
	Harness       string                        `json:"harness"`
	AccountID     uuid.UUID                     `json:"account_id"`
	AccountName   string                        `json:"account_name"`
	AccountActive bool                          `json:"account_active"`
	Active        bool                          `json:"active"`
	Available     bool                          `json:"available"`
	Model         string                        `json:"model"`
	Instructions  string                        `json:"instructions"`
	Codex         map[string]string             `json:"codex"`
}
type ProfileInput struct {
	OpenCode     session.OpenCodeConfiguration `json:"opencode,omitzero"`
	Name         string                        `json:"name"`
	Description  *string                       `json:"description"`
	AccountID    uuid.UUID                     `json:"account_id"`
	Active       *bool                         `json:"active"`
	Model        string                        `json:"model"`
	Instructions string                        `json:"instructions"`
	Codex        map[string]string             `json:"codex"`
}
type Page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
	PrevCursor *string `json:"prev_cursor"`
}
type Filter struct {
	Active       *bool
	Available    *bool
	Name         *string
	NameContains *string
}

func (f Filter) Validate() error {
	return (templates.Filter{Active: f.Active, Name: f.Name, NameContains: f.NameContains}).Validate()
}
func Invalid(field string) *session.APIError {
	return templates.Invalid("body", field, "invalid_value", "Invalid catalog field.")
}
func validText(value string, max int, required bool) bool {
	return utf8.ValidString(value) && !strings.ContainsRune(value, 0) && utf8.RuneCountInString(value) <= max && (!required || (value != "" && value == strings.TrimSpace(value)))
}
func Decode(raw []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return Invalid("")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return Invalid("")
	}
	return nil
}
func NormalizeAccount(in AccountInput) (AccountInput, error) {
	if !validText(in.Name, 255, true) {
		return in, Invalid("name")
	}
	switch in.Harness {
	case "codex":
		if in.Provider != "" && in.Provider != "openai" {
			return in, Invalid("provider")
		}
		in.Provider = "openai"
	case "opencode":
		if in.AuthMode == "account" {
			if in.Provider != "" && in.Provider != "opencode" {
				return in, Invalid("provider")
			}
			in.Provider = "opencode"
		}
		if !validText(in.Provider, 128, true) || strings.ContainsAny(in.Provider, "/\\\r\n\t ") {
			return in, Invalid("provider")
		}
	default:
		return in, Invalid("harness")
	}
	switch in.AuthMode {
	case "api_key":
		if in.APIKey == nil || !ValidKey(*in.APIKey) {
			return in, Invalid("api_key")
		}
		if in.CredentialStore != nil || len(in.AuthFile) != 0 {
			return in, Invalid("auth_file")
		}
	case "account":
		if in.CredentialStore == nil || *in.CredentialStore == "" {
			return in, Invalid("credential_store")
		}
		if len(in.AuthFile) == 0 || bytes.Equal(bytes.TrimSpace(in.AuthFile), []byte("null")) {
			return in, Invalid("auth_file")
		}
		if in.APIKey != nil {
			return in, Invalid("api_key")
		}
	default:
		return in, Invalid("auth_mode")
	}
	if in.Active == nil {
		in.Active = new(true)
	}
	return in, nil
}
func ValidKey(key string) bool {
	return utf8.ValidString(key) && strings.TrimSpace(key) != "" && !strings.ContainsRune(key, 0)
}
func MaskKey(key string) string {
	text := []rune(strings.TrimSpace(key))
	switch {
	case len(text) <= 2:
		return "***"
	case len(text) <= 8:
		return string(text[:1]) + "..." + string(text[len(text)-1:])
	default:
		return string(text[:4]) + "..." + string(text[len(text)-4:])
	}
}
func NormalizeProfile(in ProfileInput) (ProfileInput, error) {
	if !validText(in.Name, 255, true) {
		return in, Invalid("name")
	}
	if in.AccountID == uuid.Nil {
		return in, Invalid("account_id")
	}
	if !validText(in.Model, 255, true) {
		return in, Invalid("model")
	}
	if !validText(in.Instructions, 65536, false) {
		return in, Invalid("instructions")
	}
	if in.Description != nil {
		if !validText(*in.Description, 4096, false) {
			return in, Invalid("description")
		}
		if strings.TrimSpace(*in.Description) == "" {
			in.Description = nil
		}
	}
	if v := in.OpenCode.Variant; v != nil && !validText(*v, 255, true) {
		return in, Invalid("opencode")
	}
	if in.Codex == nil {
		in.Codex = map[string]string{}
	}
	for name, value := range in.Codex {
		valid := false
		switch name {
		case "effort":
			valid = slices.Contains([]string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}, value)
		case "summary":
			valid = slices.Contains([]string{"auto", "concise", "detailed", "none"}, value)
		case "personality":
			valid = slices.Contains([]string{"none", "friendly", "pragmatic"}, value)
		case "service_tier":
			valid = validText(value, 1048576, true)
		}
		if !valid {
			err := Invalid("codex")
			err.Problem.Details[0].Path = append(err.Problem.Details[0].Path, name)
			return in, err
		}
	}
	if in.Active == nil {
		in.Active = new(true)
	}
	return in, nil
}
func (p ProfileInput) ValidateHarness(harness string) error {
	if harness != "codex" && len(p.Codex) > 0 {
		return Invalid("codex")
	}
	if harness != "opencode" && p.OpenCode.Variant != nil {
		return Invalid("opencode")
	}
	return nil
}
func (p ProfileInput) Options() session.CodexConfiguration {
	return session.CodexConfiguration{Effort: p.Codex["effort"], Summary: p.Codex["summary"], Personality: p.Codex["personality"], ServiceTier: p.Codex["service_tier"]}
}
func PatchProfile(in ProfileInput, raw []byte) (ProfileInput, error) {
	var fields map[string]json.RawMessage
	if Decode(raw, &fields) != nil || len(fields) == 0 {
		return in, Invalid("")
	}
	in.Codex = maps.Clone(in.Codex)
	for name, value := range fields {
		var target any
		switch name {
		case "account_id":
			target = &in.AccountID
		case "model":
			target = &in.Model
		case "instructions":
			target = &in.Instructions
		case "active":
			target = &in.Active
		case "description":
			target = &in.Description
		case "opencode":
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				in.OpenCode = session.OpenCodeConfiguration{}
				continue
			}
			var options map[string]*string
			if Decode(value, &options) != nil {
				return in, Invalid(name)
			}
			for key, variant := range options {
				if key != "variant" {
					return in, Invalid(name)
				}
				in.OpenCode.Variant = variant
			}
			continue
		case "codex":
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				in.Codex = map[string]string{}
				continue
			}
			var options map[string]*string
			if Decode(value, &options) != nil {
				return in, Invalid(name)
			}
			if in.Codex == nil {
				in.Codex = map[string]string{}
			}
			for key, option := range options {
				if !slices.Contains([]string{"effort", "summary", "personality", "service_tier"}, key) {
					return in, Invalid(name)
				}
				if option == nil {
					delete(in.Codex, key)
				} else {
					in.Codex[key] = *option
				}
			}
			continue
		default:
			return in, Invalid(name)
		}
		if name != "description" && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return in, Invalid(name)
		}
		if Decode(value, target) != nil {
			return in, Invalid(name)
		}
	}
	return NormalizeProfile(in)
}
func PatchAccount(in Account, raw []byte) (Account, error) {
	var fields map[string]json.RawMessage
	if Decode(raw, &fields) != nil || len(fields) == 0 {
		return in, Invalid("")
	}
	for name, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return in, Invalid(name)
		}
		switch name {
		case "name":
			if Decode(value, &in.Name) != nil || !validText(in.Name, 255, true) {
				return in, Invalid(name)
			}
		case "active":
			if Decode(value, &in.Active) != nil {
				return in, Invalid(name)
			}
		default:
			return in, Invalid(name)
		}
	}
	return in, nil
}
