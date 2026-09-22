// Package agentbox adapts the official SDK to the executor's narrow I/O contract.
package agentbox

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	sdk "github.com/abox-dev/sdk/packages/go-sdk"
	"github.com/skillum-ai/orpheus/internal/harness"
)

type Platform struct{ client *sdk.Client }

func New() (*Platform, error) {
	c, err := sdk.NewClient()
	if err != nil {
		return nil, err
	}
	return &Platform{c}, nil
}
func (p *Platform) Create(ctx context.Context, template string, timeout time.Duration, metadata map[string]string) (harness.Sandbox, error) {
	s, err := p.client.Sandboxes.Create(ctx, &sdk.CreateSandboxOptions{Template: template, Timeout: timeout, Metadata: metadata, AutoPause: new(true), AutoPauseMemory: new(true), AutoResume: new(false)})
	if err != nil {
		return nil, classify(err)
	}
	return &sandbox{s}, nil
}
func (p *Platform) Connect(ctx context.Context, id string, timeout time.Duration) (harness.Sandbox, error) {
	s, err := p.client.Sandboxes.Connect(ctx, id, &sdk.ConnectSandboxOptions{Timeout: timeout})
	if err != nil {
		return nil, classify(err)
	}
	return &sandbox{s}, nil
}
func (p *Platform) Info(ctx context.Context, id string) (string, error) {
	s, err := p.client.Sandboxes.Info(ctx, id)
	if err != nil {
		return "", classify(err)
	}
	return string(s.State), nil
}
func (p *Platform) Find(ctx context.Context, metadata map[string]string) ([]string, error) {
	out := []string{}
	next := ""
	for {
		page, err := p.client.Sandboxes.List(ctx, &sdk.ListSandboxOptions{Metadata: metadata, NextToken: next})
		if err != nil {
			return nil, classify(err)
		}
		for _, s := range page.Items {
			out = append(out, s.SandboxID)
		}
		if page.NextToken == "" {
			return out, nil
		}
		next = page.NextToken
	}
}

type sandbox struct{ s *sdk.Sandbox }

const sandboxUser = "user"

func (s *sandbox) ID() string { return s.s.ID }
func (s *sandbox) Pause(ctx context.Context) error {
	return classify(s.s.Pause(ctx, &sdk.PauseOptions{Memory: new(true)}))
}
func (s *sandbox) SetTimeout(ctx context.Context, d time.Duration) error {
	return classify(s.s.SetTimeout(ctx, d))
}
func (s *sandbox) Processes(ctx context.Context) ([]harness.Process, error) {
	ps, err := s.s.Commands.List(ctx)
	if err != nil {
		return nil, classify(err)
	}
	out := make([]harness.Process, 0, len(ps))
	for _, p := range ps {
		out = append(out, harness.Process{PID: int(p.PID), Env: p.Env})
	}
	return out, nil
}
func (s *sandbox) Kill(ctx context.Context, pid int) error {
	return classify(s.s.Commands.Kill(ctx, uint32(pid), ""))
}
func (s *sandbox) Run(ctx context.Context, command string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r, err := s.s.Commands.Run(ctx, "/bin/sh", &sdk.CommandOptions{User: sandboxUser, Args: []string{"-c", command}})
	return r.Stdout, classify(err)
}
func (s *sandbox) Start(ctx context.Context, command string, env map[string]string, cwd string) (harness.Stream, int, error) {
	return openStream(ctx, 60*time.Second, func(ctx context.Context) (*sdk.CommandHandle, error) {
		return s.s.Commands.Start(ctx, "/bin/sh", &sdk.CommandOptions{User: sandboxUser, Args: []string{"-c", command}, Env: env, Cwd: cwd, Stdin: true, Streaming: &sdk.CommandStreamingOptions{StdoutChannel: true}})
	})
}
func (s *sandbox) Attach(ctx context.Context, pid int) (harness.Stream, error) {
	h, _, err := openStream(ctx, 60*time.Second, func(ctx context.Context) (*sdk.CommandHandle, error) {
		return s.s.Commands.ConnectWithOptions(ctx, uint32(pid), "", &sdk.CommandConnectOptions{Streaming: &sdk.CommandStreamingOptions{StdoutChannel: true}})
	})
	return h, err
}

// Bound connection setup and PID acknowledgement, without imposing a deadline
// on the long-lived stream. A setup timeout is uncertain and never retries Start.
func openStream(ctx context.Context, timeout time.Duration, open func(context.Context) (*sdk.CommandHandle, error)) (harness.Stream, int, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	readyCtx, readyCancel := context.WithTimeout(ctx, timeout)
	defer readyCancel()
	stop := context.AfterFunc(readyCtx, cancel)
	h, err := open(streamCtx)
	var pid uint32
	if err == nil {
		pid, err = h.PID(readyCtx)
	}
	if !stop() && err == nil {
		err = readyCtx.Err()
	}
	if err != nil {
		cancel()
		if h != nil {
			_ = h.Close()
		}
		return nil, 0, classify(err)
	}
	return &stream{h: h, cancel: cancel}, int(pid), nil
}
func (s *sandbox) Read(ctx context.Context, path string) (io.ReadCloser, error) {
	r, err := s.s.Files.Read(ctx, path, &sdk.FileOptions{User: sandboxUser})
	return r, classify(err)
}
func (s *sandbox) Write(ctx context.Context, path string, b []byte) error {
	_, err := s.s.Files.WriteBytes(ctx, path, b, &sdk.WriteFileOptions{User: sandboxUser})
	return classify(err)
}

type stream struct {
	h      *sdk.CommandHandle
	cancel context.CancelFunc
}

func (s *stream) Stdout() <-chan []byte { return s.h.Stdout }
func (s *stream) Stderr() <-chan []byte { return s.h.Stderr }
func (s *stream) Write(ctx context.Context, b []byte) (int, error) {
	n, err := s.h.Write(ctx, b)
	return n, classify(err)
}
func (s *stream) Close() error { s.cancel(); return s.h.Close() }

type watch struct {
	h      *sdk.WatchHandle
	events chan string
	done   chan struct{}
	cancel context.CancelFunc
	once   sync.Once
}

func (s *sandbox) Watch(ctx context.Context, path string) (harness.Watch, error) {
	ctx, cancel := context.WithCancel(ctx)
	h, err := s.s.Files.Watch(ctx, path, &sdk.WatchOptions{User: sandboxUser})
	if err != nil {
		cancel()
		return nil, classify(err)
	}
	w := &watch{h: h, events: make(chan string, 1), done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(w.done)
		defer close(w.events)
		for {
			select {
			case <-ctx.Done():
				return
			case e, ok := <-h.Events:
				if !ok {
					return
				}
				select {
				case w.events <- e.Name:
				default:
				}
			}
		}
	}()
	return w, nil
}
func (w *watch) Events() <-chan string { return w.events }
func (w *watch) Done() <-chan struct{} { return w.done }
func (w *watch) Close() error {
	var err error
	w.once.Do(func() { w.cancel(); err = w.h.Close(); <-w.done })
	return err
}
func classify(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*sdk.SandboxNotFoundError](err); ok {
		return harness.ErrNotFound
	}
	if _, ok := errors.AsType[*sdk.FileNotFoundError](err); ok {
		return harness.ErrEnvironmentRejected
	}
	if _, ok := errors.AsType[*sdk.AuthenticationError](err); ok {
		return harness.ErrEnvironmentRejected
	}
	if _, ok := errors.AsType[*sdk.InvalidArgumentError](err); ok {
		return harness.ErrEnvironmentRejected
	}
	if _, ok := errors.AsType[*sdk.CommandExitError](err); ok {
		return harness.ErrEnvironmentRejected
	}
	if _, ok := errors.AsType[*sdk.NotEnoughSpaceError](err); ok {
		return harness.ErrEnvironmentRejected
	}
	if _, ok := errors.AsType[*sdk.TemplateError](err); ok {
		return harness.ErrEnvironmentRejected
	}
	if e, ok := errors.AsType[*sdk.SandboxError](err); ok && e.StatusCode >= 400 && e.StatusCode < 500 && e.StatusCode != 408 && e.StatusCode != 429 {
		return harness.ErrEnvironmentRejected
	}
	return err
}
