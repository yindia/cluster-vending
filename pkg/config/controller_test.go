package config_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/config"
)

func lookupIn(values map[string]string) config.LookupFunc {
	return func(key string) (string, bool) { v, ok := values[key]; return v, ok }
}

// management are the settings the controller cannot run without.
func management(more map[string]string) map[string]string {
	out := map[string]string{
		config.EnvAWSManagementAccountID: "111122223333",
		config.EnvAWSManagementRoleARN:   "arn:aws:iam::111122223333:role/crossplane-provider-aws",
	}
	for k, v := range more {
		out[k] = v
	}
	return out
}

func TestLoadController(t *testing.T) {
	cfg, err := config.LoadController(nil, lookupIn(management(nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := config.Controller{MetricsAddr: "0", ProbeAddr: ":8081", LeaderElect: true, LeaseNamespace: config.DefaultLeaseNamespace,
		InstallProviders: true,
		Provider: config.ProviderRuntime{Poll: 10 * time.Minute, EC2Poll: 30 * time.Minute, MaxReconcileRate: 10,
			CPURequest: "100m", MemoryRequest: "512Mi", MemoryLimit: "2Gi"},
		AWS: config.AWS{ManagementAccountID: "111122223333", ManagementRoleARN: "arn:aws:iam::111122223333:role/crossplane-provider-aws"}}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("defaults = %+v, want %+v", cfg, want)
	}
	cfg, err = config.LoadController(lookupIn(map[string]string{config.FlagLeaderElect: "false", config.FlagRenderRollout: "acme, id-1"}),
		lookupIn(management(map[string]string{config.EnvMetricsAddr: "127.0.0.1:9090", config.EnvRenderTarget: "4",
			config.EnvInstallProviders: "false", config.EnvAWSSourceRoleARN: "arn:aws:iam::111122223333:role/dev",
			config.EnvAWSAccessKeyID: "AKIDEXAMPLE", config.EnvAWSSecretAccessKey: "example"})),
		lookupIn(map[string]string{config.FlagProbeAddr: "0"}))
	if err != nil || cfg.LeaderElect || cfg.MetricsAddr != "127.0.0.1:9090" || cfg.ProbeAddr != "0" || cfg.InstallProviders ||
		cfg.RenderTarget != 4 || len(cfg.RenderRollout) != 2 || cfg.AWS.StaticKeys == nil ||
		cfg.AWS.SourceRoleARN != "arn:aws:iam::111122223333:role/dev" {
		t.Errorf("flag, environment and file = %+v (%v)", cfg, err)
	}
	// The controller is what looks at AWS and installs the providers: it
	// does not start without the management account.
	if _, err := config.LoadController(nil, nil, nil); err == nil || !strings.Contains(err.Error(), "--"+config.FlagAWSManagementAccountID) {
		t.Errorf("without the management account: %v, want it required", err)
	}
	for name, flags := range map[string]map[string]string{
		"render-target": {config.FlagRenderTarget: "-1"},
		"leader-elect":  {config.FlagLeaderElect: "maybe"},
		"metrics-addr":  {config.FlagMetricsAddr: "nine thousand"},
		"probe-addr":    {config.FlagProbeAddr: "8081"},
		"source role":   {config.FlagAWSSourceRoleARN: "dev"},
		"providers":     {config.FlagInstallProviders: "maybe"},
		"provider poll": {config.FlagProviderPoll: "soon"},
	} {
		if _, err := config.LoadController(lookupIn(flags), lookupIn(management(nil)), nil); err == nil || !strings.Contains(err.Error(), "--") {
			t.Errorf("%s: %v, want an error naming the flag", name, err)
		}
	}
}
