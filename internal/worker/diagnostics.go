package worker

import (
	"errors"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/diagnostic"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/store"
)

// Bind the run observed by the failing operation, never a later active run.
func (e *Executor) bindFailure(err error, record store.SessionRecord, run *store.RunRecord) error {
	if err == nil {
		return nil
	}
	fields := diagnostic.Capture(err)
	if fields.SessionID == uuid.Nil {
		fields.SessionID = e.ID
		fields.Namespace = record.Namespace
		fields.SandboxID, fields.LaunchID, fields.ProcessID = record.SandboxID, record.LaunchID, record.ProcessID
		if run != nil {
			fields.RunID = &run.ID
			fields.Phase = "preparation"
			if run.ExecutionStartedAt != nil {
				fields.Phase = "execution"
			}
			if run.Phase != nil && *run.Phase == "after_run" {
				fields.Phase = "finalization"
			}
		}
	}
	if f, ok := errors.AsType[*harness.ExecutionError](err); ok {
		copy := *f
		copy.Cause = &diagnostic.Error{Cause: f.Cause, Fields: fields}
		return &copy
	}
	return diagnostic.With(err, fields)
}
