package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
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
	deadline := time.Date(2026, 11, 5, 15, 20, 0, 123456789, time.FixedZone("provider", 3*60*60))
	runID, invocationID := uuid.New(), uuid.New()
	entry := servicecredentials.Entry{Config: servicecredentials.Config{Endpoint: "https://provider.invalid", SecretEnv: "CORE_KEY", ContextFields: []string{"subject", "scope_context", "context_observed_at"}, TokenFile: "~/.kord/run.token"}, Grant: servicecredentials.Grant{AccessToken: "first-private", RunExpiresAt: deadline, ScopeKey: "private-scope", CredentialID: "private-credential"}}
	if err := e.installCredentialFile(t.Context(), runID, invocationID, entry); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, ".kord", "run.token")
	metadataPath := filepath.Join(home, ".kord", "run.json")
	for path, want := range map[string]os.FileMode{filepath.Dir(target): 0700, target: 0600, metadataPath: 0400} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatal("permissions", path, info, err)
		}
	}
	checkMetadata := func(wantRun, wantInvocation uuid.UUID, wantDeadline time.Time) {
		t.Helper()
		raw, err := os.ReadFile(metadataPath)
		if err != nil {
			t.Fatal(err)
		}
		var metadata credentialMetadata
		if err := json.Unmarshal(raw, &metadata); err != nil || metadata.Schema != 1 || metadata.RunID != wantRun || metadata.InvocationID != wantInvocation || !metadata.RunExpiresAt.Equal(wantDeadline) || metadata.RunExpiresAt.Location() != time.UTC {
			t.Fatal("wrong deadline metadata", metadata, err)
		}
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil || len(fields) != 4 || strings.Contains(string(raw), "private") || strings.Contains(string(raw), "CORE_KEY") || strings.Contains(string(raw), "provider.invalid") {
			t.Fatal("metadata contains nonpublic grant fields", err)
		}
	}
	checkMetadata(runID, invocationID, deadline)
	entry.Grant.AccessToken = "second-private"
	runID, invocationID = uuid.New(), uuid.New()
	entry.Grant.RunExpiresAt = deadline.Add(5 * time.Minute)
	if err := e.installCredentialFile(t.Context(), runID, invocationID, entry); err != nil {
		t.Fatal(err)
	}
	checkMetadata(runID, invocationID, entry.Grant.RunExpiresAt)
	raw, err := os.ReadFile(target)
	if err != nil || string(raw) != "second-private" {
		t.Fatal("replacement", err)
	}
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil || len(entries) != 2 {
		t.Fatal("staging file survived", entries, err)
	}
	if err := e.removeCredentialFiles(t.Context(), servicecredentials.State{Entries: map[string]servicecredentials.Entry{"kord": entry}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("token survived cleanup", err)
	}
	if _, err := os.Stat(metadataPath); !os.IsNotExist(err) {
		t.Fatal("deadline survived cleanup", err)
	}
}
func TestCredentialFileRejectsSymlinkDirectory(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(home, ".kord")); err != nil {
		t.Fatal(err)
	}
	e := &Executor{sandbox: localCredentialBox{home: home}}
	entry := servicecredentials.Entry{Config: servicecredentials.Config{Endpoint: "https://provider.invalid", SecretEnv: "CORE_KEY", ContextFields: []string{"subject", "scope_context", "context_observed_at"}, TokenFile: "~/.kord/run.token"}, Grant: servicecredentials.Grant{AccessToken: "private", RunExpiresAt: time.Now().Add(time.Minute)}}
	if err := e.installCredentialFile(t.Context(), uuid.New(), uuid.New(), entry); err == nil {
		t.Fatal("followed symlink")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatal("wrote outside configured directory")
	}
	entry.Config.TokenFile = "~/.kord/../other.token"
	if err := e.installCredentialFile(t.Context(), uuid.New(), uuid.New(), entry); err == nil || !strings.Contains(err.Error(), "service_credentials_unavailable") {
		t.Fatal("accepted traversal", err)
	}
}

type failedMetadataBox struct{ localCredentialBox }

func (b failedMetadataBox) Run(ctx context.Context, command string) ([]byte, error) {
	if strings.Contains(command, "mv -f --") {
		// Model a lost/failed final metadata rename after the bearer was replaced.
		prefix, _, _ := strings.Cut(command, "; mv -f -- ")
		first := strings.Index(command, "; mv -f -- ")
		last := strings.LastIndex(command, "; mv -f -- ")
		if first == last || prefix == "" {
			return nil, errors.New("unexpected publish command")
		}
		if _, err := b.localCredentialBox.Run(ctx, command[:last]); err != nil {
			return nil, err
		}
		return nil, errors.New("metadata rename failed")
	}
	return b.localCredentialBox.Run(ctx, command)
}

func TestFailedMetadataReplacementCannotKeepPreviousDeadline(t *testing.T) {
	home := t.TempDir()
	box := localCredentialBox{home: home}
	e := &Executor{sandbox: box}
	entry := servicecredentials.Entry{Config: servicecredentials.Config{Endpoint: "https://provider.invalid", SecretEnv: "CORE_KEY", TokenFile: "~/.kord/run.token"}, Grant: servicecredentials.Grant{AccessToken: "old-token", RunExpiresAt: time.Now().Add(time.Minute)}}
	if err := e.installCredentialFile(t.Context(), uuid.New(), uuid.New(), entry); err != nil {
		t.Fatal(err)
	}
	entry.Grant.AccessToken = "new-token"
	e.sandbox = failedMetadataBox{box}
	if err := e.installCredentialFile(t.Context(), uuid.New(), uuid.New(), entry); err == nil {
		t.Fatal("metadata failure was accepted")
	}
	if _, err := os.Stat(filepath.Join(home, ".kord", "run.json")); !os.IsNotExist(err) {
		t.Fatal("stale deadline survived partial replacement", err)
	}
	entries, err := os.ReadDir(filepath.Join(home, ".kord"))
	if err != nil || len(entries) != 1 || entries[0].Name() != "run.token" {
		t.Fatal("staging files survived failed replacement", entries, err)
	}
}

func TestCredentialMetadataRequiresBoundRunAndDeadline(t *testing.T) {
	entry := servicecredentials.Entry{Config: servicecredentials.Config{Endpoint: "https://provider.invalid", SecretEnv: "CORE_KEY", TokenFile: "~/.kord/run.token"}, Grant: servicecredentials.Grant{RunExpiresAt: time.Now().Add(time.Minute)}}
	for _, tc := range []struct {
		name            string
		run, invocation uuid.UUID
		deadline        time.Time
	}{
		{"missing run", uuid.Nil, uuid.New(), entry.Grant.RunExpiresAt},
		{"missing invocation", uuid.New(), uuid.Nil, entry.Grant.RunExpiresAt},
		{"missing deadline", uuid.New(), uuid.New(), time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Executor{} // Must fail before touching the sandbox.
			entry.Grant.RunExpiresAt = tc.deadline
			if err := e.installCredentialFile(t.Context(), tc.run, tc.invocation, entry); err == nil {
				t.Fatal("unbound deadline was accepted")
			}
		})
	}
}
