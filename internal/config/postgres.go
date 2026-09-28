package config

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	databaseConnectTimeout = 5 * time.Second
	databaseQueryTimeout   = 30 * time.Second
)

type timedContext struct {
	context.Context
	cancel context.CancelFunc
}

type databaseTimeoutTracer struct{ timeout time.Duration }

func (t *databaseTimeoutTracer) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	return databaseTimeoutContext(ctx, t.timeout)
}

func (*databaseTimeoutTracer) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireEndData) {
	ctx.(*timedContext).cancel()
}

func (t *databaseTimeoutTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return databaseTimeoutContext(ctx, t.timeout)
}

func (*databaseTimeoutTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	ctx.(*timedContext).cancel()
}

func (t *databaseTimeoutTracer) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return databaseTimeoutContext(ctx, t.timeout)
}

func (*databaseTimeoutTracer) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

func (*databaseTimeoutTracer) TraceBatchEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchEndData) {
	ctx.(*timedContext).cancel()
}

func databaseTimeoutContext(ctx context.Context, timeout time.Duration) context.Context {
	bounded, cancel := context.WithTimeout(ctx, timeout)
	return &timedContext{Context: bounded, cancel: cancel}
}

func configureDatabaseConnection(cfg *pgx.ConnConfig) {
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = databaseConnectTimeout
	}
	cfg.RuntimeParams["plan_cache_mode"] = "force_custom_plan"
	cfg.RuntimeParams["statement_timeout"] = databaseQueryTimeout.String()
	cfg.Tracer = &databaseTimeoutTracer{timeout: databaseQueryTimeout}
}

// DatabaseConnectionConfig applies the same limits to the worker's dedicated
// advisory-lock connection as to pooled database connections.
func DatabaseConnectionConfig(databaseURL string) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	configureDatabaseConnection(cfg)
	return cfg, nil
}

// DatabasePoolConfig keeps optional filters sensitive to their actual values.
// Planning each execution avoids generic plans that scan large tables for rare
// namespace/status values. API, worker, and integration tests share this policy.
func DatabasePoolConfig(databaseURL string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	configureDatabaseConnection(cfg.ConnConfig)
	return cfg, nil
}
