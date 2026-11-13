package config_test

import (
	"strings"
	"testing"
	"time"

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

// One API call is bounded by --request-timeout: two minutes unless set.
func TestLoadClientRequestTimeout(t *testing.T) {
	cfg, err := config.LoadClient(nil, lookup(nil))
	if err != nil || cfg.RequestTimeout != config.DefaultRequestTimeout {
		t.Fatalf("default = %s, %v", cfg.RequestTimeout, err)
	}
	cfg, err = config.LoadClient(lookup(map[string]string{config.FlagRequestTimeout: "5m"}),
		lookup(map[string]string{config.EnvRequestTimeout: "1s"}))
	if err != nil || cfg.RequestTimeout != 5*time.Minute {
		t.Fatalf("flag = %s, %v", cfg.RequestTimeout, err)
	}
	for _, bad := range []string{"soon", "0s", "-1m"} {
		_, err := config.LoadClient(nil, lookup(map[string]string{config.EnvRequestTimeout: bad}))
		if err == nil || !strings.Contains(err.Error(), config.EnvRequestTimeout) {
			t.Errorf("request timeout %q: %v", bad, err)
		}
	}
}
