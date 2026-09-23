package store

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

type deliveryParameters struct {
	PreviousTurns []string `json:"previous_turns"`
	PreviousItems []string `json:"previous_items"`
}
type toolMeta struct {
	ToolRecord
	HasResult bool `json:"has_result"`
}

func replaceResult(item harness.Item, tool *ToolRecord, has bool) bool {
	if !has || item.Completeness == "complete" {
		return true
	}
	var result struct {
		OriginalBytes *int `json:"original_bytes"`
	}
	_ = json.Unmarshal(item.Result, &result)
	return item.TruncationReason != nil && *item.TruncationReason == "orpheus_limit" && result.OriginalBytes != nil && tool != nil && tool.OutputCompleteness != "complete"
}
func toolStateEqual(a, b ToolRecord) bool {
	return reflect.DeepEqual(a.Position, b.Position) && a.Name == b.Name && jsonEqual(a.Input, b.Input) && a.Status == b.Status
}
func toolNext(before ToolRecord, item harness.Item, position *session.Position) ToolRecord {
	next := before
	next.Position, next.Name, next.Input, next.Status = position, item.Name, item.Input, item.Status
	return next
}
func resultChanged(item harness.Item, meta *toolMeta) bool {
	return replaceResult(item, &meta.ToolRecord, meta.HasResult) && (resultDigest(item.Result) != meta.ResultDigest || item.Completeness != meta.OutputCompleteness || !reflect.DeepEqual(item.TruncationReason, meta.TruncationReason))
}
func Reconcile(ctx context.Context, tx pgx.Tx, record *SessionRecord, snapshot harness.Snapshot) error {
	turnIDs := make([]string, 0, len(snapshot.Turns))
	messageKeys := []string{}
	toolKeys := []string{}
	thread := ""
	if record.ThreadID != nil {
		thread = *record.ThreadID
	}
	for _, turn := range snapshot.Turns {
		turnIDs = append(turnIDs, turn.NativeID)
		for _, item := range turn.Items {
			key := thread + ":" + turn.NativeID + ":" + item.NativeID
			if item.Type == "user" || item.Type == "assistant" {
				messageKeys = append(messageKeys, key)
			} else {
				toolKeys = append(toolKeys, key)
			}
		}
	}
	runs, err := runRecords(db.New(tx).ReconcileRuns(ctx, db.ReconcileRunsParams{SessionID: record.ID, TurnIds: turnIDs}))
	if err != nil {
		return err
	}
	messages, err := messageRecords(db.New(tx).ReconcileMessages(ctx, db.ReconcileMessagesParams{SessionID: record.ID, NativeKeys: messageKeys}))
	if err != nil {
		return err
	}
	byID := map[uuid.UUID]*MessageRecord{}
	byNative := map[string]*MessageRecord{}
	for i := range messages {
		m := &messages[i]
		byID[m.ID] = m
		if m.NativeKey != nil {
			byNative[*m.NativeKey] = m
		}
	}
	// Select metadata without loading the potentially large result column.
	metadata, err := toolMetadataRecords(db.New(tx).ToolMetadata(ctx, db.ToolMetadataParams{SessionID: record.ID, NativeKeys: toolKeys}))
	if err != nil {
		return err
	}
	tools := map[string]*toolMeta{}
	for i := range metadata {
		tools[*metadata[i].NativeKey] = &metadata[i]
	}
	runIDs := make([]uuid.UUID, 0, len(runs))
	for _, r := range runs {
		runIDs = append(runIDs, r.ID)
	}
	operations, err := db.New(tx).ReconcileOperations(ctx, db.ReconcileOperationsParams{SessionID: record.ID, RunIds: runIDs})
	if err != nil {
		return err
	}
	byMessage := map[uuid.UUID]*Operation{}
	starts := map[uuid.UUID]*Operation{}
	for i := range operations {
		o := &operations[i]
		if o.MessageID != nil {
			byMessage[*o.MessageID] = o
		}
		if o.Kind == "start" && o.RunID != nil && (o.Status == "sending" || o.Status == "uncertain" || o.Status == "confirmed") {
			starts[*o.RunID] = o
		}
	}
	previousOperations := slices.Clone(operations)
	byTurn := map[string]*RunRecord{}
	for i := range runs {
		r := &runs[i]
		if r.NativeTurnID != nil {
			byTurn[*r.NativeTurnID] = r
			continue
		}
		if r.Status.Terminal() {
			continue
		}
		o := starts[r.ID]
		if o == nil || o.MessageID == nil {
			continue
		}
		initial := byID[*o.MessageID]
		if initial == nil {
			continue
		}
		var params deliveryParameters
		if err := json.Unmarshal(o.Parameters, &params); err != nil {
			return err
		}
		candidates := []string{}
		for _, t := range snapshot.Turns {
			if !slices.Contains(params.PreviousTurns, t.NativeID) && slices.ContainsFunc(t.Items, func(item harness.Item) bool { return item.Type == "user" && item.Text == initial.Text }) {
				candidates = append(candidates, t.NativeID)
			}
		}
		if len(candidates) == 1 {
			r.NativeTurnID = &candidates[0]
			o.Status = "confirmed"
			o.Result, _ = json.Marshal(map[string]string{"turn_id": candidates[0]})
			byTurn[candidates[0]] = r
			if err := SaveRun(ctx, tx, r); err != nil {
				return err
			}
		}
	}
	// Fetch changed outputs together; unchanged outputs stay in PostgreSQL.
	fullIDs := []uuid.UUID{}
	for _, turn := range snapshot.Turns {
		r := byTurn[turn.NativeID]
		if r == nil {
			continue
		}
		for _, item := range turn.Items {
			meta := tools[thread+":"+turn.NativeID+":"+item.NativeID]
			if meta == nil || !meta.HasResult {
				continue
			}
			next := toolNext(meta.ToolRecord, item, &session.Position{RunNumber: r.Number, ItemIndex: item.Index})
			if resultChanged(item, meta) || !toolStateEqual(meta.ToolRecord, next) {
				fullIDs = append(fullIDs, meta.ID)
			}
		}
	}
	if len(fullIDs) > 0 {
		full, err := toolRecords(db.New(tx).ToolsByID(ctx, fullIDs))
		if err != nil {
			return err
		}
		for _, tool := range full {
			tools[*tool.NativeKey].Result = tool.Result
		}
	}
	writes := projectionBatch{DBTX: tx}
	for _, turn := range snapshot.Turns {
		r := byTurn[turn.NativeID]
		if r == nil {
			continue
		}
		oldStatus, oldObservation := r.Status, r.Observation
		for _, item := range turn.Items {
			nativeKey := thread + ":" + turn.NativeID + ":" + item.NativeID
			position := &session.Position{RunNumber: r.Number, ItemIndex: item.Index}
			if item.Type == "user" || item.Type == "assistant" {
				message := byNative[nativeKey]
				if message == nil && item.Type == "user" {
					for i := range messages {
						candidate := &messages[i]
						if candidate.RunID != r.ID || candidate.Role != "user" || candidate.NativeKey != nil || candidate.DeliveryStatus == nil || !slices.Contains([]string{"sending", "uncertain", "delivered"}, *candidate.DeliveryStatus) {
							continue
						}
						o := byMessage[candidate.ID]
						if o != nil && candidate.Text == item.Text {
							var params deliveryParameters
							if err := json.Unmarshal(o.Parameters, &params); err != nil {
								return err
							}
							if !slices.Contains(params.PreviousItems, item.NativeID) {
								message = candidate
								o.Status = "confirmed"
							}
						}
						break
					}
				}
				created := message == nil
				if created {
					if item.Type == "user" {
						continue
					}
					message = &MessageRecord{ID: uuid.New(), SessionID: record.ID, RunID: r.ID, Role: "assistant"}
				}
				before := *message
				message.NativeKey = &nativeKey
				message.Position = position
				byNative[nativeKey] = message
				if message.Role == "user" {
					message.DeliveryStatus = new("delivered")
					message.Error = nil
				} else {
					message.Text = item.Text
					message.Kind = item.Kind
				}
				if created || !reflect.DeepEqual(before, *message) {
					if err := PublishMessage(ctx, &writes, record, message); err != nil {
						return err
					}
				}
			} else {
				meta := tools[nativeKey]
				created := meta == nil
				if created {
					meta = &toolMeta{ID: uuid.New(), SessionID: record.ID, RunID: r.ID, NativeKey: &nativeKey}
					tools[nativeKey] = meta
				}
				before := meta.ToolRecord
				next := toolNext(before, item, position)
				replace := resultChanged(item, meta)
				if replace {
					next.Result = item.Result
					next.OutputCompleteness = item.Completeness
					next.TruncationReason = item.TruncationReason
				}
				changed := created || !toolStateEqual(before, next) || !jsonEqual(before.Result, next.Result) || before.OutputCompleteness != next.OutputCompleteness || !reflect.DeepEqual(before.TruncationReason, next.TruncationReason)
				if changed {
					if err := PublishTool(ctx, &writes, record, &next); err != nil {
						return err
					}
				}
				meta.ToolRecord = next
				meta.HasResult = meta.HasResult || len(next.Result) > 0
			}
		}
		if err := writes.flush(ctx, tx); err != nil {
			return err
		}
		if !r.Status.Terminal() {
			if turn.Status.Terminal() && r.AgentStatus == nil {
				var problem *session.Error
				if turn.Status == session.Failed {
					code := turn.ErrorCode
					if code == "" {
						code = "harness_failed"
					}
					problem = &session.Error{Code: code, Message: "Harness execution failed.", Phase: new("execution"), Details: []session.Detail{}}
				}
				if err := AgentFinished(ctx, tx, record, r, turn.Status, problem, nil); err != nil {
					return err
				}
			} else if !turn.Status.Terminal() {
				if r.Status != session.Cancelling {
					r.Status = session.Running
				}
				pending := slices.ContainsFunc(operations, func(o Operation) bool {
					return o.RunID != nil && *o.RunID == r.ID && (o.Status == "sending" || o.Status == "uncertain")
				})
				r.Observation = new("attached")
				if pending {
					r.Observation = new("uncertain")
				}
				if oldStatus != r.Status || !reflect.DeepEqual(oldObservation, r.Observation) {
					if err := PublishRun(ctx, tx, record, r); err != nil {
						return err
					}
				}
			}
		}
	}
	for i := range operations {
		if !reflect.DeepEqual(operations[i], previousOperations[i]) {
			if err := SaveOperation(ctx, &writes, &operations[i]); err != nil {
				return err
			}
		}
	}
	if err := writes.flush(ctx, tx); err != nil {
		return err
	}
	record.HistoryPath = snapshot.Path
	record.HistoryOffset = snapshot.Offset
	return nil
}
