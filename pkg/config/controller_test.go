package config_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/config"
)

func lookupIn(values map[string]string) config.LookupFunc {
	return func(key string) (string, bool) { v, ok := values[key]; return v, ok }
}

func TestLoadController(t *testing.T) {
	cfg, err := config.LoadController(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := config.Controller{MetricsAddr: "0", ProbeAddr: ":8081", LeaderElect: true, LeaseNamespace: config.DefaultLeaseNamespace}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("defaults = %+v, want %+v", cfg, want)
	}
	cfg, err = config.LoadController(lookupIn(map[string]string{config.FlagLeaderElect: "false", config.FlagRenderRollout: "acme, id-1"}),
		lookupIn(map[string]string{config.EnvMetricsAddr: "127.0.0.1:9090", config.EnvRenderTarget: "4",
			config.EnvAWSAccessKeyID: "AKIDEXAMPLE", config.EnvAWSSecretAccessKey: "example"}),
		lookupIn(map[string]string{config.FlagProbeAddr: "0"}))
	if err != nil || cfg.LeaderElect || cfg.MetricsAddr != "127.0.0.1:9090" || cfg.ProbeAddr != "0" ||
		cfg.RenderTarget != 4 || len(cfg.RenderRollout) != 2 || cfg.AWS.StaticKeys == nil {
		t.Errorf("flag, environment and file = %+v (%v)", cfg, err)
	}
	for name, flags := range map[string]map[string]string{
		"render-target": {config.FlagRenderTarget: "-1"},
		"leader-elect":  {config.FlagLeaderElect: "maybe"},
		"metrics-addr":  {config.FlagMetricsAddr: "nine thousand"},
		"probe-addr":    {config.FlagProbeAddr: "8081"},
	} {
		if _, err := config.LoadController(lookupIn(flags), nil, nil); err == nil || !strings.Contains(err.Error(), "--") {
			t.Errorf("%s: %v, want an error naming the flag", name, err)
		}
	}
}
