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

const managementRole = "arn:aws:iam::111122223333:role/crossplane-provider-aws"

// management are the settings the controller cannot run without.
func management(more map[string]string) map[string]string {
	out := map[string]string{config.EnvAWSManagementRoleARN: managementRole}
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
	// The management account is the role's.
	want := config.Controller{MetricsAddr: "0", ProbeAddr: ":8081", LeaderElect: true, LeaseNamespace: config.DefaultLeaseNamespace,
		InstallProviders: true, AWS: config.AWS{ManagementAccountID: "111122223333", ManagementRoleARN: managementRole}}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("defaults = %+v, want %+v", cfg, want)
	}
	cfg, err = config.LoadController(lookupIn(map[string]string{config.FlagLeaderElect: "false"}),
		lookupIn(management(map[string]string{config.EnvMetricsAddr: "127.0.0.1:9090", config.EnvInstallProviders: "false",
			config.EnvAWSAccessKeyID: "AKIDEXAMPLE", config.EnvAWSSecretAccessKey: "example"})),
		lookupIn(map[string]string{config.FlagProbeAddr: "0"}))
	if err != nil || cfg.LeaderElect || cfg.MetricsAddr != "127.0.0.1:9090" || cfg.ProbeAddr != "0" || cfg.InstallProviders ||
		cfg.AWS.StaticKeys == nil {
		t.Errorf("flag, environment and file = %+v (%v)", cfg, err)
	}
	// The controller is what looks at AWS and installs the providers: it
	// does not start without the management role.
	if _, err := config.LoadController(nil, nil, nil); err == nil || !strings.Contains(err.Error(), "--"+config.FlagAWSManagementRoleARN) {
		t.Errorf("without the management role: %v, want it required", err)
	}
	for name, flags := range map[string]map[string]string{
		"leader-elect":    {config.FlagLeaderElect: "maybe"},
		"metrics-addr":    {config.FlagMetricsAddr: "nine thousand"},
		"probe-addr":      {config.FlagProbeAddr: "8081"},
		"management role": {config.FlagAWSManagementRoleARN: "crossplane-provider-aws"},
		"partition":       {config.FlagAWSManagementRoleARN: "arn:aws-cn:iam::111122223333:role/x"},
		"providers":       {config.FlagInstallProviders: "maybe"},
		"credentials":     {config.FlagProviderCredentialsSecret: "aws-local"},
	} {
		if _, err := config.LoadController(lookupIn(flags), lookupIn(management(nil)), nil); err == nil || !strings.Contains(err.Error(), "--") {
			t.Errorf("%s: %v, want an error naming the flag", name, err)
		}
	}
}

// The controller takes exactly these settings; everything else about how
// the providers run is the platform's (or managed outside it).
func TestControllerSettingsAreFew(t *testing.T) {
	var got []string
	for _, s := range config.ControllerSettings() {
		got = append(got, s.Flag)
	}
	want := []string{config.FlagConfig, config.FlagMetricsAddr, config.FlagProbeAddr, config.FlagLeaderElect,
		config.FlagLeaseNamespace, config.FlagAWSManagementRoleARN, config.FlagInstallProviders,
		config.FlagProviderCredentialsSecret}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("controller settings = %v, want %v", got, want)
	}
}

// The providers' credentials Secret: <namespace>/<name>[:<key>], the key
// "credentials" by default.
func TestProviderCredentialsSecret(t *testing.T) {
	for raw, want := range map[string]*config.SecretKeyRef{
		"crossplane-system/aws-local":        {Namespace: "crossplane-system", Name: "aws-local", Key: "credentials"},
		"crossplane-system/aws-local:keys":   {Namespace: "crossplane-system", Name: "aws-local", Key: "keys"},
		"tenant-acme/aws-e919d8e2.creds:aws": {Namespace: "tenant-acme", Name: "aws-e919d8e2.creds", Key: "aws"},
	} {
		cfg, err := config.LoadController(lookupIn(map[string]string{config.FlagProviderCredentialsSecret: raw}),
			lookupIn(management(nil)), nil)
		if err != nil || !reflect.DeepEqual(cfg.AWS.ProviderCredentials, want) {
			t.Errorf("%s: %+v (%v), want %+v", raw, cfg.AWS.ProviderCredentials, err, want)
		}
	}
	for _, raw := range []string{"aws-local", "/aws-local", "crossplane-system/", "Crossplane/aws", "ns/name:", "ns/name:a b", "a/b/c"} {
		_, err := config.LoadController(lookupIn(map[string]string{config.FlagProviderCredentialsSecret: raw}),
			lookupIn(management(nil)), nil)
		if err == nil || !strings.Contains(err.Error(), "--"+config.FlagProviderCredentialsSecret) {
			t.Errorf("%q: %v, want an error naming the flag", raw, err)
		}
	}
	cfg, err := config.LoadController(nil, lookupIn(management(nil)), nil)
	if err != nil || cfg.AWS.ProviderCredentials != nil {
		t.Errorf("unset: %+v (%v), want none (IRSA)", cfg.AWS.ProviderCredentials, err)
	}
}
