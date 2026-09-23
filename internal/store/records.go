package store

import (
	"strconv"

	"github.com/skillum-ai/orpheus/internal/session"

	"github.com/skillum-ai/orpheus/internal/store/db"
)

func sessionRecord(v db.Session, err error) (SessionRecord, error) { return SessionRecord(v), err }
func runRecord(v db.Run, err error) (RunRecord, error) {
	return RunRecord{
		Usage:              session.Usage{InputTokens: v.InputTokens, OutputTokens: v.OutputTokens, TotalTokens: v.TotalTokens},
		InputFingerprint:   v.InputFingerprint,
		EnvNames:           v.EnvNames,
		EnvFrom:            v.EnvFrom,
		ID:                 v.ID,
		SessionID:          v.SessionID,
		Number:             v.Number,
		Status:             v.Status,
		Observation:        v.Observation,
		CreatedAt:          v.CreatedAt,
		ExecutionStartedAt: v.ExecutionStartedAt,
		DeadlineAt:         v.DeadlineAt,
		FinishedAt:         v.FinishedAt,
		CancelRequestedAt:  v.CancelRequestedAt,
		StopReason:         v.StopReason,
		StopMethod:         v.StopMethod,
		Error:              v.Error,
		CancelAttemptedAt:  v.CancelAttemptedAt,
		NativeTurnID:       v.NativeTurnID,
		NextDeliveryNumber: v.NextDeliveryNumber,
		FinalMessageID:     v.FinalMessageID,
		EnvCiphertext:      v.EnvCiphertext,
		Phase:              v.Phase,
		AgentStatus:        v.AgentStatus,
		AgentError:         v.AgentError,
	}, err
}
func messageRecord(v db.Message, err error) (MessageRecord, error) {
	return MessageRecord{
		ExternalKey:        v.ExternalKey,
		ID:                 v.ID,
		SessionID:          v.SessionID,
		RunID:              v.RunID,
		Role:               v.Role,
		Kind:               v.Kind,
		Text:               v.Text,
		DeliveryStatus:     v.DeliveryStatus,
		Error:              v.Error,
		RegisteredSequence: strconv.FormatInt(v.RegisteredSequence, 10),
		Position:           v.Position,
		CreatedAt:          v.CreatedAt,
		DeliveryNumber:     v.DeliveryNumber,
		NativeKey:          v.NativeKey,
	}, err
}
func toolRecord(v db.ToolCall, err error) (ToolRecord, error) {
	return ToolRecord{
		ID:                 v.ID,
		SessionID:          v.SessionID,
		RunID:              v.RunID,
		Name:               v.Name,
		Input:              v.Input,
		Status:             v.Status,
		Result:             v.Result,
		OutputCompleteness: v.OutputCompleteness,
		TruncationReason:   v.TruncationReason,
		RegisteredSequence: strconv.FormatInt(v.RegisteredSequence, 10),
		Position:           v.Position,
		CreatedAt:          v.CreatedAt,
		NativeKey:          v.NativeKey,
		ResultDigest:       v.ResultDigest,
	}, err
}
func sessionRecords(rows []db.Session, err error) ([]SessionRecord, error) {
	if err != nil {
		return nil, err
	}
	out := make([]SessionRecord, len(rows))
	for i, row := range rows {
		out[i], _ = sessionRecord(row, nil)
	}
	return out, nil
}
func runRecords(rows []db.Run, err error) ([]RunRecord, error) {
	if err != nil {
		return nil, err
	}
	out := make([]RunRecord, len(rows))
	for i, row := range rows {
		out[i], _ = runRecord(row, nil)
	}
	return out, nil
}
func messageRecords(rows []db.Message, err error) ([]MessageRecord, error) {
	if err != nil {
		return nil, err
	}
	out := make([]MessageRecord, len(rows))
	for i, row := range rows {
		out[i], _ = messageRecord(row, nil)
	}
	return out, nil
}
func toolRecords(rows []db.ToolCall, err error) ([]ToolRecord, error) {
	if err != nil {
		return nil, err
	}
	out := make([]ToolRecord, len(rows))
	for i, row := range rows {
		out[i], _ = toolRecord(row, nil)
	}
	return out, nil
}
func toolMetadataRecords(rows []db.ToolMetadataRow, err error) ([]toolMeta, error) {
	if err != nil {
		return nil, err
	}
	out := make([]toolMeta, len(rows))
	for i, v := range rows {
		tool, _ := toolRecord(db.ToolCall{
			ID:                 v.ID,
			SessionID:          v.SessionID,
			RunID:              v.RunID,
			Name:               v.Name,
			Input:              v.Input,
			Status:             v.Status,
			OutputCompleteness: v.OutputCompleteness,
			TruncationReason:   v.TruncationReason,
			NativeKey:          v.NativeKey,
			RegisteredSequence: v.RegisteredSequence,
			Position:           v.Position,
			CreatedAt:          v.CreatedAt,
			ResultDigest:       v.ResultDigest,
		}, nil)
		out[i] = toolMeta{ToolRecord: tool, HasResult: v.HasResult}
	}
	return out, nil
}
