// Package opencode runs a private OpenCode V2 server for a single Orpheus run.
package opencode

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/diagnostic"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/harness/codex"
	"github.com/orpheus-agents/orpheus/internal/session"
)

//go:embed bridge.py
var bridge string

type Driver struct {
	box                                     harness.Sandbox
	rpc                                     *codex.RPC
	session, run, provider, template, model string
	online                                  bool
	limit                                   int
	readAt                                  time.Time
	generation                              uint64
	redactor                                *diagnostic.Redactor
}

func New(box harness.Sandbox, timeout time.Duration, limit int, redact *diagnostic.Redactor, sid, run uuid.UUID, template, provider, model string) *Driver {
	return &Driver{box: box, rpc: codex.NewRPC(box, timeout, redact), limit: limit, redactor: redact, session: "ses_" + sid.String(), run: run.String(), template: template, provider: provider, model: model}
}
func (d *Driver) StateDir(ctx context.Context) (string, error) {
	raw, err := d.box.Run(ctx, `set -eu; d="$HOME/.local/share/orpheus-opencode/`+d.session+`"; mkdir -p "$d"; chmod 700 "$d"; printf '%s' "$d"`)
	if err != nil {
		return "", err
	}
	path := string(raw)
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\r\n\x00") {
		return "", harness.Failure("environment_unavailable", "OpenCode state directory is unavailable.")
	}
	return path, nil
}
func (d *Driver) command() string {
	return "exec python3 -u -c " + harness.Quote(bridge) + " " + harness.Quote(d.session) + " " + harness.Quote(d.run)
}
func (d *Driver) Launch(ctx context.Context, env map[string]string, cwd string, _ session.Credentials) (int, error) {
	return d.rpc.Launch(ctx, d.command(), env, cwd)
}
func (d *Driver) Attach(ctx context.Context, pid int) error { return d.rpc.Attach(ctx, pid) }
func (d *Driver) call(ctx context.Context, method string, params map[string]any, dest any) error {
	if params == nil {
		params = map[string]any{}
	}
	params["run"] = d.run
	var out struct {
		Data    json.RawMessage
		Failure json.RawMessage
	}
	if err := d.rpc.Call(ctx, method, params, &out); err != nil {
		return err
	}
	if len(out.Failure) > 0 {
		switch string(out.Failure) {
		case `"finished"`:
			return harness.ErrRejected
		case `"credential_conflict"`:
			return harness.Failure("credentials_unavailable", "Conflicting OpenCode credentials in the sandbox.")
		case `"context_lost"`:
			return harness.Failure("context_lost", "OpenCode native session is unavailable.")
		case `"integrity"`:
			return harness.Failure("context_lost", "OpenCode message identity does not match the persisted delivery.")
		case `"transport"`, "500", "502", "503", "504":
			return harness.ErrUncertain
		case "401", "403":
			return harness.Failure("authentication_failed", "OpenCode rejected authentication.")
		}
		return harness.Failure("harness_failed", fmt.Sprintf("OpenCode %s failed (%s).", method, out.Failure))
	}
	if dest != nil {
		return json.Unmarshal(out.Data, dest)
	}
	return nil
}
func (d *Driver) Initialize(ctx context.Context, source session.Credentials, fresh bool) error {
	d.provider = source.Provider
	d.online = true
	if err := d.call(ctx, "ready", nil, nil); err != nil {
		return err
	}
	if source.Mode == "api_key" && fresh {
		entry := map[string]any{"id": "cred_" + uuid.NewSHA1(uuid.NameSpaceOID, []byte(source.AccountID+":"+source.Provider)).String(), "integrationID": source.Provider, "active": true, "label": "Orpheus", "value": map[string]string{"type": "key", "key": source.APIKey}}
		return d.call(ctx, "seed", map[string]any{"entry": entry, "replaceID": entry["id"]}, nil)
	}
	return nil
}
func (d *Driver) ImportCredential(ctx context.Context, raw json.RawMessage, replaceID string) error {
	return d.call(ctx, "seed", map[string]any{"entry": raw, "replaceID": replaceID}, nil)
}
func (d *Driver) ExportCredential(ctx context.Context) (json.RawMessage, error) {
	if !d.online {
		raw, err := d.box.Run(ctx, d.command()+" credential "+harness.Quote(d.provider))
		if err != nil {
			return nil, err
		}
		if !json.Valid(raw) {
			return nil, errors.New("invalid OpenCode credential response")
		}
		if strings.TrimSpace(string(raw)) == "null" {
			return nil, nil
		}
		return raw, nil
	}
	var raw json.RawMessage
	err := d.call(ctx, "credential", map[string]any{"provider": d.provider}, &raw)
	if string(raw) == "null" {
		raw = nil
	}
	return raw, err
}
func (d *Driver) ReadConsoleLimits(ctx context.Context) (json.RawMessage, error) {
	var raw json.RawMessage
	err := d.call(ctx, "limits", nil, &raw)
	return raw, err
}
func modelRef(provider, model string) (string, string) {
	if provider == "opencode" {
		first, rest, ok := strings.Cut(model, "/")
		if ok && (first == "opencode" || first == "opencode-go") {
			return first, rest
		}
	}
	return provider, model
}
func (d *Driver) OpenContext(ctx context.Context, agent session.AgentConfiguration, cwd string, existing *string) (harness.Context, error) {
	if existing != nil && *existing != d.session {
		return harness.Context{}, harness.Failure("context_lost", "OpenCode session identity changed.")
	}
	provider, model := modelRef(d.provider, agent.Model)
	err := d.call(ctx, "open", map[string]any{"provider": provider, "model": model, "variant": agent.OpenCode.Variant, "instructions": agent.Instructions, "cwd": cwd, "existing": existing != nil}, nil)
	return harness.Context{NativeID: d.session}, err
}
func (d *Driver) Deliver(ctx context.Context, _ session.AgentConfiguration, thread string, turn *string, v harness.Delivery) (string, error) {
	if thread != d.session || v.RunID != d.run || (turn != nil && *turn != d.run) || len(v.Inputs) == 0 {
		return "", harness.ErrRejected
	}
	err := d.call(ctx, "deliver", map[string]any{"inputs": v.Inputs, "operation": v.OperationID, "start": turn == nil, "replay": v.Replay}, nil)
	d.readAt = time.Time{}
	return d.run, err
}
func (d *Driver) Interrupt(ctx context.Context, thread, turn string) error {
	if thread != d.session || turn != d.run {
		return harness.ErrRejected
	}
	var resp struct{ Interrupted bool }
	if err := d.call(ctx, "interrupt", nil, &resp); err != nil {
		return err
	}
	if !resp.Interrupted {
		return harness.ErrRejected
	}
	return nil
}

type snapshotFrame struct {
	Ticket   string
	Snapshot *projection
}

func (d *Driver) readSnapshot(ctx context.Context, frame snapshotFrame) (harness.Snapshot, error) {
	if frame.Ticket != "" {
		home, err := d.StateDir(ctx)
		if err != nil {
			return harness.Snapshot{}, err
		}
		r, err := d.box.Read(ctx, home+"/projection.json")
		if err != nil {
			return harness.Snapshot{}, err
		}
		defer func() { _ = r.Close() }()
		var saved snapshotFrame
		if err = json.NewDecoder(r).Decode(&saved); err != nil {
			return harness.Snapshot{}, errors.New("invalid OpenCode snapshot file")
		}
		if frame.Ticket != saved.Ticket {
			return harness.Snapshot{}, harness.ErrUncertain
		}
		frame = saved
	}
	if frame.Snapshot == nil {
		return harness.Snapshot{}, errors.New("empty OpenCode snapshot")
	}
	return d.normalize(*frame.Snapshot)
}
func (d *Driver) Recover(ctx context.Context, thread, _, _ *string) (harness.Snapshot, error) {
	if thread == nil || *thread != d.session {
		return harness.Snapshot{}, harness.Failure("context_lost", "OpenCode session is unavailable.")
	}
	raw, err := d.box.Run(ctx, d.command()+" recover")
	if err != nil {
		return harness.Snapshot{}, err
	}
	var v snapshotFrame
	if err := json.Unmarshal(raw, &v); err != nil {
		return harness.Snapshot{}, errors.New("invalid OpenCode recovery snapshot")
	}
	return d.readSnapshot(ctx, v)
}
func (d *Driver) HasUpdates() bool {
	return time.Now().After(d.readAt) || d.generation != d.rpc.Generation()
}
func (d *Driver) Snapshot(ctx context.Context, thread string, _ *string, _ int64) (harness.Snapshot, error) {
	if thread != d.session {
		return harness.Snapshot{}, harness.ErrRejected
	}
	d.generation = d.rpc.Generation()
	var v snapshotFrame
	if err := d.call(ctx, "snapshot", nil, &v); err != nil {
		return harness.Snapshot{}, err
	}
	return d.readSnapshot(ctx, v)
}
func (d *Driver) Committed()   { d.readAt = time.Now().Add(4 * time.Second) }
func (d *Driver) Close() error { return d.rpc.Close() }
