package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"testing"

	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/store"
)

type fileSandbox struct {
	harness.Sandbox
	err error
}

func (s fileSandbox) Read(context.Context, string) (io.ReadCloser, error) {
	if s.err != nil {
		return nil, s.err
	}
	return io.NopCloser(strings.NewReader("result")), nil
}
func TestHookFileAbsenceDoesNotHideSandboxFailure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		found bool
	}{
		{"ready", nil, true},
		{"pending", fmt.Errorf("read: %w", fs.ErrNotExist), false},
		{"sandbox lost", harness.ErrNotFound, false},
		{"access denied", harness.ErrEnvironmentRejected, false},
		{"transient failure", harness.ErrUncertain, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Executor{Store: &store.Store{Settings: config.DefaultSettings()}, sandbox: fileSandbox{err: tc.err}}
			data, found, err := e.sandboxFile(t.Context(), "result.json")
			if errors.Is(tc.err, fs.ErrNotExist) {
				if err != nil || found {
					t.Fatalf("pending result: %t %v", found, err)
				}
				return
			}
			if !errors.Is(err, tc.err) || found != tc.found {
				t.Fatal(found, err)
			}
			if found && string(data) != "result" {
				t.Fatal("result changed")
			}
		})
	}
}
