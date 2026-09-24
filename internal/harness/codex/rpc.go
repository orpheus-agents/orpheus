package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/harness"
)

type RPCError struct {
	Code          int    `json:"code"`
	NativeMessage string `json:"-"`
}

func (e *RPCError) UnmarshalJSON(raw []byte) error {
	var wire struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	e.Code, e.NativeMessage = wire.Code, wire.Message
	return nil
}

func (e *RPCError) Error() string { return "harness rejected RPC request" }
func (e *RPCError) Unwrap() error { return harness.ErrRejected }

type notification struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}
type rpcMessage struct {
	ID         json.RawMessage `json:"id"`
	Method     string          `json:"method"`
	Params     json.RawMessage `json:"params"`
	Result     json.RawMessage `json:"result"`
	Error      *RPCError       `json:"error"`
	localError error
}

var errResponseTooLarge = errors.New("RPC response exceeds the observation limit")

type RPC struct {
	sandbox       harness.Sandbox
	timeout       time.Duration
	stream        harness.Stream
	cancel        context.CancelFunc
	done          chan struct{}
	writeMu       sync.Mutex
	mu            sync.Mutex
	pending       map[string]chan rpcMessage
	notifications []notification
	dirty         bool
	disconnected  bool
}

func NewRPC(s harness.Sandbox, timeout time.Duration) *RPC {
	return &RPC{sandbox: s, timeout: timeout, pending: map[string]chan rpcMessage{}}
}
func (r *RPC) Launch(ctx context.Context, command string, env map[string]string, cwd string) (int, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	stream, pid, err := r.sandbox.Start(streamCtx, command, env, cwd)
	if err != nil {
		cancel()
		return 0, err
	}
	r.begin(streamCtx, cancel, stream)
	return pid, nil
}
func (r *RPC) Attach(ctx context.Context, pid int) error {
	streamCtx, cancel := context.WithCancel(ctx)
	stream, err := r.sandbox.Attach(streamCtx, pid)
	if err != nil {
		cancel()
		return err
	}
	r.begin(streamCtx, cancel, stream)
	return nil
}
func (r *RPC) begin(ctx context.Context, cancel context.CancelFunc, stream harness.Stream) {
	r.stream = stream
	r.cancel = cancel
	r.done = make(chan struct{})
	go r.read(ctx)
}
func (r *RPC) read(ctx context.Context) {
	defer close(r.done)
	defer func() { r.mu.Lock(); r.disconnected = true; r.dirty = true; r.mu.Unlock() }()
	stdout, stderr := r.stream.Stdout(), r.stream.Stderr()
	var buffer []byte
	discard := false
	for stdout != nil || stderr != nil {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-stderr:
			if !ok {
				stderr = nil
			}
		case chunk, ok := <-stdout:
			if !ok {
				stdout = nil
				continue
			}
			for len(chunk) > 0 {
				line, rest, complete := bytes.Cut(chunk, []byte{'\n'})
				if !discard {
					if len(buffer)+len(line) > 16<<20 {
						buffer = nil
						discard = true
						r.mu.Lock()
						r.dirty = true
						r.notifications = nil
						// The ID may occur after the truncated body. Fail all pending
						// calls as uncertain; only read operations may recover/retry.
						for _, ch := range r.pending {
							select {
							case ch <- rpcMessage{localError: errResponseTooLarge}:
							default:
							}
						}
						r.mu.Unlock()
					} else {
						buffer = append(buffer, line...)
					}
				}
				if !complete {
					break
				}
				if !discard && len(bytes.TrimSpace(buffer)) > 0 {
					r.receive(ctx, buffer)
				}
				buffer = nil
				discard = false
				chunk = rest
			}
		}
	}
}
func (r *RPC) markDirty() { r.mu.Lock(); r.dirty = true; r.mu.Unlock() }
func (r *RPC) receive(ctx context.Context, line []byte) {
	var message rpcMessage
	if json.Unmarshal(line, &message) != nil {
		r.markDirty()
		return
	}
	if message.Method != "" {
		if message.ID != nil {
			_ = r.write(ctx, struct {
				ID    json.RawMessage `json:"id"`
				Error struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}{ID: message.ID, Error: struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}{-32601, "Client requests are not supported"}})
			return
		}
		switch message.Method {
		case "item/completed", "item/started", "turn/completed", "turn/started", "thread/tokenUsage/updated":
			r.mu.Lock()
			switch {
			case r.dirty:
				// A complete read will replace this incomplete notification batch.
			case len(r.notifications) < 1024:
				r.notifications = append(r.notifications, notification{message.Method, message.Params})
			default:
				r.dirty = true
				r.notifications = nil
			}
			r.mu.Unlock()
		}
		return
	}
	var id string
	if json.Unmarshal(message.ID, &id) != nil {
		return
	}
	r.mu.Lock()
	ch := r.pending[id]
	if ch != nil {
		select {
		case ch <- message:
		default:
		}
	}
	r.mu.Unlock()
}
func (r *RPC) write(ctx context.Context, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	if r.stream == nil {
		return harness.ErrUncertain
	}
	_, err = r.stream.Write(ctx, raw)
	return err
}
func (r *RPC) Call(ctx context.Context, method string, params any, dest any) error {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	id := uuid.NewString()
	ch := make(chan rpcMessage, 1)
	r.mu.Lock()
	r.pending[id] = ch
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.pending, id); r.mu.Unlock() }()
	err := r.write(ctx, struct {
		ID     string `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params"`
	}{id, method, params})
	if err != nil {
		if errors.Is(err, harness.ErrRejected) {
			return err
		}
		return harness.ErrUncertain
	}
	select {
	case <-ctx.Done():
		return harness.ErrUncertain
	case <-r.done:
		return harness.ErrUncertain
	case reply := <-ch:
		if reply.localError != nil {
			return errors.Join(harness.ErrUncertain, reply.localError)
		}
		if reply.Error != nil {
			return reply.Error
		}
		if dest != nil {
			if len(reply.Result) == 0 {
				return harness.ErrUncertain
			}
			if err := json.Unmarshal(reply.Result, dest); err != nil {
				return harness.ErrUncertain
			}
		}
		return nil
	}
}
func (r *RPC) Initialize(ctx context.Context) error {
	err := r.Call(ctx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "orpheus", "version": "0.1.0"}}, nil)
	if e, ok := errors.AsType[*RPCError](err); ok && strings.Contains(strings.ToLower(e.NativeMessage), "already initialized") {
		return nil
	}
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return r.write(ctx, map[string]string{"method": "initialized"})
}
func (r *RPC) updates() (bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.notifications) > 0, r.dirty || r.disconnected
}
func (r *RPC) drain() ([]notification, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, dirty := r.notifications, r.dirty
	r.notifications = nil
	r.dirty = false
	return n, dirty
}
func (r *RPC) Close() error {
	if r.cancel == nil {
		return nil
	}
	r.cancel()
	err := r.stream.Close()
	<-r.done
	return err
}
