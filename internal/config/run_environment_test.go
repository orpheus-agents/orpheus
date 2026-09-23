package config

import (
	"errors"
	"testing"

	"github.com/skillum-ai/orpheus/internal/session"
)

func TestValidateRunEnvironment(t *testing.T) {
	allowlist := []string{"TOKEN_FROM"}
	for _, tc := range []struct {
		name string
		env  map[string]string
		from []string
		want bool
	}{
		{"empty", nil, nil, true},
		{"empty value", map[string]string{"EMPTY": ""}, nil, true},
		{"allowlisted missing value", nil, []string{"TOKEN_FROM"}, true},
		{"bad name", map[string]string{"BAD-NAME": "x"}, nil, false},
		{"reserved hook name", map[string]string{"ORPHEUS_RUN_ID": "x"}, nil, false},
		{"NUL", map[string]string{"VALUE": "x\x00y"}, nil, false},
		{"duplicate reference", nil, []string{"TOKEN_FROM", "TOKEN_FROM"}, false},
		{"overlap", map[string]string{"TOKEN_FROM": "x"}, []string{"TOKEN_FROM"}, false},
		{"not allowlisted", nil, []string{"OTHER"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRunEnvironment(tc.env, tc.from, allowlist)
			if tc.want && err != nil {
				t.Fatal(err)
			}
			if !tc.want {
				apiErr, ok := errors.AsType[*session.APIError](err)
				if !ok || apiErr.Status != 422 || apiErr.Problem.Code != "validation_error" {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestHookContextNamesReservedForSession(t *testing.T) {
	for _, name := range []string{"ORPHEUS_SESSION_ID", "ORPHEUS_WORKSPACE_PATH", "ORPHEUS_RUN_ID", "ORPHEUS_INPUT_FINGERPRINT", "ORPHEUS_AGENT_STATUS", "ORPHEUS_STOP_REASON"} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateSandbox(session.SandboxInput{Template: "fixture", Env: map[string]string{name: "spoofed"}}); err == nil {
				t.Fatal("accepted reserved hook context name")
			}
			if err := ValidateSandbox(session.SandboxInput{Template: "fixture", EnvFrom: []string{name}}); err == nil {
				t.Fatal("accepted reserved hook context reference")
			}
		})
	}
}
