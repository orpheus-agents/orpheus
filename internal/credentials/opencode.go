package credentials

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
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
		Type     string         `json:"type"`
		MethodID string         `json:"methodID"`
		Access   string         `json:"access"`
		Refresh  string         `json:"refresh"`
		Expires  *int64         `json:"expires"`
		Metadata map[string]any `json:"metadata"`
	} `json:"value"`
}

func ParseConsole(raw []byte) (ConsoleEntry, error) {
	var entry ConsoleEntry
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if len(raw) <= MaxAuthBytes && json.Valid(raw) && decoder.Decode(&entry) == nil {
		v := entry.Value
		org, _ := v.Metadata["orgID"].(string)
		account, _ := v.Metadata["accountID"].(string)
		if entry.Active && entry.Label != nil && strings.HasPrefix(entry.ID, "cred_") && len(entry.ID) > len("cred_") &&
			entry.IntegrationID == "opencode" && v.Type == "oauth" && v.MethodID == "device" &&
			v.Access != "" && v.Refresh != "" && v.Expires != nil && *v.Expires >= 0 &&
			v.Metadata["server"] == "https://opencode.ai/console" && org != "" && account != "" {
			return entry, nil
		}
	}
	return ConsoleEntry{}, errors.New("invalid OpenCode Console credential")
}

// mergeConsoleExport retains uploaded fields that the native version cannot
// represent. Metadata is an open map in the native schema and is replaced in full.
func mergeConsoleExport(stored, native []byte) ([]byte, error) {
	var previous, current map[string]json.RawMessage
	if err := json.Unmarshal(stored, &previous); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(native, &current); err != nil {
		return nil, err
	}
	var oldValue, newValue map[string]json.RawMessage
	if err := json.Unmarshal(previous["value"], &oldValue); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(current["value"], &newValue); err != nil {
		return nil, err
	}
	maps.Copy(oldValue, newValue)
	maps.Copy(previous, current)
	value, err := json.Marshal(oldValue)
	if err != nil {
		return nil, err
	}
	previous["value"] = value
	out, err := json.Marshal(previous)
	if err == nil && len(out) > MaxAuthBytes {
		return nil, errors.New("credential file exceeds maximum size")
	}
	return out, err
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
