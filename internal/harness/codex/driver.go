// Package codex implements app-server RPC and durable native-history recovery.
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/orpheus-agents/orpheus/internal/accountlimits"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/session"
)

type Driver struct {
	rpc             *RPC
	limit           int
	completed       map[string]bool
	outputs         map[string]Output
	thread          *Thread
	readAt          time.Time
	orders          map[string]int64
	commands        map[string]json.RawMessage
	indexLoaded     bool
	journalIDs      map[string]bool
	divergenceReads int
	recovered       map[string]NativeTurn
	historyOnly     bool
	usage           []harness.UsageReport
}

func New(s harness.Sandbox, timeout time.Duration, limit int) *Driver {
	return &Driver{rpc: NewRPC(s, timeout), limit: limit, completed: map[string]bool{}, outputs: map[string]Output{}, orders: map[string]int64{}, commands: map[string]json.RawMessage{}, journalIDs: map[string]bool{}, recovered: map[string]NativeTurn{}}
}

var _ harness.Driver = (*Driver)(nil)
var _ harness.AccountLimitsReader = (*Driver)(nil)

func (d *Driver) StateDir(ctx context.Context) (string, error) {
	raw, err := d.rpc.sandbox.Run(ctx, `set -eu; d="${CODEX_HOME:-$HOME/.codex}"; mkdir -p "$d"; chmod 700 "$d"; printf '%s' "$d"`)
	if err != nil {
		return "", err
	}
	home := string(raw)
	if !strings.HasPrefix(home, "/") || strings.ContainsAny(home, "\x00\r\n") {
		return "", harness.Failure("environment_unavailable", "Codex state directory is unavailable.")
	}
	return home, nil
}
func (d *Driver) Launch(ctx context.Context, env map[string]string, cwd string, source session.Credentials) (int, error) {
	mode := "api"
	if source.Mode == "account" {
		mode = "chatgpt"
	}
	var command strings.Builder
	command.WriteString("exec codex")
	for _, option := range []string{
		`cli_auth_credentials_store="file"`,
		`forced_login_method="` + mode + `"`,
		`approval_policy="never"`,
		`sandbox_mode="danger-full-access"`,
	} {
		command.WriteString(" -c " + harness.Quote(option))
	}
	return d.rpc.Launch(ctx, command.String()+" app-server", env, cwd)
}
func (d *Driver) Attach(ctx context.Context, pid int) error { return d.rpc.Attach(ctx, pid) }
func (d *Driver) Initialize(ctx context.Context, source session.Credentials, login bool) error {
	if err := d.rpc.Initialize(ctx); err != nil {
		return err
	}
	if login && source.Mode == "api_key" {
		key := os.Getenv(source.APIKeyEnv)
		if key == "" {
			return harness.Failure("credentials_unavailable", "Provider API key is unavailable.")
		}
		if err := d.rpc.Call(ctx, "account/login/start", map[string]string{"type": "apiKey", "apiKey": key}, nil); err != nil {
			return err
		}
	}
	if source.Mode == "account" {
		var out struct {
			Account *struct {
				Type string `json:"type"`
			} `json:"account"`
		}
		if err := d.rpc.Call(ctx, "account/read", map[string]bool{"refreshToken": false}, &out); err != nil {
			return err
		}
		if out.Account == nil || out.Account.Type != "chatgpt" {
			return harness.Failure("authentication_failed", "Account credentials are invalid.")
		}
		d.rpc.accountInitialized()
	}
	return nil
}
func (d *Driver) AccountLimitsEvents() <-chan struct{} { return d.rpc.limitsEvents() }
func (d *Driver) AccountLimitsDirty() bool             { return d.rpc.takeLimitsDirty() }
func (d *Driver) accountLimitsAvailable() error {
	disconnected, invalidated := d.rpc.accountStatus()
	if disconnected {
		return harness.ErrUncertain
	}
	if invalidated {
		return accountlimits.ErrAuthenticationUnavailable
	}
	return nil
}
func (d *Driver) ReadAccountLimits(ctx context.Context) (accountlimits.Snapshot, error) {
	if err := d.accountLimitsAvailable(); err != nil {
		return accountlimits.Snapshot{}, err
	}
	var raw json.RawMessage
	err := d.rpc.Call(ctx, "account/rateLimits/read", nil, &raw)
	if rpcError, ok := errors.AsType[*RPCError](err); ok && rpcError.Code == -32601 {
		return accountlimits.Snapshot{}, accountlimits.ErrUnsupported
	}
	if err != nil {
		return accountlimits.Snapshot{}, err
	}
	if err := d.accountLimitsAvailable(); err != nil {
		return accountlimits.Snapshot{}, err
	}
	return accountlimits.Parse(raw)
}
func (d *Driver) OpenContext(ctx context.Context, agent session.AgentConfiguration, cwd string, id *string) (harness.Context, error) {
	params := map[string]any{"model": agent.Model, "cwd": cwd, "approvalPolicy": "never", "sandbox": "danger-full-access"}
	if agent.Instructions != "" {
		params["developerInstructions"] = agent.Instructions
	}
	if id != nil {
		params["threadId"] = *id
		if err := d.rpc.Call(ctx, "thread/resume", params, nil); err != nil {
			if _, ok := errors.AsType[*RPCError](err); ok {
				return harness.Context{}, harness.Failure("context_lost", "Harness context cannot be restored.")
			}
			return harness.Context{}, err
		}
		return harness.Context{NativeID: *id}, nil
	}
	var out struct {
		Thread Thread `json:"thread"`
	}
	if err := d.rpc.Call(ctx, "thread/start", params, &out); err != nil {
		return harness.Context{}, err
	}
	if out.Thread.ID == "" {
		return harness.Context{}, harness.ErrUncertain
	}
	return harness.Context{NativeID: out.Thread.ID, HistoryPath: out.Thread.Path}, nil
}
func (d *Driver) Recover(ctx context.Context, id, path, home *string) (harness.Snapshot, error) {
	if path == nil && id != nil && home == nil {
		stateDir, err := d.StateDir(ctx)
		if err != nil {
			return harness.Snapshot{}, err
		}
		home = &stateDir
	}
	return recoverHistory(ctx, d.rpc.sandbox, id, path, home, d.limit)
}
func (d *Driver) HasUpdates() bool {
	notify, dirty := d.rpc.updates()
	return notify || dirty || len(d.usage) > 0 || d.thread == nil || !time.Now().Before(d.readAt)
}
func (d *Driver) Committed() { clear(d.outputs); d.usage = nil }
func (d *Driver) Start(ctx context.Context, agent session.AgentConfiguration, thread string, texts []string) (string, error) {
	if len(texts) == 0 {
		return "", harness.ErrRejected
	}
	if len(texts) > 1 {
		items := make([]map[string]any, 0, len(texts)-1)
		for _, text := range texts[:len(texts)-1] {
			items = append(items, map[string]any{
				"type": "message", "role": "user",
				"content": []map[string]string{{"type": "input_text", "text": text}},
			})
		}
		if err := d.rpc.Call(ctx, "thread/inject_items", map[string]any{"threadId": thread, "items": items}, nil); err != nil {
			return "", err
		}
	}
	var out struct {
		Turn NativeTurn `json:"turn"`
	}
	params := map[string]any{"threadId": thread, "input": []map[string]string{{"type": "text", "text": texts[len(texts)-1]}}}
	if agent.Codex.Effort != "" {
		params["effort"] = agent.Codex.Effort
	}
	if err := d.rpc.Call(ctx, "turn/start", params, &out); err != nil {
		if len(texts) > 1 && errors.Is(err, harness.ErrRejected) {
			// The earlier items were already injected. Retire this context so a
			// later run cannot see input from a rejected assignment.
			return "", harness.Failure("context_lost", "Harness rejected the batched assignment after context injection.")
		}
		return "", err
	}
	if out.Turn.ID == "" {
		return "", harness.ErrUncertain
	}
	if d.thread != nil {
		d.thread.Turns = append(d.thread.Turns, out.Turn)
	}
	return out.Turn.ID, nil
}
func (d *Driver) Steer(ctx context.Context, thread, turn, text string) error {
	return d.rpc.Call(ctx, "turn/steer", map[string]any{"threadId": thread, "expectedTurnId": turn, "input": []map[string]string{{"type": "text", "text": text}}}, nil)
}
func (d *Driver) Interrupt(ctx context.Context, thread, turn string) error {
	return d.rpc.Call(ctx, "turn/interrupt", map[string]string{"threadId": thread, "turnId": turn}, nil)
}
func (d *Driver) Close() error { return d.rpc.Close() }
func (d *Driver) normalize() harness.Snapshot {
	return Normalize(*d.thread, d.completed, d.outputs, d.limit, d.orders, d.commands)
}
func mergeItem(items []NativeItem, item NativeItem, replace bool) []NativeItem {
	index := slices.IndexFunc(items, func(i NativeItem) bool { return i.ID == item.ID })
	if index < 0 {
		return append(items, item)
	}
	if replace {
		items[index] = item
	}
	return items
}
func (d *Driver) mergeRecovered() {
	for id, recovered := range d.recovered {
		index := slices.IndexFunc(d.thread.Turns, func(t NativeTurn) bool { return t.ID == id })
		if index < 0 {
			clone := recovered
			clone.Items = slices.Clone(recovered.Items)
			d.thread.Turns = append(d.thread.Turns, clone)
		} else {
			for _, item := range recovered.Items {
				d.thread.Turns[index].Items = mergeItem(d.thread.Turns[index].Items, item, false)
			}
		}
	}
}
func (d *Driver) Snapshot(ctx context.Context, thread string, path *string, offset int64) (harness.Snapshot, error) {
	snapshot, err := d.snapshot(ctx, thread, path, offset)
	if err == nil {
		snapshot.Usage = slices.Clone(d.usage)
	}
	return snapshot, err
}
func (d *Driver) snapshot(ctx context.Context, thread string, path *string, offset int64) (harness.Snapshot, error) {
	startOffset := offset
	notifications, dirty := d.rpc.drain()
	for _, n := range notifications {
		if n.Method == "thread/tokenUsage/updated" {
			if report, ok := parseUsage(n.Params); ok {
				d.usage = append(d.usage, report)
			}
		}
	}
	select {
	case <-d.rpc.done:
		return harness.Snapshot{}, harness.ErrUncertain
	default:
	}
	if d.historyOnly {
		return d.historySnapshot(ctx, thread, path)
	}
	full := d.thread == nil || dirty || !time.Now().Before(d.readAt) || slices.ContainsFunc(notifications, func(n notification) bool { return n.Method == "turn/completed" })
	if full {
		var out struct {
			Thread Thread `json:"thread"`
		}
		if err := d.rpc.Call(ctx, "thread/read", map[string]any{"threadId": thread, "includeTurns": true}, &out); err != nil {
			d.rpc.markDirty()
			if errors.Is(err, errResponseTooLarge) {
				d.historyOnly = true
				return d.historySnapshot(ctx, thread, path)
			}
			if e, ok := errors.AsType[*RPCError](err); ok && strings.Contains(strings.ToLower(e.NativeMessage), "not found") {
				return harness.Snapshot{}, harness.Failure("context_lost", "Harness context is unavailable.")
			}
			return harness.Snapshot{}, err
		}
		// A completion read may omit a tool whose start we already observed.
		// Retain its identity so an interrupted tool is reported as unknown.
		if d.thread != nil {
			for i := range out.Thread.Turns {
				turn := &out.Thread.Turns[i]
				previous := slices.IndexFunc(d.thread.Turns, func(t NativeTurn) bool { return t.ID == turn.ID })
				if previous >= 0 {
					for _, item := range d.thread.Turns[previous].Items {
						// A cached message may only be an item/started placeholder.
						// Its ID must not hide missing final text from journal recovery.
						if item.Type == "agentMessage" {
							continue
						}
						turn.Items = mergeItem(turn.Items, item, false)
					}
				}
			}
		}
		d.thread = &out.Thread
		d.readAt = time.Now().Add(30 * time.Second)
	}
	if d.thread.Path != nil {
		path = d.thread.Path
	}
	if path != nil && !d.indexLoaded {
		if offset > 0 {
			var index journalIndex
			if err := readNative(ctx, d.rpc.sandbox, "index", *path, offset, d.limit, &index); err != nil {
				return harness.Snapshot{}, err
			}
			maps.Copy(d.orders, index.Orders)
			maps.Copy(d.commands, index.Commands)
			for _, id := range index.Completed {
				d.completed[id] = true
				d.journalIDs[id] = true
			}
		}
		d.indexLoaded = true
	}
	if path != nil && (full || slices.ContainsFunc(notifications, func(n notification) bool { return n.Method == "item/completed" || n.Method == "turn/completed" })) {
		for {
			var batch journalBatch
			if err := readNative(ctx, d.rpc.sandbox, "journal", *path, offset, d.limit, &batch); err != nil {
				d.rpc.markDirty()
				return harness.Snapshot{}, err
			}
			previous := offset
			offset = batch.Offset
			for _, item := range batch.Items {
				switch item.Type {
				case "completed":
					d.completed[item.ID] = true
					d.journalIDs[item.ID] = true
				case "output":
					d.outputs[item.ID] = item.Output
				case "order":
					d.orders[item.ID] = item.StartedAtMS
				case "command":
					d.commands[item.ID] = item.Argv
				}
			}
			if offset == previous || batch.Exhausted {
				break
			}
		}
	}
	for _, n := range notifications {
		switch n.Method {
		case "turn/started", "turn/completed":
			var p struct {
				Turn NativeTurn `json:"turn"`
			}
			if json.Unmarshal(n.Params, &p) != nil {
				d.rpc.markDirty()
				continue
			}
			index := slices.IndexFunc(d.thread.Turns, func(t NativeTurn) bool { return t.ID == p.Turn.ID })
			if index < 0 {
				d.thread.Turns = append(d.thread.Turns, p.Turn)
			} else {
				t := &d.thread.Turns[index]
				if p.Turn.Status != "" && (!terminalTurn(t.Status) || terminalTurn(p.Turn.Status)) {
					t.Status = p.Turn.Status
				}
				if p.Turn.Error != nil {
					t.Error = p.Turn.Error
				}
				for _, item := range p.Turn.Items {
					if full && n.Method == "turn/started" && item.Type == "agentMessage" {
						continue
					}
					t.Items = mergeItem(t.Items, item, !full && n.Method == "turn/completed")
				}
			}
		case "item/started", "item/completed":
			var p struct {
				TurnID string     `json:"turnId"`
				Item   NativeItem `json:"item"`
			}
			if json.Unmarshal(n.Params, &p) != nil {
				d.rpc.markDirty()
				continue
			}
			if n.Method == "item/completed" {
				d.completed[p.Item.ID] = true
			}
			index := slices.IndexFunc(d.thread.Turns, func(t NativeTurn) bool { return t.ID == p.TurnID })
			if index < 0 {
				d.rpc.markDirty()
			} else {
				if full && n.Method == "item/started" && p.Item.Type == "agentMessage" {
					continue
				}
				d.thread.Turns[index].Items = mergeItem(d.thread.Turns[index].Items, p.Item, n.Method == "item/completed")
			}
		}
	}
	// A durable completion must not be hidden by an item/started placeholder.
	// Keep unfinished tools only when the journal has no completion for them.
	for i := range d.thread.Turns {
		d.thread.Turns[i].Items = slices.DeleteFunc(d.thread.Turns[i].Items, func(item NativeItem) bool {
			return item.Status == "inProgress" && d.journalIDs[item.ID] && slices.Contains(toolKinds, item.Type)
		})
	}
	d.mergeRecovered()
	snapshot := d.normalize()
	nativeItems := map[string]bool{}
	tools := map[string]bool{}
	for _, turn := range d.thread.Turns {
		for _, item := range turn.Items {
			nativeItems[item.ID] = true
		}
	}
	for _, turn := range snapshot.Turns {
		for _, item := range turn.Items {
			if item.Type == "tool" {
				tools[item.NativeID] = true
			}
		}
	}
	missing := false
	for id := range d.journalIDs {
		if !nativeItems[id] {
			missing = true
		}
	}
	for id := range d.outputs {
		if !tools[id] {
			missing = true
		}
	}
	if missing {
		d.divergenceReads++
		if d.divergenceReads >= 3 && path != nil {
			var history offlineHistory
			if err := readNative(ctx, d.rpc.sandbox, "offline", *path, 0, d.limit, &history); err != nil {
				return harness.Snapshot{}, err
			}
			for _, turn := range history.Turns {
				old := map[string]bool{}
				for _, item := range d.recovered[turn.ID].Items {
					old[item.ID] = true
				}
				items := []NativeItem{}
				for _, item := range turn.Items {
					if !nativeItems[item.ID] || old[item.ID] {
						items = append(items, item)
					}
				}
				turn.Items = items
				d.recovered[turn.ID] = turn
			}
			for _, id := range history.Completed {
				d.completed[id] = true
			}
			maps.Copy(d.outputs, history.Outputs)
			d.mergeRecovered()
			snapshot = d.normalize()
			represented := map[string]bool{}
			for _, turn := range snapshot.Turns {
				for _, item := range turn.Items {
					represented[item.NativeID] = true
				}
			}
			for id := range d.journalIDs {
				if !represented[id] {
					return harness.Snapshot{}, harness.Failure("harness_failed", "Native history cannot recover the missing items.")
				}
			}
			for id := range d.outputs {
				if !represented[id] {
					return harness.Snapshot{}, harness.Failure("harness_failed", "Native history cannot recover the missing items.")
				}
			}
			d.divergenceReads = 0
		} else {
			offset = startOffset
			d.rpc.markDirty()
			for i := range snapshot.Turns {
				if snapshot.Turns[i].Status.Terminal() {
					snapshot.Turns[i].Status = session.Running
				}
			}
		}
	} else {
		d.divergenceReads = 0
	}
	snapshot.Path = path
	snapshot.Offset = offset
	return snapshot, nil
}

func terminalTurn(status string) bool {
	return status == "completed" || status == "interrupted" || status == "failed"
}

// Once full RPC history exceeds the transport bound, observe the durable JSONL
// instead. Each tool result is bounded by the sandbox reader before transfer.
func (d *Driver) historySnapshot(ctx context.Context, thread string, path *string) (harness.Snapshot, error) {
	if path == nil && d.thread != nil {
		path = d.thread.Path
	}
	if path == nil {
		var out struct {
			Thread Thread `json:"thread"`
		}
		if err := d.rpc.Call(ctx, "thread/read", map[string]any{"threadId": thread, "includeTurns": false}, &out); err != nil {
			return harness.Snapshot{}, err
		}
		path = out.Thread.Path
	}
	if path == nil {
		return harness.Snapshot{}, harness.Failure("context_lost", "Harness history is unavailable.")
	}
	var history offlineHistory
	if err := readNative(ctx, d.rpc.sandbox, "offline", *path, 0, d.limit, &history); err != nil {
		d.rpc.markDirty()
		return harness.Snapshot{}, err
	}
	d.thread = &history.Thread
	d.thread.Path = path
	d.readAt = time.Now().Add(30 * time.Second)
	complete := map[string]bool{}
	for _, id := range history.Completed {
		complete[id] = true
	}
	snapshot := Normalize(history.Thread, complete, history.Outputs, d.limit, nil, nil)
	snapshot.Path, snapshot.Offset = path, history.Offset
	return snapshot, nil
}
