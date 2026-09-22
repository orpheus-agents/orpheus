package config

import "testing"

func TestDatabasePoolConfig(t *testing.T) {
	for _, mode := range []string{"auto", "force_generic_plan", "force_custom_plan"} {
		cfg, err := DatabasePoolConfig("postgres://user@localhost/orpheus?plan_cache_mode=" + mode + "&application_name=orpheus-test&pool_max_conns=3")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ConnConfig.RuntimeParams["plan_cache_mode"] != "force_custom_plan" {
			t.Fatal("pool must use custom plans")
		}
		if cfg.ConnConfig.RuntimeParams["application_name"] != "orpheus-test" || cfg.MaxConns != 3 {
			t.Fatal("other pool settings lost")
		}
	}
	if _, err := DatabasePoolConfig("://invalid"); err == nil {
		t.Fatal("invalid URL accepted")
	}
}
