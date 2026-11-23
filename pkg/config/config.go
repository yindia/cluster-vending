// Package config holds the settings of the cluster-vending CLI and server.
// Every setting is a command-line flag backed by a CV_* environment
// variable and, optionally, a YAML config file (--config): a flag on the
// command line wins over the variable, which wins over the file, which wins
// over the default. See LoadFile for the file format.
//
// Global settings apply to every command (which cluster to talk to, how to
// log). Serve settings apply to the API server only: its listen address
// and shutdown timeout, and the settings file (--config). Controller
// settings apply to the controller (controller.go). Client settings apply to the commands that call the API
// (bootstrap, network, cluster); their --config is the request
// input, not a settings file.
//
// Region and zone data is not configuration: it lives in the static catalog
// in pkg/constant.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"time"
)

// Flag names and environment variables of the global settings.
const (
	FlagConfig      = "config"
	EnvConfig       = "CV_CONFIG"
	FlagKubeconfig  = "kubeconfig"
	EnvKubeconfig   = "CV_KUBECONFIG"
	FlagKubeContext = "context"
	EnvKubeContext  = "CV_KUBE_CONTEXT"
	FlagLogLevel    = "log-level"
	EnvLogLevel     = "CV_LOG_LEVEL"
	FlagLogFormat   = "log-format"
	EnvLogFormat    = "CV_LOG_FORMAT"
)

// Flag names and environment variables of the serve settings.
const (
	FlagHTTPAddr        = "http-addr"
	EnvHTTPAddr         = "CV_HTTP_ADDR"
	FlagShutdownTimeout = "shutdown-timeout"
	EnvShutdownTimeout  = "CV_SHUTDOWN_TIMEOUT"
)

// Flag names and environment variables of the controller's reconciling
// settings (controller.go): leader election, the renderer rollout and how
// the Crossplane providers are installed and run.
const (
	FlagInstallProviders = "install-providers"
	EnvInstallProviders  = "CV_INSTALL_PROVIDERS"
	FlagInstallCatalog   = "install-catalog"
	EnvInstallCatalog    = "CV_INSTALL_CATALOG"

	FlagRecoverStuckReleases = "recover-stuck-releases"
	EnvRecoverStuckReleases  = "CV_RECOVER_STUCK_RELEASES"
	FlagHelmBackoffMax       = "helm-backoff-max"
	EnvHelmBackoffMax        = "CV_HELM_BACKOFF_MAX"
	FlagHelmMaxFailures      = "helm-max-failures"
	EnvHelmMaxFailures       = "CV_HELM_MAX_FAILURES"
	FlagRecommendations      = "recommendations"
	EnvRecommendations       = "CV_RECOMMENDATIONS"

	FlagLeaderElect    = "leader-elect"
	EnvLeaderElect     = "CV_LEADER_ELECT"
	FlagLeaseNamespace = "lease-namespace"
	EnvLeaseNamespace  = "CV_LEASE_NAMESPACE"
	FlagServiceAccount = "service-account"
	EnvServiceAccount  = "CV_SERVICE_ACCOUNT"
	FlagRenderTarget   = "render-target"
	EnvRenderTarget    = "CV_RENDER_TARGET"
	FlagRenderRollout  = "render-rollout"
	EnvRenderRollout   = "CV_RENDER_ROLLOUT"

	FlagProviderPoll          = "provider-poll"
	EnvProviderPoll           = "CV_PROVIDER_POLL"
	FlagProviderEC2Poll       = "provider-ec2-poll"
	EnvProviderEC2Poll        = "CV_PROVIDER_EC2_POLL"
	FlagProviderReconcileRate = "provider-max-reconcile-rate"
	EnvProviderReconcileRate  = "CV_PROVIDER_MAX_RECONCILE_RATE"
	FlagProviderCPURequest    = "provider-cpu-request"
	EnvProviderCPURequest     = "CV_PROVIDER_CPU_REQUEST"
	FlagProviderMemoryRequest = "provider-memory-request"
	EnvProviderMemoryRequest  = "CV_PROVIDER_MEMORY_REQUEST"
	FlagProviderCPULimit      = "provider-cpu-limit"
	EnvProviderCPULimit       = "CV_PROVIDER_CPU_LIMIT"
	FlagProviderMemoryLimit   = "provider-memory-limit"
	EnvProviderMemoryLimit    = "CV_PROVIDER_MEMORY_LIMIT"
)

// Defaults used when neither the flag nor the variable is set.
const (
	DefaultLogLevel        = "info"
	DefaultLogFormat       = "json"
	DefaultHTTPAddr        = ":8080"
	DefaultShutdownTimeout = 30 * time.Second
	// DefaultLeaseNamespace holds the leader election Lease: Crossplane's
	// namespace exists in every management cluster.
	DefaultLeaseNamespace = "crossplane-system"
)

var (
	logLevels  = []string{"debug", "info", "warn", "error"}
	logFormats = []string{"json", "text"}
)

var globalSettings = []Setting{
	{Flag: FlagKubeconfig, Env: EnvKubeconfig,
		Usage: "kubeconfig of the Crossplane management cluster; empty uses $KUBECONFIG, ~/.kube/config, then in-cluster config"},
	{Flag: FlagKubeContext, Env: EnvKubeContext,
		Usage: "kubeconfig context to use; empty uses the current context"},
	{Flag: FlagLogLevel, Env: EnvLogLevel, Default: DefaultLogLevel,
		Usage: "log level: debug, info, warn or error"},
	{Flag: FlagLogFormat, Env: EnvLogFormat, Default: DefaultLogFormat,
		Usage: "log format: json or text"},
}

// serveSettings are the API server's. It validates requests and stores
// the desired state; it reconciles nothing, installs nothing and makes no
// AWS call: the controller does.
var serveSettings = withNodeGroupSettings([]Setting{
	{Flag: FlagConfig, Env: EnvConfig,
		Usage: "YAML file with any of these settings, keyed by flag name; flags and environment variables win over it"},
	{Flag: FlagHTTPAddr, Env: EnvHTTPAddr, Default: DefaultHTTPAddr,
		Usage: "listen address of the Connect API, host:port"},
	{Flag: FlagShutdownTimeout, Env: EnvShutdownTimeout, Default: DefaultShutdownTimeout.String(),
		Usage: "how long in-flight requests get to finish on shutdown"},
	{Flag: FlagAWSManagementRoleARN, Env: EnvAWSManagementRoleARN,
		Usage: "the platform's management role (the controller's aws-management-role-arn): the principal the account role templates trust, which account create prints as the role stack's parameter; the server makes no AWS call with it. Empty: account create says to set it and creates nothing"},
	{Flag: FlagAWSServerRoleARN, Env: EnvAWSServerRoleARN,
		Usage: "the IAM role the API server's own credentials are (its IRSA role): the principal the account role templates trust, next to the management role, to read the results buckets; account create prints it as the role stack's ServerPrincipalArn. Not needed when aws-source-role-arn is set (that role is it). Empty with no source role: the server cannot read results, and account create prints no server principal"},
	awsAccessSettings[0], awsAccessSettings[1], awsAccessSettings[2],
	liveServeSettings[0], liveServeSettings[1],
})

// GlobalSettings returns the settings every command accepts.
func GlobalSettings() []Setting { return slices.Clone(globalSettings) }

// ServeSettings returns the settings of the serve command.
func ServeSettings() []Setting { return slices.Clone(serveSettings) }

// Global is the validated configuration shared by every command.
type Global struct {
	// Kubeconfig is the management cluster's kubeconfig. Empty means the
	// standard client-go loading rules, so any cluster (kind, a dev
	// cluster, in-cluster) works without extra setup.
	Kubeconfig string
	// KubeContext selects a context in the kubeconfig. Empty means the
	// current context.
	KubeContext string
	LogLevel    string
	LogFormat   string
}

// Serve is the validated configuration of the API server.
type Serve struct {
	HTTPAddr        string
	ShutdownTimeout time.Duration
	// ManagementRoleARN is what account onboarding puts in the role's stack
	// (optional).
	ManagementRoleARN string
	// NodeGroups is the node group layout: the allowlist and defaults of
	// user node groups, and the system group.
	NodeGroups NodeGroups
	// ServerRoleARN is the role the server's own credentials are (optional;
	// see ServerPrincipalARN).
	ServerRoleARN string
	// AWSAccess is how the server reaches tenant accounts to read the
	// results buckets (pkg/cloud/aws). Its base credentials come from the
	// AWS SDK's default chain, never from a setting.
	AWSAccess AWSAccess
	// LiveURL is the live reader's URL; empty: no live reads (PROP-02 5.5).
	LiveURL string
	// LiveAllowWithoutAuth lets live reads run with auth.mode none (7.5).
	LiveAllowWithoutAuth bool
}

// ServerPrincipalARN is the principal a tenant role trusts for the server:
// the source role when one is set (it is what assumes the tenant role),
// else the server's own role (aws-server-role-arn); empty for none.
func (s Serve) ServerPrincipalARN() string {
	if s.AWSAccess.SourceRoleARN != "" {
		return s.AWSAccess.SourceRoleARN
	}
	return s.ServerRoleARN
}

// LoadGlobal resolves and validates the global settings from the flags, the
// environment and the config file (nil for none; see FileFrom). All
// problems are reported together.
func LoadGlobal(flags, env, file LookupFunc) (Global, error) {
	v, errs := resolve(globalSettings, flags, env, file)
	cfg := Global{
		Kubeconfig:  v[FlagKubeconfig],
		KubeContext: v[FlagKubeContext],
		LogLevel:    v[FlagLogLevel],
		LogFormat:   v[FlagLogFormat],
	}
	errs = append(errs, cfg.validate()...)
	if len(errs) > 0 {
		return Global{}, invalid(errs)
	}
	return cfg, nil
}

// LoadServe resolves and validates the serve settings from the flags, the
// environment and the config file (nil for none). All problems are
// reported together.
func LoadServe(flags, env, file LookupFunc) (Serve, error) {
	v, errs := resolve(serveSettings, flags, env, file)
	cfg := Serve{HTTPAddr: v[FlagHTTPAddr], ManagementRoleARN: v[FlagAWSManagementRoleARN], ServerRoleARN: v[FlagAWSServerRoleARN]}
	access, accessErrs := awsAccessFrom(v)
	cfg.AWSAccess = access
	errs = append(errs, accessErrs...)
	errs = append(errs, validateRoleARN(FlagAWSServerRoleARN, cfg.ServerRoleARN)...)
	timeout, err := parseTimeout(v[FlagShutdownTimeout])
	if err != nil {
		errs = append(errs, err)
	}
	cfg.ShutdownTimeout = timeout
	var liveErrs []error
	cfg.LiveURL, cfg.LiveAllowWithoutAuth, liveErrs = liveServeFrom(v)
	errs = append(errs, liveErrs...)
	allow, allowErrs := nodeGroupsFrom(v)
	cfg.NodeGroups = allow
	errs = append(errs, allowErrs...)
	errs = append(errs, cfg.validateOnboarding()...)
	if cfg.HTTPAddr != "" {
		if _, _, err := net.SplitHostPort(cfg.HTTPAddr); err != nil {
			errs = append(errs, fmt.Errorf("%s must be host:port: %w", setting(FlagHTTPAddr), err))
		}
	}
	if len(errs) > 0 {
		return Serve{}, invalid(errs)
	}
	return cfg, nil
}

// validateOnboarding checks the optional onboarding setting: a role ARN.
func (s Serve) validateOnboarding() []error {
	var errs []error
	if s.ManagementRoleARN != "" && !roleARNPattern.MatchString(s.ManagementRoleARN) {
		errs = append(errs, fmt.Errorf("%s must be an IAM role ARN (arn:aws:iam::<account>:role/<name>), got %q",
			setting(FlagAWSManagementRoleARN), s.ManagementRoleARN))
	}
	return errs
}

func (g Global) validate() []error {
	var errs []error
	if g.LogLevel != "" && !slices.Contains(logLevels, g.LogLevel) {
		errs = append(errs, fmt.Errorf("%s must be one of %v, got %q", setting(FlagLogLevel), logLevels, g.LogLevel))
	}
	if g.LogFormat != "" && !slices.Contains(logFormats, g.LogFormat) {
		errs = append(errs, fmt.Errorf("%s must be one of %v, got %q", setting(FlagLogFormat), logFormats, g.LogFormat))
	}
	if g.Kubeconfig != "" {
		if err := regularFile(g.Kubeconfig); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", setting(FlagKubeconfig), err))
		}
	}
	return errs
}

func parseTimeout(v string) (time.Duration, error) {
	if v == "" {
		return 0, nil // already reported as set but empty
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", setting(FlagShutdownTimeout), err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %s", setting(FlagShutdownTimeout), v)
	}
	return d, nil
}

func regularFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	return nil
}

// setting returns the setting with the given flag name, for error messages.
func setting(flag string) Setting {
	for _, s := range slices.Concat(globalSettings, serveSettings, clientSettings, controllerSettings) {
		if s.Flag == flag {
			return s
		}
	}
	return Setting{Flag: flag}
}

func invalid(errs []error) error {
	return fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
}
