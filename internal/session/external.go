package session

import (
	"strings"
	"unicode/utf8"
)

const (
	NamespaceMaxBytes        = 128
	ExternalKeyMaxBytes      = 512
	InputFingerprintMaxBytes = 256
)

// ValidText is shared by prompts and opaque identifiers.
func ValidText(value string) bool {
	return utf8.ValidString(value) && !strings.ContainsRune(value, 0) && strings.TrimSpace(value) != ""
}

// ValidateExternal applies byte limits after transport type validation.
func ValidateExternal(value *string, maxBytes int, path ...any) error {
	if value == nil {
		return nil
	}
	if len(*value) <= maxBytes && ValidText(*value) {
		return nil
	}
	p := Problem(422, "validation_error", "Invalid external identifier.")
	p.Problem.Details = []Detail{{Path: path, Code: "invalid_value"}}
	return p
}
