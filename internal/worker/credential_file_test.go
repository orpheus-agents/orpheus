package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/servicecredentials"
)

type localCredentialBox struct {
	harness.Sandbox
	home string
}

func (b localCredentialBox) Run(ctx context.Context, command string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Env = append(os.Environ(), "HOME="+b.home)
	return cmd.Output()
}
func (b localCredentialBox) Write(_ context.Context, path string, raw []byte) error {
	return os.WriteFile(path, raw, 0600)
}
func TestCredentialFileAtomicPermissionsAndCleanup(t *testing.T) {
	home := t.TempDir()
	e := &Executor{sandbox: localCredentialBox{home: home}}
	entry := servicecredentials.Entry{Config: servicecredentials.Config{Endpoint: "https://provider.invalid", SecretEnv: "CORE_KEY", ContextFields: []string{"subject", "scope_context", "context_observed_at"}, TokenFile: "~/.kord/run.token"}, Grant: servicecredentials.Grant{AccessToken: "first-private"}}
	if err := e.installCredentialFile(t.Context(), entry); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, ".kord", "run.token")
	for path, want := range map[string]os.FileMode{filepath.Dir(target): 0700, target: 0600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatal("permissions", path, info, err)
		}
	}
	entry.Grant.AccessToken = "second-private"
	if err := e.installCredentialFile(t.Context(), entry); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(target)
	if err != nil || string(raw) != "second-private" {
		t.Fatal("replacement", err)
	}
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil || len(entries) != 1 {
		t.Fatal("staging file survived", entries, err)
	}
	if err := e.removeCredentialFiles(t.Context(), servicecredentials.State{Entries: map[string]servicecredentials.Entry{"kord": entry}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("token survived cleanup", err)
	}
}
func TestCredentialFileRejectsSymlinkDirectory(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(home, ".kord")); err != nil {
		t.Fatal(err)
	}
	e := &Executor{sandbox: localCredentialBox{home: home}}
	entry := servicecredentials.Entry{Config: servicecredentials.Config{Endpoint: "https://provider.invalid", SecretEnv: "CORE_KEY", ContextFields: []string{"subject", "scope_context", "context_observed_at"}, TokenFile: "~/.kord/run.token"}, Grant: servicecredentials.Grant{AccessToken: "private"}}
	if err := e.installCredentialFile(t.Context(), entry); err == nil {
		t.Fatal("followed symlink")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatal("wrote outside configured directory")
	}
	entry.Config.TokenFile = "~/.kord/../other.token"
	if err := e.installCredentialFile(t.Context(), entry); err == nil || !strings.Contains(err.Error(), "service_credentials_unavailable") {
		t.Fatal("accepted traversal", err)
	}
}
