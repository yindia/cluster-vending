package config_test

import (
	"testing"

	"github.com/yindia/cluster-vending/pkg/config"
)

func TestLoadClient(t *testing.T) {
	cfg, err := config.LoadClient(nil, lookup(nil))
	if err != nil || cfg.Server != config.DefaultServer {
		t.Fatalf("default = %+v, %v", cfg, err)
	}
	cfg, err = config.LoadClient(lookup(map[string]string{config.FlagServer: "https://vending.example.com"}),
		lookup(map[string]string{config.EnvServer: "http://ignored:1"}))
	if err != nil || cfg.Server != "https://vending.example.com" {
		t.Fatalf("flag = %+v, %v", cfg, err)
	}
	for _, bad := range []string{"localhost:8080", "ftp://x", "http://", ""} {
		if _, err := config.LoadClient(nil, lookup(map[string]string{config.EnvServer: bad})); err == nil {
			t.Errorf("server %q accepted", bad)
		}
	}
}
