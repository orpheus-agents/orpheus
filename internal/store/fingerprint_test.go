package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/orpheus-agents/orpheus/internal/session"
)

func TestMessageMetadataFingerprintCanonical(t *testing.T) {
	a := Admission{Messages: []session.TextMessage{{Text: "task", Metadata: json.RawMessage(`{"b":2,"a":{"y":1,"x":2}}`)}}}
	first, err := fingerprint(a)
	if err != nil {
		t.Fatal(err)
	}
	a.Messages[0].Metadata = json.RawMessage(`{ "a": {"x":2,"y":1}, "b":2 }`)
	second, err := fingerprint(a)
	if err != nil || second != first {
		t.Fatalf("same object changed fingerprint: %s %s %v", first, second, err)
	}
	a.Messages[0].Metadata = json.RawMessage(`{"a":{"x":2,"y":1},"b":3}`)
	third, err := fingerprint(a)
	if err != nil || third == first {
		t.Fatalf("changed object kept fingerprint: %s %s %v", first, third, err)
	}
}

func TestRunFingerprintWithoutEnvironment(t *testing.T) {
	a := Admission{Messages: []session.TextMessage{{Text: "task"}}, InputFingerprint: new("snapshot")}
	got, err := fingerprint(a)
	if err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{"messages":[{"text":"task"}],"input_fingerprint":"snapshot"}`)
	sum := sha256.Sum256(legacy)
	if got != hex.EncodeToString(sum[:]) {
		t.Fatalf("empty run ENV changed existing idempotency fingerprint: %s", legacy)
	}
	a.Env = map[string]string{"TOKEN": "private"}
	withEnv, err := fingerprint(a)
	if err != nil || withEnv == got {
		t.Fatal("run ENV name omitted from fingerprint", withEnv, err)
	}
	a.Env["TOKEN"] = "changed"
	sameNames, err := fingerprint(a)
	if err != nil || sameNames != withEnv {
		t.Fatal("plaintext ENV entered fingerprint", sameNames, err)
	}
}
