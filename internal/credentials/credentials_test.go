package credentials

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skillum-ai/orpheus/internal/harness"
)

const authA = `{"tokens":{"access_token":"a","refresh_token":"r","id_token":"i"}}`
const authB = `{"tokens":{"access_token":"b","refresh_token":"r","id_token":"i"}}`

type fakeStore struct {
	raw  []byte
	puts int
	err  error
}

func (s *fakeStore) Get(context.Context) ([]byte, error) { return s.raw, s.err }
func (s *fakeStore) Put(_ context.Context, b []byte) error {
	if s.err != nil {
		return s.err
	}
	s.raw = append([]byte(nil), b...)
	s.puts++
	return nil
}

type fakeWatch struct {
	events chan string
	done   chan struct{}
	once   sync.Once
}

func (w *fakeWatch) Events() <-chan string { return w.events }
func (w *fakeWatch) Done() <-chan struct{} { return w.done }
func (w *fakeWatch) Close() error          { w.once.Do(func() { close(w.done) }); return nil }

type fakeBox struct {
	harness.Sandbox
	raw     string
	reads   int
	partial bool
}

func (b *fakeBox) Write(_ context.Context, _ string, raw []byte) error {
	b.raw = string(raw)
	return nil
}
func (b *fakeBox) Run(context.Context, string) ([]byte, error) { return nil, nil }
func (b *fakeBox) Read(context.Context, string) (io.ReadCloser, error) {
	b.reads++
	if b.partial && b.reads == 1 {
		return io.NopCloser(strings.NewReader("{")), nil
	}
	return io.NopCloser(strings.NewReader(b.raw)), nil
}
func (b *fakeBox) Watch(context.Context, string) (harness.Watch, error) {
	return &fakeWatch{events: make(chan string, 1), done: make(chan struct{})}, nil
}
func TestValidate(t *testing.T) {
	for _, raw := range []string{"", `{`, `{}`, `{"tokens":{}}`, `{"OPENAI_API_KEY":"x"}`, strings.Repeat("x", MaxAuthBytes+1)} {
		if Validate([]byte(raw)) == nil {
			t.Fatal("accepted malformed account file")
		}
	}
	if err := Validate([]byte(authA)); err != nil {
		t.Fatal(err)
	}
}
func TestSeedSyncPartialAndRetry(t *testing.T) {
	box := &fakeBox{}
	source := &fakeStore{raw: []byte(authA)}
	account := NewWithStore(box, "/home", source)
	defer func() { _ = account.Close() }()
	if err := account.Seed(t.Context()); err != nil {
		t.Fatal(err)
	}
	if box.raw != authA {
		t.Fatal("seed changed credentials")
	}
	account.Sync(t.Context(), true)
	if source.puts != 0 {
		t.Fatal("unchanged auth uploaded")
	}
	box.raw = authB
	box.reads = 0
	box.partial = true
	account.Sync(t.Context(), true)
	if source.puts != 1 || box.reads != 2 || string(source.raw) != authB {
		t.Fatal(source.puts, box.reads)
	}
	source.err = errors.New("secret error")
	box.raw = authA
	account.Sync(t.Context(), true)
	if !account.dirty.Load() {
		t.Fatal("failed upload lost dirty state")
	}
	source.err = nil
	account.Sync(t.Context(), false)
	if source.puts != 2 {
		t.Fatal("failed upload not retried")
	}
	source.err = errors.New("provider secret")
	if err := account.Seed(t.Context()); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal(err)
	}
}

func TestCoalescedWatchEventStillSyncsAuth(t *testing.T) {
	box := &fakeBox{raw: authA}
	source := &fakeStore{raw: []byte(authA)}
	account := NewWithStore(box, "/home", source)
	defer func() { _ = account.Close() }()
	account.Sync(t.Context(), true)
	box.raw = authB
	account.watch.(*fakeWatch).events <- "unrelated-file"
	deadline := time.After(time.Second)
	for !account.dirty.Load() {
		select {
		case <-deadline:
			t.Fatal("coalesced directory change was ignored")
		case <-time.After(time.Millisecond):
		}
	}
	account.Sync(t.Context(), false)
	if string(source.raw) != authB {
		t.Fatal("coalesced auth change was lost")
	}
}

func TestWatchReplacementAndReconnectReadCurrentAuth(t *testing.T) {
	box := &fakeBox{raw: authA}
	source := &fakeStore{raw: []byte(authA)}
	a := NewWithStore(box, "/home", source)
	t.Cleanup(func() { _ = a.Close() })
	a.Sync(t.Context(), true)
	first, done := a.watch, a.watchDone
	if err := a.Watch(t.Context()); err != nil || a.watch != first {
		t.Fatal("watch not reused", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("closed watch retained")
	}
	box.raw = authB
	a.Sync(t.Context(), false)
	if a.watch == first || string(source.raw) != authB {
		t.Fatal("watch replacement missed rotation")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	box.raw = authA
	restarted := NewWithStore(box, "/home", source)
	defer func() { _ = restarted.Close() }()
	restarted.Sync(t.Context(), false)
	if string(source.raw) != authA {
		t.Fatal("reconnect relied on watch replay")
	}
}

func TestBrokenSeedNeverWritesSandbox(t *testing.T) {
	box := &fakeBox{raw: "untouched"}
	a := NewWithStore(box, "/home", &fakeStore{raw: []byte(`{"tokens":{}}`)})
	if err := a.Seed(t.Context()); err == nil {
		t.Fatal("invalid seed accepted")
	}
	if box.raw != "untouched" {
		t.Fatal("invalid seed wrote sandbox")
	}
}
