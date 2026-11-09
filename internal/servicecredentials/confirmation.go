package servicecredentials

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/orpheus-agents/orpheus/internal/session"
)

// Witness binds a fresh source check to the exact frozen context. It carries no
// instructions, transport routing, or provider-specific permission calculation.
type Witness struct {
	ContextDigest string    `json:"context_digest"`
	ObservedAt    time.Time `json:"observed_at"`
	Confirmed     bool      `json:"confirmed"`
}

func (c Client) confirm(ctx context.Context, cfg Config, access session.AccessContext) (Witness, error) {
	denied := session.Problem(403, "audience_unconfirmed", "Source did not confirm the invocation context and audience.")
	if cfg.Confirmation == nil || cfg.Validate() != nil || access.Validate() != nil || access.Source == nil {
		return Witness{}, denied
	}
	raw, err := json.Marshal(access)
	if err != nil {
		return Witness{}, denied
	}
	digest := sha256.Sum256(raw)
	expected := hex.EncodeToString(digest[:])
	// The authority is fixed by trusted service configuration. Neither the model
	// nor the submitted source context can choose the confirmation endpoint/key.
	authority := Config{Endpoint: cfg.Confirmation.Endpoint, SecretEnv: cfg.Confirmation.SecretEnv, TokenFile: cfg.TokenFile}
	response, err := c.request(ctx, authority, struct {
		Operation     string          `json:"operation"`
		Context       json.RawMessage `json:"context"`
		ContextDigest string          `json:"context_digest"`
	}{"confirm", raw, expected})
	if err != nil {
		return Witness{}, denied
	}
	var witness Witness
	decoder := json.NewDecoder(bytes.NewReader(response))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&witness) != nil || !errors.Is(decoder.Decode(new(any)), io.EOF) {
		return Witness{}, denied
	}
	now := time.Now()
	if !witness.Confirmed || witness.ContextDigest != expected || witness.ObservedAt.IsZero() || witness.ObservedAt.Before(now.Add(-30*time.Second)) || witness.ObservedAt.After(now.Add(maxProviderClockSkew)) {
		return Witness{}, denied
	}
	return witness, nil
}
