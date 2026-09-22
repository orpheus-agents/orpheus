package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/skillum-ai/orpheus/internal/store/db"
)

// projectionBatch queues writes within the caller's transaction. A failed flush
// rolls back entity changes and their events together. Flush before reading any
// projection written by the batch (in particular, before choosing a final answer).
type projectionBatch struct {
	db.DBTX
	batch pgx.Batch
}

func (b *projectionBatch) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	b.batch.Queue(sql, args...)
	return pgconn.CommandTag{}, nil
}
func (b *projectionBatch) flush(ctx context.Context, tx pgx.Tx) error {
	if b.batch.Len() == 0 {
		return nil
	}
	err := tx.SendBatch(ctx, &b.batch).Close()
	b.batch = pgx.Batch{}
	return err
}

// Store a digest with each output so unchanged native results can be compared
// without fetching or detoasting PostgreSQL's potentially large result column.
func resultDigest(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if normalized, err := canonicalJSON(raw); err == nil {
		raw = normalized
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
