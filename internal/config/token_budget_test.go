package config

import (
	"math"
	"testing"
)

func TestTokenBudgetSettings(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	if got := DefaultSettings().DefaultMaxSessionTokens; got != 100_000_000 {
		t.Fatal(got)
	}
	for _, tc := range []struct {
		value string
		want  int64
	}{
		{"1", 1}, {"100000000", 100_000_000}, {"9223372036854775807", math.MaxInt64},
		{"0", 0}, {"-1", 0}, {"", 0}, {"1.5", 0}, {"true", 0}, {"9223372036854775808", 0},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("DEFAULT_MAX_SESSION_TOKENS", tc.value)
			got, err := Load()
			if tc.want == 0 {
				if err == nil {
					t.Fatal("invalid budget accepted")
				}
			} else if err != nil || got.DefaultMaxSessionTokens != tc.want {
				t.Fatal(got.DefaultMaxSessionTokens, err)
			}
		})
	}
}
