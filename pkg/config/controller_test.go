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
		InstallProviders: true, InstallCatalog: true, RecoverStuckReleases: true, Recommendations: true,
		ReconcileWorkers: config.DefaultReconcileWorkers, ScanWorkers: config.DefaultScanWorkers,
		KubeQPS: config.DefaultKubeQPS, KubeBurst: config.DefaultKubeBurst, UpgradeTimeout: config.DefaultUpgradeTimeout,
		TeardownTimeout: config.DefaultTeardownTimeout, HelmBackoffMax: config.DefaultHelmBackoffMax, HelmMaxFailures: config.DefaultHelmMaxFailures,
		NodeGroups: config.DefaultNodeGroups(),
		AWS:        config.AWS{ManagementAccountID: "111122223333", ManagementRoleARN: managementRole}}
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
		"catalog":         {config.FlagInstallCatalog: "maybe"},
		"recover":         {config.FlagRecoverStuckReleases: "sometimes"},
		"recommendations": {config.FlagRecommendations: "maybe"},
		"credentials":     {config.FlagProviderCredentialsSecret: "aws-local"},
		"workers zero":    {config.FlagReconcileWorkers: "0"},
		"workers word":    {config.FlagReconcileWorkers: "many"},
		"upgrades":        {config.FlagAutomaticUpgrades: "soon"},
		"timeout word":    {config.FlagUpgradeTimeout: "long"},
		"timeout zero":    {config.FlagUpgradeTimeout: "0s"},
		"teardown word":   {config.FlagTeardownTimeout: "soon"},
		"backoff word":    {config.FlagHelmBackoffMax: "long"},
		"backoff small":   {config.FlagHelmBackoffMax: "30s"},
		"failures zero":   {config.FlagHelmMaxFailures: "0"},
		"failures word":   {config.FlagHelmMaxFailures: "many"},
		"teardown zero":   {config.FlagTeardownTimeout: "0s"},
		"scan workers 0":  {config.FlagScanWorkers: "0"},
		"scan workers":    {config.FlagScanWorkers: "a few"},
		"qps zero":        {config.FlagKubeQPS: "0"},
		"qps word":        {config.FlagKubeQPS: "fast"},
		"burst zero":      {config.FlagKubeBurst: "0"},
		"burst below qps": {config.FlagKubeQPS: "80", config.FlagKubeBurst: "40"},
	} {
		if _, err := config.LoadController(lookupIn(flags), lookupIn(management(nil)), nil); err == nil || !strings.Contains(err.Error(), "--") {
			t.Errorf("%s: %v, want an error naming the flag", name, err)
		}
	}
}

// --reconcile-workers is how many accounts, networks and clusters of each
// kind are reconciled at once.
func TestReconcileWorkers(t *testing.T) {
	cfg, err := config.LoadController(nil, lookupIn(management(map[string]string{config.EnvReconcileWorkers: "16"})), nil)
	if err != nil || cfg.ReconcileWorkers != 16 {
		t.Errorf("CV_RECONCILE_WORKERS=16: %d (%v), want 16", cfg.ReconcileWorkers, err)
	}
	if config.DefaultReconcileWorkers != 8 {
		t.Errorf("default = %d, want 8", config.DefaultReconcileWorkers)
	}
}

// --automatic-upgrades is off unless turned on: the rollout of a
// channel's Kubernetes minor is then planned and reported only.
func TestAutomaticUpgrades(t *testing.T) {
	cfg, err := config.LoadController(nil, lookupIn(management(nil)), nil)
	if err != nil || cfg.AutomaticUpgrades {
		t.Errorf("default: %v (%v), want off", cfg.AutomaticUpgrades, err)
	}
	cfg, err = config.LoadController(nil, lookupIn(management(map[string]string{config.EnvAutomaticUpgrades: "true"})), nil)
	if err != nil || !cfg.AutomaticUpgrades {
		t.Errorf("CV_AUTOMATIC_UPGRADES=true: %v (%v), want on", cfg.AutomaticUpgrades, err)
	}
}

// --upgrade-timeout is how long an automatic upgrade may take before the
// rollout counts it as failed.
func TestUpgradeTimeout(t *testing.T) {
	cfg, err := config.LoadController(nil, lookupIn(management(map[string]string{config.EnvUpgradeTimeout: "2h"})), nil)
	if err != nil || cfg.UpgradeTimeout != 2*time.Hour {
		t.Errorf("CV_UPGRADE_TIMEOUT=2h: %s (%v), want 2h", cfg.UpgradeTimeout, err)
	}
	if config.DefaultUpgradeTimeout != 90*time.Minute {
		t.Errorf("default = %s, want 90m", config.DefaultUpgradeTimeout)
	}
}

// --teardown-timeout bounds the first step of a cluster deletion: deleting
// what its controllers made in the cloud.
func TestTeardownTimeout(t *testing.T) {
	cfg, err := config.LoadController(nil, lookupIn(management(map[string]string{config.EnvTeardownTimeout: "30m"})), nil)
	if err != nil || cfg.TeardownTimeout != 30*time.Minute {
		t.Errorf("CV_TEARDOWN_TIMEOUT=30m: %s (%v), want 30m", cfg.TeardownTimeout, err)
	}
	if config.DefaultTeardownTimeout != 15*time.Minute {
		t.Errorf("default = %s, want 15m", config.DefaultTeardownTimeout)
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
		config.FlagLeaseNamespace, config.FlagServiceAccount, config.FlagAWSManagementRoleARN, config.FlagInstallProviders,
		config.FlagInstallCatalog, config.FlagRecoverStuckReleases, config.FlagHelmBackoffMax, config.FlagHelmMaxFailures, config.FlagRecommendations,
		config.FlagReconcileWorkers, config.FlagScanWorkers, config.FlagKubeQPS, config.FlagKubeBurst,
		config.FlagAutomaticUpgrades, config.FlagUpgradeTimeout, config.FlagTeardownTimeout,
		config.FlagProviderCredentialsSecret,
		config.FlagSkipTenantRole, config.FlagAWSRegion, config.FlagAWSSourceRoleARN, config.FlagAWSExternalID,
		config.FlagSystemInstanceTypes, config.FlagSystemNodeGroupInstanceType, config.FlagSystemNodeGroupSize, config.FlagSystemNodeGroupMinSize, config.FlagSystemNodeGroupMaxSize, config.FlagSystemNodeGroupCapacityType, config.FlagSystemNodeGroupAMIType,
		config.FlagSystemNodeGroupDiskSizeGiB, config.FlagNodeGroupInstanceFamilies, config.FlagNodeGroupInstanceTypes,
		config.FlagNodeGroupDefaultArchitectures, config.FlagNodeGroupDefaultCapacityTypes,
		config.FlagPlatformInstanceFamilies, config.FlagPlatformInstanceSizes, config.FlagPlatformArchitectures, config.FlagPlatformCapacityTypes,
		config.FlagPlatformLimitCPU, config.FlagPlatformLimitMemory, config.FlagPlatformConsolidationPolicy, config.FlagPlatformConsolidateAfter,
		config.FlagPlatformExpireAfter, config.FlagPlatformTerminationGracePeriod, config.FlagPlatformDisruptionBudget, config.FlagPlatformMaxPods}
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

// --skip-tenant-role marks the credentials Secret direct, and needs one.
func TestSkipTenantRole(t *testing.T) {
	cfg, err := config.LoadController(lookupIn(map[string]string{
		config.FlagProviderCredentialsSecret: "crossplane-system/aws-local", config.FlagSkipTenantRole: "true",
	}), lookupIn(management(nil)), nil)
	if err != nil || cfg.AWS.ProviderCredentials == nil || !cfg.AWS.ProviderCredentials.Direct {
		t.Errorf("with a Secret: %+v (%v), want it direct", cfg.AWS.ProviderCredentials, err)
	}
	_, err = config.LoadController(lookupIn(map[string]string{config.FlagSkipTenantRole: "true"}), lookupIn(management(nil)), nil)
	if err == nil || !strings.Contains(err.Error(), "--"+config.FlagSkipTenantRole) {
		t.Errorf("without a Secret: %v, want an error naming the flag", err)
	}
	cfg, err = config.LoadController(lookupIn(map[string]string{config.FlagProviderCredentialsSecret: "crossplane-system/aws-local"}),
		lookupIn(management(nil)), nil)
	if err != nil || cfg.AWS.ProviderCredentials.Direct {
		t.Errorf("default: %+v (%v), want the tenant role assumed", cfg.AWS.ProviderCredentials, err)
	}
}

// The Helm reliability settings: the design's defaults, and what a person
// may set.
func TestHelmSettings(t *testing.T) {
	cfg, err := config.LoadController(lookupIn(map[string]string{config.FlagHelmBackoffMax: "45m", config.FlagHelmMaxFailures: "8"}),
		lookupIn(management(nil)), nil)

	if err != nil || cfg.HelmBackoffMax != 45*time.Minute || cfg.HelmMaxFailures != 8 {
		t.Fatalf("%+v (%v)", cfg, err)
	}
	if config.DefaultHelmBackoffMax != 30*time.Minute || config.DefaultHelmMaxFailures != 5 {
		t.Error("the defaults are the design's: 30m and 5")
	}
}

// The scanners' workers and the management client's rate limits come from
// a flag, a variable or the platform file like every setting.
func TestLoadControllerScanWorkersAndRateLimits(t *testing.T) {
	cfg, err := config.LoadController(lookupIn(map[string]string{config.FlagScanWorkers: "2"}),
		lookupIn(management(map[string]string{config.EnvKubeQPS: "120.5"})),
		lookupIn(map[string]string{config.FlagKubeBurst: "300"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ScanWorkers != 2 || cfg.KubeQPS != 120.5 || cfg.KubeBurst != 300 {
		t.Errorf("scan workers %d, QPS %v, burst %d", cfg.ScanWorkers, cfg.KubeQPS, cfg.KubeBurst)
	}
}
