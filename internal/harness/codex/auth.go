package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"

	"github.com/orpheus-agents/orpheus/internal/harness"
)

const maxAttachedAuthBytes = 1 << 20

// Register local credentials before RPC, including a login interrupted before
// its durable initialized marker. A first login can start without an auth file.
func (d *Driver) rememberAuth(ctx context.Context, required bool) error {
	failure := harness.Failure("credentials_unavailable", "Running harness credentials are unavailable.")
	home, err := d.StateDir(ctx)
	if err != nil {
		return failure
	}
	file, err := d.rpc.sandbox.Read(ctx, home+"/auth.json")
	if !required && errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return failure
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxAttachedAuthBytes+1))
	closeErr := file.Close()
	if err != nil || closeErr != nil || len(raw) > maxAttachedAuthBytes {
		return failure
	}
	var auth struct {
		APIKey string `json:"OPENAI_API_KEY"`
		Tokens struct {
			Access  string `json:"access_token"`
			Refresh string `json:"refresh_token"`
			ID      string `json:"id_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(raw, &auth) != nil {
		return failure
	}
	if auth.APIKey == "" && (auth.Tokens.Access == "" || auth.Tokens.Refresh == "" || auth.Tokens.ID == "") {
		return failure
	}
	d.rpc.redactor.Add(auth.APIKey, auth.Tokens.Access, auth.Tokens.Refresh, auth.Tokens.ID)
	return nil
}
