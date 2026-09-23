// Package harness defines the process and history operations used by the executor.
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/orpheus-agents/orpheus/internal/session"
)

var ErrUncertain = errors.New("external operation outcome is uncertain")
var ErrNotFound = errors.New("sandbox or resource not found")
var ErrRejected = errors.New("external operation was rejected")
var ErrEnvironmentRejected = fmt.Errorf("sandbox environment: %w", ErrRejected)

type ExecutionError struct{ Code, Message string }

func (e *ExecutionError) Error() string  { return e.Code }
func Failure(code, message string) error { return &ExecutionError{code, message} }

type Item struct {
	NativeID, Type   string
	Index            int
	Text             string
	Kind             *string
	Name             string
	Input            json.RawMessage
	Status           string
	Result           json.RawMessage
	Completeness     string
	TruncationReason *string
}
type Turn struct {
	NativeID  string
	Status    session.Status
	Items     []Item
	ErrorCode string
}
type Snapshot struct {
	Usage  []UsageReport
	Turns  []Turn
	Path   *string
	Offset int64
}

// UsageReport carries cumulative counters for a native context, attributed to
// the turn that reported them. It is not a per-turn delta.
type UsageReport struct {
	ContextID string
	TurnID    string
	Total     session.Usage
}
type Context struct {
	NativeID    string
	HistoryPath *string
}
type Process struct {
	PID int
	Env map[string]string
}
type Stream interface {
	Stdout() <-chan []byte
	Stderr() <-chan []byte
	Write(context.Context, []byte) (int, error)
	Close() error
}
type Watch interface {
	Events() <-chan string
	Done() <-chan struct{}
	Close() error
}
type Sandbox interface {
	ID() string
	Pause(context.Context) error
	SetTimeout(context.Context, time.Duration) error
	Processes(context.Context) ([]Process, error)
	Kill(context.Context, int) error
	Run(context.Context, string) ([]byte, error)
	Start(context.Context, string, map[string]string, string) (Stream, int, error)
	Attach(context.Context, int) (Stream, error)
	Read(context.Context, string) (io.ReadCloser, error)
	Write(context.Context, string, []byte) error
	Watch(context.Context, string) (Watch, error)
}
type Platform interface {
	Create(context.Context, string, time.Duration, map[string]string) (Sandbox, error)
	Find(context.Context, map[string]string) ([]string, error)
	Info(context.Context, string) (string, error)
	Connect(context.Context, string, time.Duration) (Sandbox, error)
}
type Driver interface {
	Prepare(context.Context, string, session.Credentials) (map[string]string, error)
	Launch(context.Context, map[string]string, string) (int, error)
	Attach(context.Context, int) error
	Initialize(context.Context, session.Credentials, bool) error
	OpenContext(context.Context, session.AgentConfiguration, string, *string) (Context, error)
	Recover(context.Context, *string, *string, *string) (Snapshot, error)
	HasUpdates() bool
	Committed()
	Start(context.Context, string, string) (string, error)
	Steer(context.Context, string, string, string) error
	Interrupt(context.Context, string, string) error
	Snapshot(context.Context, string, *string, int64) (Snapshot, error)
	Close() error
}

func Quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
