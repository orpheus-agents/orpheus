package config

import "github.com/jackc/pgx/v5/pgxpool"

// DatabasePoolConfig keeps optional filters sensitive to their actual values.
// Planning each execution avoids generic plans that scan large tables for rare
// namespace/status values. API, worker, and integration tests share this policy.
func DatabasePoolConfig(databaseURL string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.RuntimeParams["plan_cache_mode"] = "force_custom_plan"
	return cfg, nil
}
