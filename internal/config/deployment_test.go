package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/orpheus-agents/orpheus/internal/llm"
	"github.com/orpheus-agents/orpheus/internal/session"
)

func TestDeploymentWithoutLegacyCatalog(t *testing.T) {
	for _, raw := range []string{"", "[credential_stores.s]\nbucket='b'\n"} {
		got, err := ReadDeployment(strings.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		if store, ok := got.CredentialStores["s"]; ok && (store.Type != "s3" || store.Region != "us-east-1") {
			t.Fatal(store)
		}
	}
	for _, raw := range []string{"[profiles.p]\nharness='codex'", "[templates.t]\n", "[credential_stores.s]\nbucket='b'\nregion=''", "[credential_stores.s]\nbucket='b'\nendpoint_url='invalid'"} {
		if _, err := ReadDeployment(strings.NewReader(raw)); err == nil {
			t.Fatal("legacy or invalid config accepted", raw)
		}
	}
}
func TestSnapshotContainsExecutionOnly(t *testing.T) {
	profile := llm.ProfileInput{Name: "default", Model: "model", Description: new("description"), Codex: map[string]string{"effort": "high"}}
	in := session.ConfigurationInput{Agent: session.AgentInput{Profile: "default"}, Sandbox: session.SandboxInput{Template: "team/template:tag"}, Limits: session.Limits{RunTimeoutSeconds: 3600}}
	before, err := Resolve(in, profile, "codex", Deployment{}, nil, DefaultMaxSessionTokens)
	if err != nil {
		t.Fatal(err)
	}
	profile.Description = new("changed")
	after, err := Resolve(in, profile, "codex", Deployment{}, nil, DefaultMaxSessionTokens)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("description altered snapshot", err)
	}
	raw, err := json.Marshal(after)
	if err != nil || strings.Contains(string(raw), "credentials") || strings.Contains(string(raw), "description") {
		t.Fatal(string(raw), err)
	}
	in.Agent.Model = new("override")
	in.Agent.Instructions = new("")
	override, err := Resolve(in, profile, "codex", Deployment{}, nil, DefaultMaxSessionTokens)
	if err != nil || override.Public.Agent.Model != "override" || override.Public.Agent.Codex.Effort != "high" {
		t.Fatal(override, err)
	}
}
func TestSourceFingerprintKeepsLegacyIdentity(t *testing.T) {
	a := session.Credentials{Mode: "account", Store: &session.CredentialStore{Type: "s3", Bucket: "bucket", Region: "region", EndpointURL: new("HTTPS://S3.EXAMPLE:443/path")}, Key: "auth.json"}
	b := a
	b.Store = &session.CredentialStore{Type: "s3", Bucket: "bucket", Region: "region", EndpointURL: new("https://s3.example/path")}
	b.AccountID = "new-uuid"
	first, err := SourceFingerprint("codex", a)
	if err != nil {
		t.Fatal(err)
	}
	second, err := SourceFingerprint("codex", b)
	if err != nil || first != second || len(first) != 64 {
		t.Fatal(first, second, err)
	}
	b.Key = "another.json"
	third, err := SourceFingerprint("codex", b)
	if err != nil || third == first {
		t.Fatal("source change was hidden", err)
	}
}
