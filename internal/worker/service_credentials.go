package worker

import (
	"context"
	"errors"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/servicecredentials"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
)

// Called on every tick before hooks/harness. Restoration loads the original grant;
// it never calls issue, and a dead lease never resumes the old model context.
func (e *Executor) serviceCredentials(ctx context.Context, record store.SessionRecord, run store.RunRecord) (bool, error) {
	state, err := e.Store.ServiceCredentials(ctx, e.ID, run.ID)
	if err != nil {
		return false, err
	}
	if len(state.Entries) == 0 {
		return false, nil
	}
	if run.CancelRequestedAt != nil {
		if e.sandbox != nil {
			return false, e.removeCredentialFiles(ctx, state)
		}
		return false, nil
	}
	if state.Expired(time.Now()) {
		return true, e.expireCredentials(ctx, record, run, state)
	}
	if e.credentialsRetryAt == nil || e.credentialsInvocationID != state.Context.InvocationID {
		e.credentialsRetryAt = map[string]time.Time{}
		e.credentialsInvocationID = state.Context.InvocationID
	}
	changed := false
	for code, entry := range state.Entries {
		if time.Until(entry.Grant.ExpiresAt) > 60*time.Second || entry.Grant.ExpiresAt.Equal(entry.Grant.RunExpiresAt) || time.Now().Before(e.credentialsRetryAt[code]) {
			continue
		}
		e.credentialsRetryAt[code] = time.Now().Add(10 * time.Second)
		grant, err := e.Store.CredentialClient.Renew(ctx, entry.Config, code, state.Context.InvocationID, entry.Grant)
		if err != nil {
			continue
		} // Retain the original deadline; no permanent-key fallback.
		entry.Grant = grant
		state.Entries[code] = entry
		changed = true
	}
	if changed {
		if err := e.Store.SaveServiceCredentials(ctx, e.ID, run.ID, state); err != nil {
			return false, err
		}
	}
	if state.Expired(time.Now()) {
		return true, e.expireCredentials(ctx, record, run, state)
	}
	if e.sandbox == nil {
		return false, nil
	}
	if e.credentialsRunID != run.ID {
		for _, entry := range state.Entries {
			if err := e.installCredentialFile(ctx, entry); err != nil {
				return false, err
			}
		}
		e.credentialsRunID = run.ID
	}
	return false, nil
}
func (e *Executor) installCredentialFile(ctx context.Context, entry servicecredentials.Entry) error {
	if entry.Config.Validate() != nil {
		return harness.Failure("service_credentials_unavailable", "Invalid stored provider configuration.")
	}
	relative := strings.TrimPrefix(entry.Config.TokenFile, "~/")
	dir := path.Dir(relative)
	command := `set -eu; umask 077; d="$HOME/` + dir + `"; test ! -L "$d"; mkdir -p "$d"; chmod 700 "$d"; mktemp "$d/.run.XXXXXXXX"`
	raw, err := e.sandbox.Run(ctx, command)
	if err != nil {
		return err
	}
	temporary := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(temporary, "/") || strings.ContainsAny(temporary, "\x00\r\n") || !strings.HasPrefix(path.Base(temporary), ".run.") || !strings.HasSuffix(path.Dir(temporary), "/"+dir) {
		return harness.Failure("service_credentials_unavailable", "Token staging path is unavailable.")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = e.sandbox.Run(cleanup, "rm -f -- "+harness.Quote(temporary))
	}()
	if err := e.sandbox.Write(ctx, temporary, []byte(entry.Grant.AccessToken)); err != nil {
		return err
	}
	_, err = e.sandbox.Run(ctx, "set -eu; chmod 600 "+harness.Quote(temporary)+"; mv -f -- "+harness.Quote(temporary)+" "+harness.Quote(path.Join(path.Dir(temporary), "run.token")))
	return err
}
func (e *Executor) removeCredentialFiles(ctx context.Context, state servicecredentials.State) error {
	for _, entry := range state.Entries {
		if entry.Config.Validate() != nil {
			return errors.New("invalid stored credential target")
		}
		relative := strings.TrimPrefix(entry.Config.TokenFile, "~/")
		if _, err := e.sandbox.Run(ctx, `set -eu; d="$HOME/`+path.Dir(relative)+`"; test ! -L "$d"; rm -f -- "$d/run.token"`); err != nil {
			return err
		}
	}
	e.credentialsRunID = uuid.Nil
	return nil
}
func (e *Executor) expireCredentials(ctx context.Context, record store.SessionRecord, run store.RunRecord, state servicecredentials.State) error {
	if record.SandboxID != nil {
		if err := e.ensureSandbox(ctx, &record, &run); err != nil {
			return err
		}
		if err := e.removeCredentialFiles(ctx, state); err != nil {
			return err
		}
		// Pause also stops an in-flight hook. Do not resume unconfirmed model memory
		// after an absolute deadline, even if a new grant would now be available.
		if err := e.sandbox.Pause(ctx); err != nil {
			return err
		}
	}
	return e.Store.Mutate(ctx, e.ID, false, func(tx pgx.Tx, r *store.SessionRecord) error {
		current, err := store.GetRun(ctx, tx, e.ID, run.ID)
		if err != nil {
			return err
		}
		if current.Status.Terminal() {
			return nil
		}
		problem := &session.Error{Code: "service_credentials_expired", Message: "Service credential lease or absolute deadline expired.", Details: []session.Detail{}}
		activeHook := ""
		if current.Phase != nil {
			activeHook = *current.Phase
		}
		if err := store.FailUnfinishedHooks(ctx, tx, run.ID, activeHook, problem); err != nil {
			return err
		}
		if err := store.UpdateSandbox(ctx, tx, r, func(r *store.SessionRecord) {
			r.SandboxState = "unavailable"
			r.SandboxLastKnownState = new("paused")
			r.SandboxError = problem
		}); err != nil {
			return err
		}
		return store.Finish(ctx, tx, r, &current, session.Failed, problem, nil)
	})
}
func (e *Executor) cleanLastCredentials(ctx context.Context) error {
	last, err := e.Store.LastTerminalRun(ctx, e.ID)
	if err != nil {
		return err
	}
	state, err := e.Store.ServiceCredentials(ctx, e.ID, last.ID)
	if err != nil {
		return err
	}
	if len(state.Entries) == 0 {
		return nil
	}
	return e.removeCredentialFiles(ctx, state)
}
