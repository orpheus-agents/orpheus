package config

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDatabasePoolConfig(t *testing.T) {
	for _, mode := range []string{"auto", "force_generic_plan", "force_custom_plan"} {
		cfg, err := DatabasePoolConfig("postgres://user@localhost/orpheus?plan_cache_mode=" + mode + "&application_name=orpheus-test&pool_max_conns=3")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ConnConfig.RuntimeParams["plan_cache_mode"] != "force_custom_plan" {
			t.Fatal("pool must use custom plans")
		}
		if cfg.ConnConfig.RuntimeParams["statement_timeout"] != "30s" || cfg.ConnConfig.ConnectTimeout != databaseConnectTimeout {
			t.Fatal("pool database limits missing")
		}
		if _, ok := cfg.ConnConfig.Tracer.(*databaseTimeoutTracer); !ok {
			t.Fatal("pool query and acquire limits missing")
		}
		if cfg.ConnConfig.RuntimeParams["application_name"] != "orpheus-test" || cfg.MaxConns != 3 {
			t.Fatal("other pool settings lost")
		}
	}
	if _, err := DatabasePoolConfig("://invalid"); err == nil {
		t.Fatal("invalid URL accepted")
	}
}

func TestDatabaseConnectionConfig(t *testing.T) {
	cfg, err := DatabaseConnectionConfig("postgres://user@localhost/orpheus?connect_timeout=7&application_name=owner")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConnectTimeout != 7*time.Second || cfg.RuntimeParams["statement_timeout"] != "30s" || cfg.RuntimeParams["application_name"] != "owner" {
		t.Fatal("owner connection limits or caller settings missing")
	}
	if _, ok := cfg.Tracer.(*databaseTimeoutTracer); !ok {
		t.Fatal("owner query limits missing")
	}
	if _, err := DatabaseConnectionConfig("://invalid"); err == nil {
		t.Fatal("invalid URL accepted")
	}
}

func TestDatabaseTimeoutTracer(t *testing.T) {
	tracer := &databaseTimeoutTracer{timeout: databaseQueryTimeout}
	tests := []struct {
		name  string
		start func(context.Context) context.Context
		end   func(context.Context)
	}{
		{"acquire", func(ctx context.Context) context.Context {
			return tracer.TraceAcquireStart(ctx, nil, pgxpool.TraceAcquireStartData{})
		}, func(ctx context.Context) { tracer.TraceAcquireEnd(ctx, nil, pgxpool.TraceAcquireEndData{}) }},
		{"query", func(ctx context.Context) context.Context {
			return tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{})
		}, func(ctx context.Context) { tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{}) }},
		{"batch", func(ctx context.Context) context.Context {
			return tracer.TraceBatchStart(ctx, nil, pgx.TraceBatchStartData{})
		}, func(ctx context.Context) { tracer.TraceBatchEnd(ctx, nil, pgx.TraceBatchEndData{}) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := tt.start(t.Context())
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > databaseQueryTimeout {
				t.Fatal("database call has no finite deadline")
			}
			tt.end(ctx)
			if ctx.Err() != context.Canceled {
				t.Fatal("database call retained its timer")
			}
		})
	}
}
