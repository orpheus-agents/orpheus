// Package session defines the durable, harness-neutral session model.
package session

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type Status string

const (
	Accepted   Status = "accepted"
	Starting   Status = "starting"
	Running    Status = "running"
	Cancelling Status = "cancelling"
	Finalizing Status = "finalizing"
	Completed  Status = "completed"
	Failed     Status = "failed"
	Cancelled  Status = "cancelled"
)

func (s Status) Valid() bool {
	switch s {
	case Accepted, Starting, Running, Cancelling, Finalizing, Completed, Failed, Cancelled:
		return true
	default:
		return false
	}
}

func (s Status) Terminal() bool { return s == Completed || s == Failed || s == Cancelled }

type Error struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Phase   *string  `json:"phase"`
	Details []Detail `json:"details"`
}

type Detail struct {
	Path []any  `json:"path"`
	Code string `json:"code"`
}

type APIError struct {
	Status  int
	Problem Error
}

func (e *APIError) Error() string { return e.Problem.Code }
func Problem(status int, code, message string) *APIError {
	return &APIError{Status: status, Problem: Error{Code: code, Message: message}}
}

type AgentInput struct {
	Profile      string  `json:"profile"`
	Model        *string `json:"model,omitzero"`
	Instructions *string `json:"instructions,omitzero"`
}
type SandboxInput struct {
	Template string            `json:"template"`
	Env      map[string]string `json:"env"`
	EnvFrom  []string          `json:"env_from"`
}
type Limits struct {
	RunTimeoutSeconds int   `json:"run_timeout_seconds"`
	MaxSessionTokens  int64 `json:"max_session_tokens,omitzero"`
}
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}
type HooksInput struct {
	AfterCreate    *string `json:"after_create,omitzero"`
	BeforeRun      *string `json:"before_run,omitzero"`
	AfterRun       *string `json:"after_run,omitzero"`
	BeforeRemove   *string `json:"before_remove,omitzero"`
	TimeoutSeconds *int    `json:"timeout_seconds,omitzero"`
}
type HooksConfiguration struct {
	AfterCreate    *string `json:"after_create,omitzero"`
	BeforeRun      *string `json:"before_run,omitzero"`
	AfterRun       *string `json:"after_run,omitzero"`
	BeforeRemove   *string `json:"before_remove,omitzero"`
	TimeoutSeconds int     `json:"timeout_seconds"`
}
type ConfigurationInput struct {
	Agent   AgentInput   `json:"agent"`
	Sandbox SandboxInput `json:"sandbox"`
	Limits  Limits       `json:"limits"`
	Hooks   *HooksInput  `json:"hooks,omitzero"`
}
type CodexConfiguration struct {
	Effort string `json:"effort,omitzero"`
}
type AgentConfiguration struct {
	Profile      string             `json:"profile"`
	Model        string             `json:"model"`
	Codex        CodexConfiguration `json:"codex,omitzero"`
	Instructions string             `json:"instructions"`
}
type SandboxConfiguration struct {
	Template string   `json:"template"`
	EnvNames []string `json:"env_names"`
	EnvFrom  []string `json:"env_from"`
}
type Configuration struct {
	Agent   AgentConfiguration   `json:"agent"`
	Sandbox SandboxConfiguration `json:"sandbox"`
	Limits  Limits               `json:"limits"`
	Hooks   HooksConfiguration   `json:"hooks"`
}
type CredentialStore struct {
	Type        string  `json:"type" toml:"type"`
	Bucket      string  `json:"bucket" toml:"bucket"`
	Region      string  `json:"region" toml:"region"`
	EndpointURL *string `json:"endpoint_url" toml:"endpoint_url"`
}
type Credentials struct {
	Mode      string           `json:"mode" toml:"mode"`
	APIKeyEnv string           `json:"api_key_env" toml:"api_key_env"`
	Store     *CredentialStore `json:"store" toml:"-"`
	Key       string           `json:"key" toml:"key"`
}
type ResolvedConfiguration struct {
	Version     int           `json:"version"`
	Harness     string        `json:"harness"`
	Public      Configuration `json:"public"`
	Credentials Credentials   `json:"credentials"`
}
type TextMessage struct {
	ExternalKey *string `json:"external_key,omitzero"`
	Text        string  `json:"text"`
}
type CreateSession struct {
	Namespace        *string            `json:"namespace,omitzero"`
	ExternalKey      *string            `json:"external_key,omitzero"`
	InputFingerprint *string            `json:"input_fingerprint,omitzero"`
	Configuration    ConfigurationInput `json:"configuration"`
	Message          TextMessage        `json:"message"`
	Env              map[string]string  `json:"env,omitzero"`
	EnvFrom          []string           `json:"env_from,omitzero"`
}
type Acceptance struct {
	SessionID uuid.UUID `json:"session_id"`
	RunID     uuid.UUID `json:"run_id"`
	MessageID uuid.UUID `json:"message_id"`
}
type Position struct {
	RunNumber int `json:"run_number"`
	ItemIndex int `json:"item_index"`
}
type Message struct {
	ExternalKey        *string   `json:"external_key"`
	ID                 uuid.UUID `json:"id"`
	SessionID          uuid.UUID `json:"session_id"`
	RunID              uuid.UUID `json:"run_id"`
	Role               string    `json:"role"`
	Kind               *string   `json:"kind"`
	Text               string    `json:"text"`
	DeliveryStatus     *string   `json:"delivery_status"`
	Error              *Error    `json:"error"`
	RegisteredSequence string    `json:"registered_sequence"`
	Position           *Position `json:"position"`
	CreatedAt          time.Time `json:"created_at"`
}
type ToolCall struct {
	ID                 uuid.UUID       `json:"id"`
	SessionID          uuid.UUID       `json:"session_id"`
	RunID              uuid.UUID       `json:"run_id"`
	Name               string          `json:"name"`
	Input              json.RawMessage `json:"input"`
	Status             string          `json:"status"`
	Result             json.RawMessage `json:"result"`
	OutputCompleteness string          `json:"output_completeness"`
	TruncationReason   *string         `json:"truncation_reason"`
	RegisteredSequence string          `json:"registered_sequence"`
	Position           *Position       `json:"position"`
	CreatedAt          time.Time       `json:"created_at"`
}
type HookResult struct {
	ID                 uuid.UUID       `json:"id"`
	Name               string          `json:"name"`
	Status             string          `json:"status"`
	StartedAt          *time.Time      `json:"started_at"`
	DeadlineAt         *time.Time      `json:"deadline_at"`
	FinishedAt         *time.Time      `json:"finished_at"`
	ExitCode           *int            `json:"exit_code"`
	Signal             *int            `json:"signal"`
	Output             json.RawMessage `json:"output"`
	OutputCompleteness string          `json:"output_completeness"`
	TruncationReason   *string         `json:"truncation_reason"`
	Error              *Error          `json:"error"`
}
type Run struct {
	Usage              Usage        `json:"usage"`
	InputFingerprint   *string      `json:"input_fingerprint"`
	EnvNames           []string     `json:"env_names"`
	EnvFrom            []string     `json:"env_from"`
	ID                 uuid.UUID    `json:"id"`
	SessionID          uuid.UUID    `json:"session_id"`
	Number             int          `json:"number"`
	Status             Status       `json:"status"`
	Phase              *string      `json:"phase"`
	AgentStatus        *Status      `json:"agent_status"`
	AgentError         *Error       `json:"agent_error"`
	Hooks              []HookResult `json:"hooks"`
	Observation        *string      `json:"observation"`
	CreatedAt          time.Time    `json:"created_at"`
	ExecutionStartedAt *time.Time   `json:"execution_started_at"`
	DeadlineAt         *time.Time   `json:"deadline_at"`
	FinishedAt         *time.Time   `json:"finished_at"`
	CancelRequestedAt  *time.Time   `json:"cancel_requested_at"`
	StopReason         *string      `json:"stop_reason"`
	StopMethod         *string      `json:"stop_method"`
	FinalMessage       *Message     `json:"final_message"`
	Error              *Error       `json:"error"`
}
type SandboxState struct {
	State          string  `json:"state"`
	LastKnownState *string `json:"last_known_state"`
	Error          *Error  `json:"error"`
	ID             *string `json:"id"`
	Workspace      *string `json:"workspace"`
}
type Session struct {
	Usage            Usage         `json:"usage"`
	Namespace        *string       `json:"namespace"`
	ExternalKey      *string       `json:"external_key"`
	ID               uuid.UUID     `json:"id"`
	CreatedAt        time.Time     `json:"created_at"`
	Configuration    Configuration `json:"configuration"`
	Sandbox          SandboxState  `json:"sandbox"`
	ActiveRunID      *uuid.UUID    `json:"active_run_id"`
	LastRunID        uuid.UUID     `json:"last_run_id"`
	LastRunCreatedAt time.Time     `json:"last_run_created_at"`
	Status           Status        `json:"status"`
	Phase            *string       `json:"phase"`
	FinalMessage     *Message      `json:"final_message"`
	Error            *Error        `json:"error"`
}
type Cancellation struct {
	RunID  uuid.UUID `json:"run_id"`
	Status Status    `json:"status"`
}
type Page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}
type HistoryItem struct {
	Type     string    `json:"type"`
	Message  *Message  `json:"message,omitzero"`
	ToolCall *ToolCall `json:"tool_call,omitzero"`
}
type HistoryPage struct {
	Page[HistoryItem]
	EventCursor string `json:"event_cursor"`
}
type Event struct {
	ID        string          `json:"id"`
	SessionID uuid.UUID       `json:"session_id"`
	Type      string          `json:"type"`
	CreatedAt time.Time       `json:"created_at"`
	Data      json.RawMessage `json:"data"`
}
type EventPage struct {
	Items      []Event `json:"items"`
	NextCursor string  `json:"next_cursor"`
	HasMore    bool    `json:"has_more"`
}
