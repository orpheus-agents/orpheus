package credentials

import (
	"encoding/json"
	"errors"
	"strings"
)

// ConsoleEntry is the active entry from "opencode auth export opencode".
// Preserve the original JSON when writing it back so unknown metadata survives.
type ConsoleEntry struct {
	ID            string  `json:"id"`
	IntegrationID string  `json:"integrationID"`
	Label         *string `json:"label"`
	Active        bool    `json:"active"`
	Value         struct {
		Type     string `json:"type"`
		MethodID string `json:"methodID"`
		Access   string `json:"access"`
		Refresh  string `json:"refresh"`
		Expires  *int64 `json:"expires"`
		Metadata struct {
			Server    string `json:"server"`
			AccountID string `json:"accountID"`
			OrgID     string `json:"orgID"`
		} `json:"metadata"`
	} `json:"value"`
}

func ParseConsole(raw []byte) (ConsoleEntry, error) {
	var entry ConsoleEntry
	if len(raw) <= MaxAuthBytes && json.Unmarshal(raw, &entry) == nil {
		v := entry.Value
		if entry.Active && entry.Label != nil && strings.HasPrefix(entry.ID, "cred_") && len(entry.ID) > len("cred_") &&
			entry.IntegrationID == "opencode" && v.Type == "oauth" && v.MethodID == "device" &&
			v.Access != "" && v.Refresh != "" && v.Expires != nil && *v.Expires >= 0 &&
			v.Metadata.Server == "https://opencode.ai/console" && v.Metadata.OrgID != "" && v.Metadata.AccountID != "" {
			return entry, nil
		}
	}
	return ConsoleEntry{}, errors.New("invalid OpenCode Console credential")
}

func ValidateFile(harnessName string, raw []byte) error {
	switch harnessName {
	case "codex":
		return Validate(raw)
	case "opencode":
		_, err := ParseConsole(raw)
		return err
	default:
		return errors.New("unsupported credential format")
	}
}
