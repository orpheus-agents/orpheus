package session

import (
	"errors"
	"strings"
	"testing"
)

func TestExternalIdentifierValidation(t *testing.T) {
	for _, limit := range []int{128, 256, 512} {
		for _, v := range []*string{nil, new(" x "), new("null"), new(strings.Repeat("é", limit/2))} {
			if err := ValidateExternal(v, limit, "body", "external_key"); err != nil {
				t.Fatal(err)
			}
		}
		for _, v := range []string{"", " \t\n\u2003", "nul\x00", "\xff", strings.Repeat("é", limit/2) + "a"} {
			err := ValidateExternal(&v, limit, "body", "external_key")
			p, ok := errors.AsType[*APIError](err)
			if !ok || p.Status != 422 || p.Problem.Details[0].Code != "invalid_value" {
				t.Fatalf("value %q: %v", v, err)
			}
		}
	}
}
