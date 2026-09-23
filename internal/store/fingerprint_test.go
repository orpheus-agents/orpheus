package store

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestRunFingerprintWithoutEnvironment(t *testing.T) {
	a := Admission{Text: "task", InputFingerprint: new("snapshot")}
	got, err := fingerprint(a)
	if err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{"message":{"text":"task"},"input_fingerprint":"snapshot"}`)
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
