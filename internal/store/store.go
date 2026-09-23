// Package store owns PostgreSQL transactions, admission, and durable projections.
package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skillum-ai/orpheus/internal/config"
	"github.com/skillum-ai/orpheus/internal/secret"
	"github.com/skillum-ai/orpheus/internal/session"
	"github.com/skillum-ai/orpheus/internal/store/db"
)

const CapacityLock int64 = 4857668146151
const WorkerLock int64 = 4857668146152

type Store struct {
	Pool     *pgxpool.Pool
	Settings config.Settings
	Profiles config.Profiles
	Cipher   *secret.Cipher
}
type SessionRecord db.Session

func (s SessionRecord) Sandbox() session.SandboxState {
	return session.SandboxState{State: s.SandboxState, LastKnownState: s.SandboxLastKnownState, Error: s.SandboxError, ID: s.SandboxID, Workspace: s.Workspace}
}

// UpdateSandbox publishes the complete snapshot only when public sandbox data changes.
// The caller's Mutate transaction commits the event and session update together.
func UpdateSandbox(ctx context.Context, tx pgx.Tx, r *SessionRecord, change func(*SessionRecord)) error {
	before := r.Sandbox()
	change(r)
	if reflect.DeepEqual(before, r.Sandbox()) {
		return nil
	}
	return Emit(ctx, tx, r, "sandbox.updated", r.Sandbox())
}

type RunRecord struct {
	session.Run
	EnvCiphertext      *string    `json:"-"`
	CancelAttemptedAt  *time.Time `json:"cancel_attempted_at"`
	NativeTurnID       *string    `json:"native_turn_id"`
	NextDeliveryNumber int        `json:"next_delivery_number"`
	FinalMessageID     *uuid.UUID `json:"final_message_id"`
}
type MessageRecord struct {
	session.Message
	DeliveryNumber *int    `json:"delivery_number"`
	NativeKey      *string `json:"native_key"`
}
type ToolRecord struct {
	session.ToolCall
	NativeKey    *string `json:"native_key"`
	ResultDigest string  `json:"result_digest"`
}
type Operation = db.Operation

func GetSession(ctx context.Context, q db.DBTX, id uuid.UUID, lock bool) (SessionRecord, error) {
	queries := db.New(q)
	var row db.Session
	var err error
	if lock {
		row, err = queries.LockSession(ctx, id)
	} else {
		row, err = queries.GetSession(ctx, id)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		err = session.Problem(404, "session_not_found", "Session not found.")
	}
	return SessionRecord(row), err
}
func GetRun(ctx context.Context, q db.DBTX, sid, rid uuid.UUID) (RunRecord, error) {
	v, err := runRecord(db.New(q).GetRun(ctx, db.GetRunParams{SessionID: sid, ID: rid}))
	if errors.Is(err, pgx.ErrNoRows) {
		err = session.Problem(404, "run_not_found", "Run not found.")
	}
	return v, err
}
func ActiveRun(ctx context.Context, q db.DBTX, sid uuid.UUID) (*RunRecord, error) {
	v, err := runRecord(db.New(q).ActiveRun(ctx, sid))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &v, err
}

func GetMessage(ctx context.Context, q db.DBTX, id uuid.UUID) (MessageRecord, error) {
	return messageRecord(db.New(q).GetMessage(ctx, id))
}
func Messages(ctx context.Context, q db.DBTX, rid uuid.UUID) ([]MessageRecord, error) {
	return messageRecords(db.New(q).Messages(ctx, rid))
}
func RunView(ctx context.Context, q db.DBTX, r RunRecord) (session.Run, error) {
	v := r.Run
	if v.EnvNames == nil {
		v.EnvNames = []string{}
	}
	if v.EnvFrom == nil {
		v.EnvFrom = []string{}
	}
	hooks, err := db.New(q).HookExecutions(ctx, r.ID)
	if err != nil {
		return v, err
	}
	v.Hooks = make([]session.HookResult, 0, len(hooks))
	for _, h := range hooks {
		v.Hooks = append(v.Hooks, session.HookResult{
			ID: h.ID, Name: h.Name, Status: h.Status, StartedAt: h.StartedAt,
			DeadlineAt: h.DeadlineAt, FinishedAt: h.FinishedAt, ExitCode: h.ExitCode,
			Signal: h.Signal, Output: h.Output, OutputCompleteness: h.OutputCompleteness,
			TruncationReason: h.TruncationReason, Error: h.Error,
		})
	}
	v.FinalMessage = nil
	if (r.Status.Terminal() || r.Status == session.Finalizing) && r.FinalMessageID != nil {
		m, err := GetMessage(ctx, q, *r.FinalMessageID)
		if err != nil {
			return v, err
		}
		v.FinalMessage = &m.Message
	}
	return v, nil
}
func SessionView(ctx context.Context, q db.DBTX, s SessionRecord) (session.Session, error) {
	r, err := runRecord(db.New(q).LatestRun(ctx, s.ID))
	if err != nil {
		return session.Session{}, err
	}
	v, err := RunView(ctx, q, r)
	if err != nil {
		return session.Session{}, err
	}
	out := session.Session{Namespace: s.Namespace, ExternalKey: s.ExternalKey, ID: s.ID, CreatedAt: s.CreatedAt, Configuration: s.Configuration.Public, Sandbox: s.Sandbox(), LastRunID: r.ID, Status: r.Status, Phase: r.Phase, FinalMessage: v.FinalMessage, Error: r.Error}
	if !r.Status.Terminal() {
		out.ActiveRunID = &r.ID
	}
	return out, nil
}
func Advisory(ctx context.Context, tx pgx.Tx, key int64) error {
	return db.New(tx).AdvisoryLock(ctx, key)
}
func LockKey(s string) int64 {
	h := sha256.Sum256([]byte(s))
	return int64(binary.BigEndian.Uint64(h[:8]))
}

// Mutate serializes mutations on the session. External I/O must happen outside fn.
// Admission and capacity release acquire the global capacity lock before the row.
func (s *Store) Mutate(ctx context.Context, id uuid.UUID, capacity bool, fn func(pgx.Tx, *SessionRecord) error) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if capacity {
			if err := Advisory(ctx, tx, CapacityLock); err != nil {
				return err
			}
		}
		record, err := GetSession(ctx, tx, id, true)
		if err != nil {
			return err
		}
		// Encode only persisted fields, independently of pointers and nested values
		// that fn may mutate in place.
		before, err := json.Marshal(sessionParams(&record))
		if err != nil {
			return err
		}
		if err := fn(tx, &record); err != nil {
			return err
		}
		after, err := json.Marshal(sessionParams(&record))
		if err != nil {
			return err
		}
		if bytes.Equal(before, after) {
			return nil
		}
		return SaveSession(ctx, tx, &record)
	})
}
func SaveSession(ctx context.Context, tx pgx.Tx, s *SessionRecord) error {
	return db.New(tx).SaveSession(ctx, sessionParams(s))
}
func sessionParams(s *SessionRecord) db.SaveSessionParams {
	return db.SaveSessionParams{
		ID:                    s.ID,
		SandboxState:          s.SandboxState,
		SandboxLastKnownState: s.SandboxLastKnownState,
		SandboxError:          s.SandboxError,
		SandboxID:             s.SandboxID,
		ProcessID:             s.ProcessID,
		LaunchID:              s.LaunchID,
		ThreadID:              s.ThreadID,
		HistoryPath:           s.HistoryPath,
		HistoryOffset:         s.HistoryOffset,
		Workspace:             s.Workspace,
		HarnessHome:           s.HarnessHome,
		SlotReserved:          s.SlotReserved,
		NextRunNumber:         s.NextRunNumber,
		NextEventSequence:     s.NextEventSequence,
	}
}
func SaveRun(ctx context.Context, tx pgx.Tx, r *RunRecord) error {
	for _, value := range []**time.Time{&r.ExecutionStartedAt, &r.DeadlineAt, &r.FinishedAt, &r.CancelRequestedAt, &r.CancelAttemptedAt} {
		if *value != nil {
			*value = new((*value).UTC().Truncate(time.Microsecond))
		}
	}
	return db.New(tx).SaveRun(ctx, db.SaveRunParams{
		ID:                 r.ID,
		Status:             r.Status,
		Observation:        r.Observation,
		ExecutionStartedAt: r.ExecutionStartedAt,
		DeadlineAt:         r.DeadlineAt,
		FinishedAt:         r.FinishedAt,
		CancelRequestedAt:  r.CancelRequestedAt,
		CancelAttemptedAt:  r.CancelAttemptedAt,
		StopReason:         r.StopReason,
		StopMethod:         r.StopMethod,
		Error:              r.Error,
		NativeTurnID:       r.NativeTurnID,
		NextDeliveryNumber: r.NextDeliveryNumber,
		FinalMessageID:     r.FinalMessageID,
		Phase:              r.Phase,
		AgentStatus:        r.AgentStatus,
		AgentError:         r.AgentError,
	})
}

func Emit(ctx context.Context, tx db.DBTX, s *SessionRecord, kind string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	err = db.New(tx).InsertEvent(ctx, db.InsertEventParams{SessionID: s.ID, Sequence: s.NextEventSequence, Type: kind, Data: raw})
	if err == nil {
		s.NextEventSequence++
	}
	return err
}
func PublishRun(ctx context.Context, tx pgx.Tx, s *SessionRecord, r *RunRecord) error {
	if err := SaveRun(ctx, tx, r); err != nil {
		return err
	}
	v, err := RunView(ctx, tx, *r)
	if err != nil {
		return err
	}
	return Emit(ctx, tx, s, "run.updated", v)
}
func PublishMessage(ctx context.Context, tx db.DBTX, s *SessionRecord, m *MessageRecord) error {
	if m.RegisteredSequence == "" || m.RegisteredSequence == "0" {
		m.RegisteredSequence = strconv.FormatInt(s.NextEventSequence, 10)
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	m.CreatedAt = m.CreatedAt.UTC().Truncate(time.Microsecond)
	seq, err := strconv.ParseInt(m.RegisteredSequence, 10, 64)
	if err != nil {
		return err
	}
	err = db.New(tx).UpsertMessage(ctx, db.UpsertMessageParams{
		ExternalKey:        m.ExternalKey,
		ID:                 m.ID,
		SessionID:          m.SessionID,
		RunID:              m.RunID,
		Role:               m.Role,
		Kind:               m.Kind,
		Text:               m.Text,
		DeliveryStatus:     m.DeliveryStatus,
		DeliveryNumber:     m.DeliveryNumber,
		Error:              m.Error,
		NativeKey:          m.NativeKey,
		RegisteredSequence: seq,
		Position:           m.Position,
		CreatedAt:          m.CreatedAt,
	})
	if err != nil {
		return err
	}
	return Emit(ctx, tx, s, "message.updated", m.Message)
}
func PublishTool(ctx context.Context, tx db.DBTX, s *SessionRecord, t *ToolRecord) error {
	if t.RegisteredSequence == "" {
		t.RegisteredSequence = strconv.FormatInt(s.NextEventSequence, 10)
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	t.CreatedAt = t.CreatedAt.UTC().Truncate(time.Microsecond)
	seq, err := strconv.ParseInt(t.RegisteredSequence, 10, 64)
	if err != nil {
		return err
	}
	if t.Input == nil {
		t.Input = json.RawMessage("null")
	}
	t.ResultDigest = resultDigest(t.Result)
	err = db.New(tx).UpsertTool(ctx, db.UpsertToolParams{
		ID:                 t.ID,
		SessionID:          t.SessionID,
		RunID:              t.RunID,
		Name:               t.Name,
		Input:              t.Input,
		Status:             t.Status,
		Result:             t.Result,
		OutputCompleteness: t.OutputCompleteness,
		TruncationReason:   t.TruncationReason,
		NativeKey:          t.NativeKey,
		RegisteredSequence: seq,
		Position:           t.Position,
		CreatedAt:          t.CreatedAt,
		ResultDigest:       t.ResultDigest,
	})
	if err != nil {
		return err
	}
	return Emit(ctx, tx, s, "tool_call.updated", t.ToolCall)
}
func Finish(ctx context.Context, tx pgx.Tx, s *SessionRecord, r *RunRecord, status session.Status, problem *session.Error, method *string) error {
	if err := selectFinalMessage(ctx, tx, r); err != nil {
		return err
	}
	r.Status = status
	r.Phase = nil
	r.FinishedAt = new(time.Now().UTC())
	r.Observation = new("attached")
	r.Error = problem
	if status == session.Cancelled {
		r.StopMethod = method
		if method == nil {
			r.StopMethod = new("graceful")
		}
	}
	messages, err := Messages(ctx, tx, r.ID)
	if err != nil {
		return err
	}
	for i := range messages {
		m := &messages[i]
		if m.DeliveryStatus != nil && *m.DeliveryStatus == "pending" {
			m.DeliveryStatus = new("rejected")
			m.Error = &session.Error{Code: "run_finished_before_delivery", Message: "Run finished before delivery.", Details: []session.Detail{}}
			if err := PublishMessage(ctx, tx, s, m); err != nil {
				return err
			}
		}
	}
	return PublishRun(ctx, tx, s, r)
}

func AgentFinished(ctx context.Context, tx pgx.Tx, s *SessionRecord, r *RunRecord, status session.Status, problem *session.Error, method *string) error {
	r.AgentStatus = &status
	r.AgentError = problem
	if s.Configuration.Public.Hooks.AfterRun == nil || r.ExecutionStartedAt == nil {
		return Finish(ctx, tx, s, r, status, problem, method)
	}
	if err := selectFinalMessage(ctx, tx, r); err != nil {
		return err
	}
	r.Status = session.Finalizing
	r.Phase = new("after_run")
	r.Error = problem
	r.Observation = new("attached")
	if status == session.Cancelled {
		r.StopMethod = method
		if method == nil {
			r.StopMethod = new("graceful")
		}
	}
	return PublishRun(ctx, tx, s, r)
}

func planHooks(ctx context.Context, tx pgx.Tx, s *SessionRecord, r *RunRecord) error {
	hooks := s.Configuration.Public.Hooks
	list := []struct {
		name   string
		script *string
	}{
		{"after_create", hooks.AfterCreate},
		{"before_run", hooks.BeforeRun},
		{"after_run", hooks.AfterRun},
	}
	for _, hook := range list {
		if hook.script == nil {
			continue
		}
		if hook.name == "after_create" {
			already, err := db.New(tx).SuccessfulAfterCreate(ctx, s.ID)
			if err != nil {
				return err
			}
			if already {
				continue
			}
		}
		if err := db.New(tx).InsertHookExecution(ctx, db.InsertHookExecutionParams{
			ID: uuid.New(), SessionID: s.ID, RunID: r.ID, Name: hook.name, Status: "pending",
		}); err != nil {
			return err
		}
	}
	return nil
}

func selectFinalMessage(ctx context.Context, tx pgx.Tx, r *RunRecord) error {
	messages, err := Messages(ctx, tx, r.ID)
	if err != nil {
		return err
	}
	var answer *MessageRecord
	for i := range messages {
		m := &messages[i]
		if m.Role == "assistant" && m.Kind != nil && *m.Kind == "answer" {
			index := func(m *MessageRecord) int {
				if m.Position == nil {
					return -1
				}
				return m.Position.ItemIndex
			}
			if answer == nil || index(m) >= index(answer) {
				answer = m
			}
		}
	}
	if answer != nil {
		r.FinalMessageID = &answer.ID
	}
	return nil
}

type Admission struct {
	InputFingerprint   *string
	MessageExternalKey *string
	Create             *session.CreateSession
	Text               string
	Key                uuid.UUID
	SessionID          uuid.UUID
	RunID              uuid.UUID
	Env                map[string]string
	EnvFrom            []string
}

func fingerprint(a Admission) (string, error) {
	runEnvNames := slices.Sorted(maps.Keys(a.Env))
	runEnvFrom := slices.Sorted(slices.Values(a.EnvFrom))
	var value any = struct {
		Message          session.TextMessage `json:"message"`
		InputFingerprint *string             `json:"input_fingerprint,omitzero"`
		EnvNames         []string            `json:"env_names,omitzero"`
		EnvFrom          []string            `json:"env_from,omitzero"`
	}{session.TextMessage{Text: a.Text, ExternalKey: a.MessageExternalKey}, a.InputFingerprint, runEnvNames, runEnvFrom}
	if a.Create != nil {
		c := a.Create.Configuration
		if c.Sandbox.EnvFrom == nil {
			c.Sandbox.EnvFrom = []string{}
		}
		names := slices.Sorted(maps.Keys(c.Sandbox.Env))
		if names == nil {
			names = []string{}
		}
		value = struct {
			Namespace        *string             `json:"namespace,omitzero"`
			ExternalKey      *string             `json:"external_key,omitzero"`
			InputFingerprint *string             `json:"input_fingerprint,omitzero"`
			Configuration    any                 `json:"configuration"`
			Message          session.TextMessage `json:"message"`
			RunEnvNames      []string            `json:"run_env_names,omitzero"`
			RunEnvFrom       []string            `json:"run_env_from,omitzero"`
		}{Namespace: a.Create.Namespace, ExternalKey: a.Create.ExternalKey, InputFingerprint: a.Create.InputFingerprint, Configuration: struct {
			Agent   session.AgentInput  `json:"agent"`
			Sandbox any                 `json:"sandbox"`
			Limits  session.Limits      `json:"limits"`
			Hooks   *session.HooksInput `json:"hooks,omitzero"`
		}{Agent: c.Agent, Sandbox: struct {
			Template string   `json:"template"`
			Env      []string `json:"env"`
			EnvFrom  []string `json:"env_from"`
		}{c.Sandbox.Template, names, c.Sandbox.EnvFrom}, Limits: c.Limits, Hooks: c.Hooks}, Message: a.Create.Message, RunEnvNames: runEnvNames, RunEnvFrom: runEnvFrom}
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:]), nil
}

// Accept locks idempotency first, then capacity (except steer), then the session
// row. Mutate follows the same capacity-before-session order.
func (s *Store) Accept(ctx context.Context, a Admission) (session.Acceptance, error) {
	var result session.Acceptance
	if a.Create != nil {
		a.Text = a.Create.Message.Text
		a.InputFingerprint = a.Create.InputFingerprint
		a.MessageExternalKey = a.Create.Message.ExternalKey
		a.Env = a.Create.Env
		a.EnvFrom = a.Create.EnvFrom
	}
	if err := a.validateExternal(); err != nil {
		return result, err
	}
	if err := config.ValidateText(a.Text); err != nil {
		return result, err
	}
	op, resource := "create", "sessions"
	if a.SessionID != uuid.Nil {
		op, resource = "run", a.SessionID.String()
	}
	if a.RunID != uuid.Nil {
		op, resource = "steer", a.RunID.String()
	}
	digest, err := fingerprint(a)
	if err != nil {
		return result, err
	}
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := Advisory(ctx, tx, LockKey(op+":"+resource+":"+a.Key.String())); err != nil {
			return err
		}
		prior, err := db.New(tx).GetIdempotency(ctx, db.GetIdempotencyParams{Operation: op, Resource: resource, Key: a.Key})
		if err == nil {
			result = session.Acceptance{SessionID: prior.SessionID, RunID: prior.RunID, MessageID: prior.MessageID}
			same := prior.Fingerprint == digest
			if same && a.Create != nil {
				r, e := GetSession(ctx, tx, result.SessionID, false)
				if e != nil {
					return e
				}
				env, e := s.Cipher.Decrypt(r.ID, r.EnvCiphertext)
				if e != nil {
					return session.Problem(503, "storage_unavailable", "Stored environment cannot be decrypted.")
				}
				same = maps.Equal(env, a.Create.Configuration.Sandbox.Env)
			}
			if same && a.RunID == uuid.Nil {
				r, e := GetRun(ctx, tx, result.SessionID, result.RunID)
				if e != nil {
					return e
				}
				env, e := s.Cipher.DecryptRun(r.SessionID, r.ID, r.EnvCiphertext)
				if e != nil {
					return session.Problem(503, "storage_unavailable", "Stored run environment cannot be decrypted.")
				}
				same = maps.Equal(env, a.Env)
			}
			if !same {
				return session.Problem(409, "idempotency_conflict", "Idempotency key was used for another request.")
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if a.RunID == uuid.Nil {
			if err := config.ValidateRunEnvironment(a.Env, a.EnvFrom, s.Settings.HarnessEnvAllowlist); err != nil {
				return err
			}
		}
		if a.RunID == uuid.Nil {
			if err := Advisory(ctx, tx, CapacityLock); err != nil {
				return err
			}
		}
		var record SessionRecord
		if a.Create != nil {
			resolved, err := config.Resolve(a.Create.Configuration, s.Profiles, s.Settings.HarnessEnvAllowlist)
			if err != nil {
				return err
			}
			id := uuid.New()
			token, err := s.Cipher.Encrypt(id, a.Create.Configuration.Sandbox.Env)
			if err != nil {
				return err
			}
			record, err = sessionRecord(db.New(tx).CreateSession(ctx, db.CreateSessionParams{Namespace: a.Create.Namespace, ExternalKey: a.Create.ExternalKey, ID: id, Configuration: resolved, EnvCiphertext: token}))
			if err != nil {
				return err
			}
		} else {
			record, err = GetSession(ctx, tx, a.SessionID, true)
			if err != nil {
				return err
			}
		}
		var run RunRecord
		if a.RunID != uuid.Nil {
			run, err = GetRun(ctx, tx, record.ID, a.RunID)
			if err != nil {
				return err
			}
			if run.Status != session.Running || run.CancelRequestedAt != nil {
				return session.Problem(409, "run_not_accepting_messages", "Run is not accepting messages.")
			}
		} else {
			if record.SandboxState == "unavailable" {
				return session.Problem(409, "session_unavailable", "Session environment is unavailable.")
			}
			active, err := ActiveRun(ctx, tx, record.ID)
			if err != nil {
				return err
			}
			if active != nil {
				return session.Problem(409, "session_busy", "Session already has an unfinished run.")
			}
			if !record.SlotReserved {
				count, err := db.New(tx).CountReserved(ctx)
				if err != nil {
					return err
				}
				if count >= int64(s.Settings.MaxConcurrentSessions) {
					return session.Problem(503, "capacity_exhausted", "Concurrent session limit reached.")
				}
				record.SlotReserved = true
			}
			runID := uuid.New()
			token, e := s.Cipher.EncryptRun(record.ID, runID, a.Env)
			if e != nil {
				return e
			}
			envNames := slices.Sorted(maps.Keys(a.Env))
			if envNames == nil {
				envNames = []string{}
			}
			envFrom := slices.Sorted(slices.Values(a.EnvFrom))
			if envFrom == nil {
				envFrom = []string{}
			}
			run, err = runRecord(db.New(tx).CreateRun(ctx, db.CreateRunParams{InputFingerprint: a.InputFingerprint, ID: runID, SessionID: record.ID, Number: record.NextRunNumber, EnvCiphertext: token, EnvNames: envNames, EnvFrom: envFrom}))
			if err != nil {
				return err
			}
			record.NextRunNumber++
		}
		m := MessageRecord{
			ExternalKey: a.MessageExternalKey, ID: uuid.New(),
			SessionID:      record.ID,
			RunID:          run.ID,
			Role:           "user",
			Text:           a.Text,
			DeliveryStatus: new("pending"),
			DeliveryNumber: new(run.NextDeliveryNumber),
		}
		run.NextDeliveryNumber++
		if err := PublishMessage(ctx, tx, &record, &m); err != nil {
			return err
		}
		if a.RunID == uuid.Nil {
			if err := planHooks(ctx, tx, &record, &run); err != nil {
				return err
			}
			err = PublishRun(ctx, tx, &record, &run)
		} else {
			err = SaveRun(ctx, tx, &run)
		}
		if err != nil {
			return err
		}
		if err := SaveSession(ctx, tx, &record); err != nil {
			return err
		}
		result = session.Acceptance{SessionID: record.ID, RunID: run.ID, MessageID: m.ID}
		err = db.New(tx).InsertIdempotency(ctx, db.InsertIdempotencyParams{
			Operation:   op,
			Resource:    resource,
			Key:         a.Key,
			Fingerprint: digest,
			SessionID:   result.SessionID,
			RunID:       result.RunID,
			MessageID:   result.MessageID,
		})
		return err
	})
	return result, err
}
func (s *Store) Cancel(ctx context.Context, sid, rid uuid.UUID) (session.Cancellation, error) {
	var out session.Cancellation
	err := s.Mutate(ctx, sid, true, func(tx pgx.Tx, record *SessionRecord) error {
		run, err := GetRun(ctx, tx, sid, rid)
		if err != nil {
			return err
		}
		if run.Status == session.Finalizing && run.CancelRequestedAt == nil {
			return session.Problem(409, "run_not_cancellable", "The agent has finished; the final hook cannot be cancelled.")
		}
		if !run.Status.Terminal() && run.CancelRequestedAt == nil {
			run.CancelRequestedAt = new(time.Now().UTC())
			run.StopReason = new("user_request")
			if run.Status == session.Accepted {
				if err := db.New(tx).SkipPendingHooks(ctx, run.ID); err != nil {
					return err
				}
				if err := Finish(ctx, tx, record, &run, session.Cancelled, nil, nil); err != nil {
					return err
				}
				if record.SandboxState == "not_created" || record.SandboxState == "paused" {
					record.SlotReserved = false
				}
			} else {
				run.Status = session.Cancelling
				if err := PublishRun(ctx, tx, record, &run); err != nil {
					return err
				}
			}
		}
		out = session.Cancellation{RunID: rid, Status: run.Status}
		return nil
	})
	return out, err
}
func (s *Store) Snapshot(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, fn)
}
func (s *Store) Read(ctx context.Context, sid uuid.UUID) (SessionRecord, *RunRecord, error) {
	var record SessionRecord
	var run *RunRecord
	err := s.Snapshot(ctx, func(tx pgx.Tx) error {
		var err error
		record, err = GetSession(ctx, tx, sid, false)
		if err != nil {
			return err
		}
		run, err = ActiveRun(ctx, tx, sid)
		return err
	})
	return record, run, err
}
func (s *Store) Reserved(ctx context.Context) ([]uuid.UUID, error) {
	return db.New(s.Pool).ReservedSessions(ctx)
}

// TryWorkerLock must use the worker's dedicated physical connection.
func TryWorkerLock(ctx context.Context, conn *pgx.Conn) (bool, error) {
	return db.New(conn).TryWorkerLock(ctx, WorkerLock)
}
