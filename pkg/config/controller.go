package config

import (
	"fmt"
	"net"
	"slices"
	"strconv"
)

// Flags and environment variables of the controller command.
const (
	FlagMetricsAddr = "metrics-addr"
	EnvMetricsAddr  = "CV_METRICS_ADDR"
	FlagProbeAddr   = "health-probe-addr"
	EnvProbeAddr    = "CV_HEALTH_PROBE_ADDR"
)

// Defaults of the controller command.
const (
	// DefaultMetricsAddr turns the controller's metrics endpoint off.
	DefaultMetricsAddr = "0"
	DefaultProbeAddr   = ":8081"
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
	{Flag: FlagAWSManagementRoleARN, Env: EnvAWSManagementRoleARN,
		Usage: "IAM role ARN the Crossplane AWS providers run as (IRSA), in the management account (the account is taken from the ARN); tenant roles trust it; required"},
	{Flag: FlagInstallProviders, Env: EnvInstallProviders, Default: "true",
		Usage: "on startup, create the Crossplane AWS providers, provider-helm and the AWS runtime config when missing (existing ones are left as they are); false when they are managed elsewhere, also to run the providers with other settings than the platform's"},
	providerCredentialsSetting,
}

// ControllerSettings returns the settings of the controller command.
func ControllerSettings() []Setting { return slices.Clone(controllerSettings) }

// Controller is the validated configuration of the controller command.
type Controller struct {
	// MetricsAddr and ProbeAddr are listen addresses; "0" is off.
	MetricsAddr, ProbeAddr string
	LeaderElect            bool
	LeaseNamespace         string
	// InstallProviders: create the Crossplane providers on startup.
	InstallProviders bool
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
	creds, err := parseSecretKeyRef(v[FlagProviderCredentialsSecret])
	if err != nil {
		errs = append(errs, err)
	}
	cfg.AWS.ProviderCredentials = creds
	if cfg.InstallProviders, err = strconv.ParseBool(v[FlagInstallProviders]); err != nil {
		errs = append(errs, fmt.Errorf("%s must be true or false, got %q", setting(FlagInstallProviders), v[FlagInstallProviders]))
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
