package store

import (
	"context"
	"errors"
	"log/slog"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/analytics"
	"github.com/orpheus-agents/orpheus/internal/diagnostic"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

// PostgreSQL 16's four-argument generate_series preserves local day boundaries
// and distinguishes the two occurrences of a repeated DST hour. sqlc's SQL
// analyzer does not yet recognize that overload, so this query uses pgx.
const analyticsBucketsSQL = `
WITH boundaries AS (
  SELECT boundary, LEAD(boundary) OVER (ORDER BY boundary) AS next_boundary
  FROM generate_series(
    date_trunc($1::text, $2::timestamptz, $4::text),
    $3::timestamptz + interval '2 days',
    CASE WHEN $1::text = 'hour' THEN interval '1 hour' ELSE interval '1 day' END,
    $4::text
  ) AS boundary
), windows AS (
  SELECT GREATEST(boundary, $2::timestamptz) AS bucket_from,
         LEAST(next_boundary, $3::timestamptz) AS bucket_to
  FROM boundaries WHERE boundary < $3::timestamptz AND next_boundary > $2::timestamptz
)
SELECT w.bucket_from, w.bucket_to, COUNT(r.id)::bigint,
       COUNT(r.id) FILTER (WHERE r.status = 'accepted')::bigint,
       COUNT(r.id) FILTER (WHERE r.status = 'starting')::bigint,
       COUNT(r.id) FILTER (WHERE r.status = 'running')::bigint,
       COUNT(r.id) FILTER (WHERE r.status = 'cancelling')::bigint,
       COUNT(r.id) FILTER (WHERE r.status = 'finalizing')::bigint,
       COUNT(r.id) FILTER (WHERE r.status = 'completed')::bigint,
       COUNT(r.id) FILTER (WHERE r.status = 'failed')::bigint,
       COUNT(r.id) FILTER (WHERE r.status = 'cancelled')::bigint,
       COALESCE(SUM(r.input_tokens), 0)::text,
       COALESCE(SUM(r.output_tokens), 0)::text,
       COALESCE(SUM(r.total_tokens), 0)::text,
       COALESCE(SUM(GREATEST(0, EXTRACT(EPOCH FROM LEAST(COALESCE(r.finished_at, $5::timestamptz), $5::timestamptz) - r.created_at))), 0)::double precision
FROM windows w
LEFT JOIN LATERAL (
  SELECT r.id, r.status, r.input_tokens, r.output_tokens, r.total_tokens, r.created_at, r.finished_at
  FROM runs r JOIN sessions s ON s.id = r.session_id
  WHERE r.created_at >= w.bucket_from AND r.created_at < w.bucket_to
    AND ($6::text IS NULL OR s.namespace = $6::text)
) r ON true
GROUP BY w.bucket_from, w.bucket_to ORDER BY w.bucket_from`

const analyticsBucketCountSQL = `SELECT count(*)::bigint FROM generate_series(
  date_trunc($1::text, $2::timestamptz, $4::text), $3::timestamptz,
  CASE WHEN $1::text = 'hour' THEN interval '1 hour' ELSE interval '1 day' END,
  $4::text) AS boundary WHERE boundary < $3::timestamptz`

func analyticsUnavailable() error {
	return session.Problem(503, "analytics_unavailable", "Analytics are temporarily unavailable.")
}

func analyticsOverflow() error {
	return session.Problem(503, "analytics_overflow", "Analytics count exceeds the supported range.")
}

func (s *Store) AnalyticsOverview(ctx context.Context, request analytics.Request) (analytics.Overview, error) {
	var out analytics.Overview
	requestCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := s.Snapshot(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '3s'"); err != nil {
			return err
		}
		queries := db.New(tx)
		asOf, err := queries.AnalyticsAsOf(ctx)
		if err != nil {
			return err
		}
		q, err := request.Resolve(asOf)
		if err != nil {
			return err
		}
		var validTimezone bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_timezone_names WHERE name = $1)", q.Timezone).Scan(&validTimezone); err != nil {
			return err
		}
		if !validTimezone {
			return analytics.Invalid("timezone")
		}
		// The 31-day limit currently stays below 800 hourly buckets; keep the
		// explicit bound at the SQL edge if supported ranges change later.
		var bucketCount int64
		if err := tx.QueryRow(ctx, analyticsBucketCountSQL, q.Bucket, q.From, q.To, q.Timezone).Scan(&bucketCount); err != nil {
			return err
		}
		if bucketCount > 800 {
			return analytics.Invalid("bucket")
		}
		out.AsOf, out.From, out.To, out.Bucket, out.Timezone, out.Namespace = q.AsOf, q.From, q.To, q.Bucket, q.Timezone, q.Namespace
		out.Current.ActiveSessions, err = queries.AnalyticsActiveSessions(ctx, q.Namespace)
		if err != nil {
			return err
		}
		if out.Current.ActiveSessions > analytics.MaxCount {
			return analyticsOverflow()
		}
		out.Period.Usage = analytics.Usage{InputTokens: "0", OutputTokens: "0", TotalTokens: "0"}
		out.Series = []analytics.Bucket{}
		rows, err := tx.Query(ctx, analyticsBucketsSQL, q.Bucket, q.From, q.To, q.Timezone, q.AsOf, q.Namespace)
		if err != nil {
			return err
		}
		defer rows.Close()
		input, output, total := new(big.Int), new(big.Int), new(big.Int)
		for rows.Next() {
			var bucket analytics.Bucket
			var rawInput, rawOutput, rawTotal string
			var runtimeSeconds float64
			if err := rows.Scan(&bucket.From, &bucket.To, &bucket.RunsCount,
				&bucket.ByStatus.Accepted, &bucket.ByStatus.Starting, &bucket.ByStatus.Running,
				&bucket.ByStatus.Cancelling, &bucket.ByStatus.Finalizing, &bucket.ByStatus.Completed,
				&bucket.ByStatus.Failed, &bucket.ByStatus.Cancelled,
				&rawInput, &rawOutput, &rawTotal, &runtimeSeconds); err != nil {
				return err
			}
			if bucket.RunsCount > analytics.MaxCount-out.Period.RunsCount {
				return analyticsOverflow()
			}
			bucket.From, bucket.To = bucket.From.UTC(), bucket.To.UTC()
			out.Period.RunsCount += bucket.RunsCount
			out.Period.ByStatus.Add(bucket.ByStatus)
			out.Period.RuntimeSeconds += runtimeSeconds
			if err := addDecimal(input, rawInput); err != nil {
				return err
			}
			if err := addDecimal(output, rawOutput); err != nil {
				return err
			}
			if err := addDecimal(total, rawTotal); err != nil {
				return err
			}
			out.Series = append(out.Series, bucket)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		out.Period.Usage = analytics.Usage{InputTokens: input.String(), OutputTokens: output.String(), TotalTokens: total.String()}
		return nil
	})
	if err != nil {
		if _, ok := errors.AsType[*session.APIError](err); ok {
			return out, err
		}
		if requestCtx.Err() == nil {
			slog.WarnContext(requestCtx, "Analytics query failed", "error_type", diagnostic.Describe(err))
		}
		return analytics.Overview{}, analyticsUnavailable()
	}
	return out, nil
}

func addDecimal(sum *big.Int, value string) error {
	v, ok := new(big.Int).SetString(value, 10)
	if !ok || v.Sign() < 0 {
		return analyticsUnavailable()
	}
	sum.Add(sum, v)
	return nil
}
