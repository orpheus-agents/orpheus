package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"

	"github.com/orpheus-agents/orpheus/internal/diagnostic"
)

const consoleA = `{"active":true,"id":"cred_xyz","integrationID":"opencode","label":"Console","value":{"type":"oauth","methodID":"device","access":"access-before","refresh":"refresh-private","expires":123,"metadata":{"accountID":"acct-id","orgID":"org-id","server":"https://opencode.ai/console"}}}`

type consoleBox struct{ fakeBox }

func (b *consoleBox) Read(ctx context.Context, name string) (io.ReadCloser, error) {
	if b.raw == "" {
		return nil, fs.ErrNotExist
	}
	return b.fakeBox.Read(ctx, name)
}

type nativeConsole struct {
	raw              json.RawMessage
	breakAfterDelete bool
}

func (n *nativeConsole) ExportCredential(context.Context) (json.RawMessage, error) { return n.raw, nil }
func (n *nativeConsole) ImportCredential(_ context.Context, raw json.RawMessage, replaceID string) error {
	if string(raw) == string(n.raw) {
		return nil
	}
	if len(n.raw) > 0 {
		old, err := ParseConsole(n.raw)
		if err != nil || replaceID != old.ID {
			return errors.New("credential conflict")
		}
	}
	n.raw = nil
	if n.breakAfterDelete {
		return errors.New("transport failure")
	}
	n.raw = raw
	return nil
}

func TestConsoleRecovery(t *testing.T) {
	ctx := t.Context()
	source := &fakeStore{raw: []byte(consoleA)}
	box := &consoleBox{}
	native := &nativeConsole{}
	newSync := func() *ConsoleSync {
		return &ConsoleSync{box: box, source: source, client: native, marker: "/state/version", redactor: &diagnostic.Redactor{}}
	}
	syncer := newSync()
	if err := syncer.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(box.raw, "private") {
		t.Fatal("marker contains raw credential")
	}
	native.raw = []byte("  " + consoleA + "\n")
	if err := newSync().Flush(ctx); err != nil || source.puts != 0 {
		t.Fatal("rewrote equivalent record", err)
	}
	rotated := strings.Replace(consoleA, "access-before", "access-after", 1)
	native.raw = []byte(rotated)
	if err := newSync().Flush(ctx); err != nil || source.puts != 1 || string(source.raw) != rotated {
		t.Fatal("failed to resume pending rotation", err)
	}
	replacement := strings.Replace(consoleA, "cred_xyz", "cred_abc", 1)
	if _, err := source.Put(ctx, []byte(replacement), source.etag); err != nil {
		t.Fatal(err)
	}
	native.breakAfterDelete = true
	if err := newSync().Seed(ctx); err == nil || native.raw != nil {
		t.Fatal("replacement was not interrupted")
	}
	native.breakAfterDelete = false
	if err := newSync().Seed(ctx); err != nil || string(native.raw) != replacement {
		t.Fatal("failed to recover replacement", err)
	}
}

func TestConsoleConcurrentRotations(t *testing.T) {
	ctx := t.Context()
	source := &fakeStore{raw: []byte(consoleA)}
	first := &nativeConsole{}
	second := &nativeConsole{}
	connect := func(client *nativeConsole) *ConsoleSync {
		syncer := &ConsoleSync{box: &consoleBox{}, source: source, client: client, marker: "version", redactor: &diagnostic.Redactor{}}
		if err := syncer.Seed(ctx); err != nil {
			t.Fatal(err)
		}
		return syncer
	}
	a, b := connect(first), connect(second)
	first.raw = []byte(strings.Replace(consoleA, "access-before", "winner-access", 1))
	second.raw = []byte(strings.Replace(consoleA, "access-before", "loser-access", 1))
	if err := a.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Flush(ctx); err == nil {
		t.Fatal("lost competing rotation")
	}
	if err := b.Seed(ctx); err == nil || string(source.raw) != string(first.raw) {
		t.Fatal("conflict replaced remote credential")
	}
	for _, token := range []string{"access-before", "winner-access", "refresh-private"} {
		if clean, ok := a.redactor.Clean(token); !ok || clean != "[REDACTED]" {
			t.Fatal("credential not redacted")
		}
	}
}

func TestConsoleFlushRecoversCommittedPut(t *testing.T) {
	ctx := t.Context()
	source := &fakeStore{raw: []byte(consoleA)}
	native := &nativeConsole{}
	c := &ConsoleSync{box: &consoleBox{}, source: source, client: native, marker: "version", redactor: &diagnostic.Redactor{}}
	if err := c.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	native.raw = []byte(strings.Replace(consoleA, "access-before", "rotated", 1))
	if _, err := source.Put(ctx, native.raw, source.etag); err != nil {
		t.Fatal(err)
	}
	if err := c.Flush(ctx); err != nil || source.puts != 1 || c.base.ETag != source.etag {
		t.Fatal("lost PUT acknowledgement was not reconciled", err)
	}
}

func TestConsoleValidation(t *testing.T) {
	if err := ValidateFile("opencode", []byte(consoleA)); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{authA, strings.Replace(consoleA, "opencode.ai", "other.example", 1), strings.Replace(consoleA, "\"active\":true", "\"active\":false", 1), strings.Replace(consoleA, "device", "api", 1), strings.Replace(consoleA, "refresh-private", "", 1)} {
		if err := ValidateFile("opencode", []byte(invalid)); err == nil {
			t.Fatal("accepted invalid Console record")
		}
	}
}
