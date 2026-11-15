package config

import (
	"fmt"
	"net"
	"slices"
	"strconv"
	"time"
)

// Flags and environment variables of the controller command.
const (
	FlagMetricsAddr = "metrics-addr"
	EnvMetricsAddr  = "CV_METRICS_ADDR"
	FlagProbeAddr   = "health-probe-addr"
	EnvProbeAddr    = "CV_HEALTH_PROBE_ADDR"
	// FlagReconcileWorkers is how many objects of each kind (accounts,
	// networks, clusters) are reconciled at once.
	FlagReconcileWorkers = "reconcile-workers"
	EnvReconcileWorkers  = "CV_RECONCILE_WORKERS"
	// FlagAutomaticUpgrades: the controller upgrades automatic clusters to
	// their channel's Kubernetes minor. Off, the rollout is planned and
	// reported on each ReleaseChannel only.
	FlagAutomaticUpgrades = "automatic-upgrades"
	EnvAutomaticUpgrades  = "CV_AUTOMATIC_UPGRADES"
	// FlagUpgradeTimeout is how long an automatic upgrade may take before
	// the rollout counts it as failed (TimedOut).
	FlagUpgradeTimeout = "upgrade-timeout"
	EnvUpgradeTimeout  = "CV_UPGRADE_TIMEOUT"
	// FlagTeardownTimeout bounds the first step of a cluster's deletion:
	// deleting the load balancers and Karpenter nodes its controllers made.
	FlagTeardownTimeout = "teardown-timeout"
	EnvTeardownTimeout  = "CV_TEARDOWN_TIMEOUT"
)

// Defaults of the controller command.
const (
	// DefaultMetricsAddr turns the controller's metrics endpoint off.
	DefaultMetricsAddr = "0"
	DefaultProbeAddr   = ":8081"
	// DefaultReconcileWorkers: one slow pass (a cluster waiting on its
	// network, a Helm install) no longer holds up every other object of
	// its kind. An object is never reconciled by two workers at once.
	DefaultReconcileWorkers = 8
	// DefaultUpgradeTimeout covers an EKS control plane upgrade (about 10
	// minutes), a node group's rolling replacement and the add-ons.
	DefaultUpgradeTimeout = 90 * time.Minute
	// DefaultTeardownTimeout: deleting a load balancer takes a minute or
	// two, draining and terminating Karpenter's nodes a few minutes more.
	DefaultTeardownTimeout = 15 * time.Minute
)

// controllerSettings are all the controller takes. How the providers run
// (poll intervals, reconcile rate, resources) is fixed in the code that
// renders their runtime config (crossplaneaws.DefaultProviderRuntime);
// an operator who needs other values sets --install-providers=false and
// manages the provider objects. The controller always brings resources to
// this build's renderer.
var controllerSettings = []Setting{
	{Flag: FlagConfig, Env: EnvConfig,
		Usage: "YAML file with any of these settings, keyed by flag name; flags and environment variables win over it"},
	{Flag: FlagMetricsAddr, Env: EnvMetricsAddr, Default: DefaultMetricsAddr,
		Usage: "listen address of the controller's Prometheus metrics, host:port; 0 turns them off"},
	{Flag: FlagProbeAddr, Env: EnvProbeAddr, Default: DefaultProbeAddr,
		Usage: "listen address of the controller's /healthz and /readyz probes, host:port; 0 turns them off"},
	{Flag: FlagLeaderElect, Env: EnvLeaderElect, Default: "true",
		Usage: "run the controllers behind leader election (a Lease), so one replica reconciles"},
	{Flag: FlagLeaseNamespace, Env: EnvLeaseNamespace, Default: DefaultLeaseNamespace,
		Usage: "namespace of the leader election Lease in the management cluster: the controller's own namespace"},
	{Flag: FlagServiceAccount, Env: EnvServiceAccount,
		Usage: "name of the controller's own service account, in its own namespace (--lease-namespace): each tenant namespace binds it to the role that gets the kubeconfig Secrets of that tenant's workload clusters (the scan for deprecated API versions); empty outside the cluster, where no binding is made"},
	{Flag: FlagAWSManagementRoleARN, Env: EnvAWSManagementRoleARN,
		Usage: "IAM role ARN the Crossplane AWS providers run as (IRSA), in the management account (the account is taken from the ARN); tenant roles trust it; required"},
	{Flag: FlagInstallProviders, Env: EnvInstallProviders, Default: "true",
		Usage: "on startup, create the Crossplane AWS providers, provider-helm and the AWS runtime config when missing (existing ones are left as they are); false when they are managed elsewhere, also to run the providers with other settings than the platform's"},
	{Flag: FlagInstallCatalog, Env: EnvInstallCatalog, Default: "true",
		Usage: "on startup, create the component catalog this build ships (pkg/catalog/definitions) where a ComponentDefinition of that name is missing (existing ones are left as they are); false when the platform team manages the catalog itself"},
	{Flag: FlagRecoverStuckReleases, Env: EnvRecoverStuckReleases, Default: "true",
		Usage: "recover a platform component whose Helm release is stuck (uninstalling or pending for 10 minutes): remove the Helm record of the stuck revision in the workload cluster so the chart is installed again, at most 3 times per component version; false leaves such a component failed for a person to look at"},
	{Flag: FlagRecommendations, Env: EnvRecommendations, Default: "true",
		Usage: "every five minutes, read each running cluster's workloads, the usage its metrics-server reports and its own Vertical Pod Autoscalers (read-only, nothing is installed there) and record a week of usage, for resource recommendations (ListClusterRecommendations); false records nothing"},
	{Flag: FlagReconcileWorkers, Env: EnvReconcileWorkers, Default: strconv.Itoa(DefaultReconcileWorkers),
		Usage: "how many cloud accounts, networks and clusters of each kind are reconciled at once (at least 1); one object is never reconciled twice at the same time"},
	{Flag: FlagAutomaticUpgrades, Env: EnvAutomaticUpgrades, Default: "false",
		Usage: "upgrade clusters with upgradeMode Automatic to their release channel's Kubernetes minor, one minor at a time, tier by tier (irreversible: EKS cannot downgrade); false plans and reports the rollout on each ReleaseChannel and cluster (status.rollout) and upgrades nothing"},
	{Flag: FlagUpgradeTimeout, Env: EnvUpgradeTimeout, Default: DefaultUpgradeTimeout.String(),
		Usage: "how long an automatic upgrade may take before the rollout counts it as failed (TimedOut, against the tier's maxFailed), e.g. 2h"},
	{Flag: FlagTeardownTimeout, Env: EnvTeardownTimeout, Default: DefaultTeardownTimeout.String(),
		Usage: "how long a cluster's deletion waits for the load balancers and Karpenter nodes its controllers made to be deleted, before it goes on (what is left is reported), e.g. 30m"},
	providerCredentialsSetting,
	{Flag: FlagSkipTenantRole, Env: EnvSkipTenantRole, Default: "false",
		Usage: "local testing only, with --provider-credentials-secret: the controller and the providers act with those keys directly, without assuming each account's tenant role (for an account whose policies forbid assuming roles); every account is then reached as that one identity"},
}

// ControllerSettings returns the settings of the controller command.
func ControllerSettings() []Setting { return slices.Clone(controllerSettings) }

// Controller is the validated configuration of the controller command.
type Controller struct {
	// MetricsAddr and ProbeAddr are listen addresses; "0" is off.
	MetricsAddr, ProbeAddr string
	LeaderElect            bool
	LeaseNamespace         string
	// ServiceAccount is the controller's own service account; nil when it
	// runs outside the cluster.
	ServiceAccount *ServiceAccountRef
	// InstallProviders: create the Crossplane providers on startup.
	InstallProviders bool
	// InstallCatalog: create the shipped component definitions that are
	// missing on startup.
	InstallCatalog bool
	// RecoverStuckReleases: remove the Helm record of a platform
	// component's stuck revision in the workload cluster.
	RecoverStuckReleases bool
	// Recommendations: sample running clusters for resource
	// recommendations.
	Recommendations bool
	// ReconcileWorkers is how many objects of each kind are reconciled at
	// once (at least 1).
	ReconcileWorkers int
	// AutomaticUpgrades: upgrade automatic clusters to their channel's
	// minor; off, the rollout is reported only.
	AutomaticUpgrades bool
	// UpgradeTimeout is how long an automatic upgrade may take.
	UpgradeTimeout time.Duration
	// TeardownTimeout bounds a cluster deletion's teardown.
	TeardownTimeout time.Duration
	// AWS is the management role the providers run as (its account
	// derived from it), the credentials Secret of the providers when set,
	// and whether keys were found in the environment.
	AWS AWS
}

// LoadController resolves and validates the controller settings from the
// flags, the environment and the config file (nil for none). All problems
// are reported together.
func LoadController(flags, env, file LookupFunc) (Controller, error) {
	v, errs := resolve(controllerSettings, flags, env, file)
	cfg := Controller{MetricsAddr: v[FlagMetricsAddr], ProbeAddr: v[FlagProbeAddr], LeaseNamespace: v[FlagLeaseNamespace]}
	aws, awsErrs := managementFromRole(v[FlagAWSManagementRoleARN])
	errs = append(errs, awsErrs...)
	cfg.AWS = aws
	cfg.AWS.StaticKeys = staticKeysFrom(env)
	account, err := serviceAccountRef(v[FlagLeaseNamespace], v[FlagServiceAccount])
	if err != nil {
		errs = append(errs, err)
	}
	cfg.ServiceAccount = account
	creds, err := parseSecretKeyRef(v[FlagProviderCredentialsSecret])
	if err != nil {
		errs = append(errs, err)
	}
	cfg.AWS.ProviderCredentials = creds
	skip, err := strconv.ParseBool(v[FlagSkipTenantRole])
	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("%s must be true or false, got %q", setting(FlagSkipTenantRole), v[FlagSkipTenantRole]))
	case skip && creds == nil:
		errs = append(errs, fmt.Errorf("%s needs %s: without a credentials Secret there are no keys to act with directly",
			setting(FlagSkipTenantRole), setting(FlagProviderCredentialsSecret)))
	case skip:
		creds.Direct = true
	}
	if cfg.InstallProviders, err = strconv.ParseBool(v[FlagInstallProviders]); err != nil {
		errs = append(errs, fmt.Errorf("%s must be true or false, got %q", setting(FlagInstallProviders), v[FlagInstallProviders]))
	}
	if cfg.InstallCatalog, err = strconv.ParseBool(v[FlagInstallCatalog]); err != nil {
		errs = append(errs, fmt.Errorf("%s must be true or false, got %q", setting(FlagInstallCatalog), v[FlagInstallCatalog]))
	}
	if cfg.RecoverStuckReleases, err = strconv.ParseBool(v[FlagRecoverStuckReleases]); err != nil {
		errs = append(errs, fmt.Errorf("%s must be true or false, got %q", setting(FlagRecoverStuckReleases), v[FlagRecoverStuckReleases]))
	}
	if cfg.Recommendations, err = strconv.ParseBool(v[FlagRecommendations]); err != nil {
		errs = append(errs, fmt.Errorf("%s must be true or false, got %q", setting(FlagRecommendations), v[FlagRecommendations]))
	}
	if cfg.ReconcileWorkers, err = strconv.Atoi(v[FlagReconcileWorkers]); err != nil || cfg.ReconcileWorkers < 1 {
		errs = append(errs, fmt.Errorf("%s must be a whole number of at least 1, got %q", setting(FlagReconcileWorkers), v[FlagReconcileWorkers]))
	}
	if cfg.AutomaticUpgrades, err = strconv.ParseBool(v[FlagAutomaticUpgrades]); err != nil {
		errs = append(errs, fmt.Errorf("%s must be true or false, got %q", setting(FlagAutomaticUpgrades), v[FlagAutomaticUpgrades]))
	}
	if cfg.UpgradeTimeout, err = time.ParseDuration(v[FlagUpgradeTimeout]); err != nil || cfg.UpgradeTimeout <= 0 {
		errs = append(errs, fmt.Errorf("%s must be a positive duration like 90m or 2h, got %q", setting(FlagUpgradeTimeout), v[FlagUpgradeTimeout]))
	}
	if cfg.TeardownTimeout, err = time.ParseDuration(v[FlagTeardownTimeout]); err != nil || cfg.TeardownTimeout <= 0 {
		errs = append(errs, fmt.Errorf("%s must be a positive duration like 15m, got %q", setting(FlagTeardownTimeout), v[FlagTeardownTimeout]))
	}
	if cfg.LeaderElect, err = strconv.ParseBool(v[FlagLeaderElect]); err != nil {
		errs = append(errs, fmt.Errorf("%s must be true or false, got %q", setting(FlagLeaderElect), v[FlagLeaderElect]))
	}
	for _, flag := range []string{FlagMetricsAddr, FlagProbeAddr} {
		addr := v[flag]
		if addr == "" || addr == "0" {
			continue
		}
		if _, _, err := net.SplitHostPort(addr); err != nil {
			errs = append(errs, fmt.Errorf("%s must be host:port or 0: %w", setting(flag), err))
		}
	}
	if len(errs) > 0 {
		return Controller{}, invalid(errs)
	}
	return cfg, nil
}

// ServiceAccountRef names a service account.
type ServiceAccountRef struct{ Namespace, Name string }

// serviceAccountRef is the service account name in the controller's own
// namespace; nil for no name.
func serviceAccountRef(namespace, name string) (*ServiceAccountRef, error) {
	if name == "" {
		return nil, nil
	}
	if !dnsSubdomainPattern.MatchString(name) {
		return nil, fmt.Errorf("%s must be a service account name, got %q", setting(FlagServiceAccount), name)
	}
	if !dnsLabelPattern.MatchString(namespace) {
		return nil, fmt.Errorf("%s must be the controller's namespace for %s, got %q",
			setting(FlagLeaseNamespace), setting(FlagServiceAccount), namespace)
	}
	return &ServiceAccountRef{Namespace: namespace, Name: name}, nil
}

// managementFromRole is the management account of the role the providers
// run as: the account is the ARN's. The role is required and validated as
// the server's two settings are (AWS.validate).
func managementFromRole(roleARN string) (AWS, []error) {
	if roleARN == "" {
		return AWS{}, []error{fmt.Errorf("%s is required", setting(FlagAWSManagementRoleARN))}
	}
	m := roleARNPattern.FindStringSubmatch(roleARN)
	if m == nil {
		return AWS{}, []error{fmt.Errorf("%s must be an IAM role ARN (arn:aws:iam::<account>:role/<name>), got %q",
			setting(FlagAWSManagementRoleARN), roleARN)}
	}
	a := AWS{ManagementAccountID: m[2], ManagementRoleARN: roleARN}
	return a, a.validate()
}
