package store

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/accountlimits"
	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/diagnostic"
	"github.com/orpheus-agents/orpheus/internal/secret"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

const accountLimitsStaleAfter = 300

type LimitItem struct {
	AccountID             string
	Profiles              []string
	State                 string
	ObservedAt            *time.Time
	LastAttemptAt         *time.Time
	ErrorCode             *string
	Buckets               []accountlimits.Bucket
	ResetCreditsAvailable *int64
}

type LimitReport struct {
	AsOf              time.Time
	StaleAfterSeconds int
	Items             []LimitItem
}

func accountLimitsUnavailable() error {
	return session.Problem(503, "account_limits_unavailable", "Account limits are temporarily unavailable.")
}

func (s *Store) SaveAccountLimitObservation(ctx context.Context, id, fingerprint string, at time.Time, snapshot *accountlimits.Snapshot, errorCode *string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	accountID, err := uuid.Parse(id)
	if err != nil {
		return errors.New("invalid account limits identity")
	}
	row, err := db.New(s.Pool).ReadLLMAccount(ctx, accountID)
	if err != nil {
		return err
	}
	source, err := s.credentialSource(row)
	if err != nil {
		return err
	}
	if row.Provider == "kimi-code-plan-global" && row.AuthMode == "api_key" {
		k, err := db.New(s.Pool).GetSecret(ctx, *row.ApiKeySecretID)
		if err != nil {
			return err
		}
		source.KeyVersion = hex.EncodeToString(k.Nonce)
	}
	expected, err := config.SourceFingerprint(row.Harness, source)
	valid := err == nil && expected == fingerprint
	if !valid || (snapshot == nil) == (errorCode == nil) {
		return errors.New("invalid account limits observation")
	}
	var raw json.RawMessage
	if snapshot != nil {
		var err error
		raw, err = json.Marshal(snapshot)
		if err != nil {
			return err
		}
	}
	return db.New(s.Pool).UpsertAccountLimitObservation(ctx, db.UpsertAccountLimitObservationParams{AccountID: id, SourceFingerprint: fingerprint, ObservedAt: at.UTC(), ErrorCode: errorCode, Snapshot: raw})
}

func (s *Store) AccountLimits(ctx context.Context) (LimitReport, error) {
	return s.AccountLimitsFor(ctx, nil)
}

func (s *Store) AccountLimitsFor(ctx context.Context, selected []uuid.UUID) (LimitReport, error) {
	out := LimitReport{StaleAfterSeconds: accountLimitsStaleAfter, Items: []LimitItem{}}
	requestCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := s.Snapshot(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '3s'"); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, "SELECT transaction_timestamp()").Scan(&out.AsOf); err != nil {
			return err
		}
		out.AsOf = out.AsOf.UTC()
		accounts, err := s.accountSources(ctx, tx, selected)
		if err != nil {
			return err
		}
		if len(accounts) == 0 {
			return nil
		}
		ids := make([]string, len(accounts))
		for i, account := range accounts {
			ids[i] = account.ID
		}
		rows, err := db.New(tx).ListAccountLimitObservations(ctx, ids)
		if err != nil {
			return err
		}
		byKey := make(map[string]db.AccountLimitObservation, len(rows))
		for _, row := range rows {
			byKey[row.AccountID+"\x00"+row.SourceFingerprint] = row
		}
		for _, account := range accounts {
			item := LimitItem{AccountID: account.ID, Profiles: account.Profiles, State: "unknown", Buckets: []accountlimits.Bucket{}}
			if row, ok := byKey[account.ID+"\x00"+account.Fingerprint]; ok {
				item.LastAttemptAt = new(row.LastAttemptAt.UTC())
				item.ErrorCode = row.LastErrorCode
				if row.LastSuccessAt == nil {
					item.State = "unavailable"
				} else {
					item.ObservedAt = new(row.LastSuccessAt.UTC())
					var snapshot accountlimits.Snapshot
					if err := json.Unmarshal(row.Snapshot, &snapshot); err != nil {
						return err
					}
					item.Buckets = snapshot.Buckets
					item.ResetCreditsAvailable = snapshot.ResetCreditsAvailable
					if item.Buckets == nil {
						item.Buckets = []accountlimits.Bucket{}
					}
					item.State = "fresh"
					if item.ErrorCode != nil || !out.AsOf.Before(item.ObservedAt.Add(accountLimitsStaleAfter*time.Second)) || snapshot.ResetElapsed(out.AsOf) {
						item.State = "stale"
					}
				}
			}
			out.Items = append(out.Items, item)
		}
		return nil
	})
	if err != nil {
		if !errors.Is(requestCtx.Err(), context.Canceled) {
			slog.WarnContext(requestCtx, "Account limits query failed", "error_type", diagnostic.Describe(err))
		}
		return LimitReport{}, accountLimitsUnavailable()
	}
	return out, nil
}

func (s *Store) accountSources(ctx context.Context, q db.DBTX, selected []uuid.UUID) ([]config.Account, error) {
	rows, err := db.New(q).AccountSources(ctx, selected)
	if err != nil {
		return nil, err
	}
	out := make([]config.Account, 0, len(rows))
	for _, row := range rows {
		source, err := s.credentialSource(db.LlmAccount{ID: row.ID, Harness: row.Harness, Provider: row.Provider, AuthMode: row.AuthMode, CredentialStore: row.CredentialStore, CredentialKey: row.CredentialKey})
		if err != nil {
			return nil, err
		}
		source.KeyVersion = row.KeyVersion
		fingerprint, err := config.SourceFingerprint(row.Harness, source)
		if err != nil {
			return nil, err
		}
		out = append(out, config.Account{ID: row.ID.String(), Profiles: row.Profiles, Fingerprint: fingerprint})
	}
	return out, nil
}

// KimiLimitCredential is consumed only by the worker. It must never be returned in API responses.
type KimiLimitCredential struct {
	AccountID   string
	Fingerprint string
	Key         string
}

func (s *Store) KimiLimitCredentials(ctx context.Context) ([]KimiLimitCredential, error) {
	rows, err := db.New(s.Pool).KimiLimitCredentials(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]KimiLimitCredential, 0, len(rows))
	for _, r := range rows {
		source := session.Credentials{AccountID: r.ID.String(), Provider: "kimi-code-plan-global", Mode: "api_key", KeyVersion: hex.EncodeToString(r.Nonce)}
		fp, err := config.SourceFingerprint(r.Harness, source)
		if err != nil {
			return nil, err
		}
		var key string
		if s.Cipher != nil {
			// Return an unavailable credential as an empty key so one unreadable
			// account cannot suppress observations for the rest of the catalog.
			key, err = s.Cipher.DecryptAccount(r.ID, &secret.Value{Nonce: r.Nonce, Ciphertext: r.Ciphertext})
			if err != nil {
				key = ""
			}
		}
		out = append(out, KimiLimitCredential{AccountID: source.AccountID, Fingerprint: fp, Key: key})
	}
	return out, nil
}
