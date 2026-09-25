package store

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/accountlimits"
	"github.com/orpheus-agents/orpheus/internal/diagnostic"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

const accountLimitsStaleAfter = 300

type LimitItem struct {
	AccountID     string
	Profiles      []string
	State         string
	ObservedAt    *time.Time
	LastAttemptAt *time.Time
	ErrorCode     *string
	Buckets       []accountlimits.Bucket
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
	valid := false
	for _, account := range s.Profiles.Accounts() {
		if account.ID == id && account.Fingerprint == fingerprint {
			valid = true
			break
		}
	}
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
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return db.New(s.Pool).UpsertAccountLimitObservation(ctx, db.UpsertAccountLimitObservationParams{AccountID: id, SourceFingerprint: fingerprint, ObservedAt: at.UTC(), ErrorCode: errorCode, Snapshot: raw})
}

func (s *Store) AccountLimits(ctx context.Context) (LimitReport, error) {
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
		accounts := s.Profiles.Accounts()
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
		if requestCtx.Err() == nil {
			slog.WarnContext(requestCtx, "Account limits query failed", "error_type", diagnostic.Describe(err))
		}
		return LimitReport{}, accountLimitsUnavailable()
	}
	return out, nil
}
