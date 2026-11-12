// Package config holds the settings of the cluster-vending CLI and server.
// Every setting is a command-line flag backed by a CV_* environment
// variable and, optionally, a YAML config file (--config): a flag on the
// command line wins over the variable, which wins over the file, which wins
// over the default. See LoadFile for the file format.
//
// Global settings apply to every command (which cluster to talk to, how to
// log). Serve settings apply to the API server only, including the settings
// file (--config). Client settings apply to the commands that call the API
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
	"strconv"
	"strings"
	"time"

	"github.com/yindia/cluster-vending/pkg/database"
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
	FlagHTTPAddr         = "http-addr"
	EnvHTTPAddr          = "CV_HTTP_ADDR"
	FlagShutdownTimeout  = "shutdown-timeout"
	EnvShutdownTimeout   = "CV_SHUTDOWN_TIMEOUT"
	FlagInstallProviders = "install-providers"
	EnvInstallProviders  = "CV_INSTALL_PROVIDERS"
	FlagLoopInterval     = "loop-interval"
	EnvLoopInterval      = "CV_LOOP_INTERVAL"
	FlagLeaderElect      = "leader-elect"
	EnvLeaderElect       = "CV_LEADER_ELECT"
	FlagLeaseNamespace   = "lease-namespace"
	EnvLeaseNamespace    = "CV_LEASE_NAMESPACE"
	FlagStore            = "store"
	EnvStore             = "CV_STORE"
	FlagRenderTarget     = "render-target"
	EnvRenderTarget      = "CV_RENDER_TARGET"
	FlagRenderRollout    = "render-rollout"
	EnvRenderRollout     = "CV_RENDER_ROLLOUT"

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

// Where the server keeps its state (FlagStore).
const (
	// StoreDatabase: the database of the database-* settings.
	StoreDatabase = "database"
	// StoreKubernetes: the cloud.cluster-vending.io kinds in the tenants'
	// namespaces of the management cluster (pkg/database/cloudstore); no
	// database is opened.
	StoreKubernetes = "kubernetes"
)

var (
	logLevels  = []string{"debug", "info", "warn", "error"}
	logFormats = []string{"json", "text"}
	stores     = []string{StoreDatabase, StoreKubernetes}
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

var serveSettings = append([]Setting{
	{Flag: FlagConfig, Env: EnvConfig,
		Usage: "YAML file with any of these settings, keyed by flag name (nesting allowed: database: {driver: postgres}); flags and environment variables win over it"},
	{Flag: FlagHTTPAddr, Env: EnvHTTPAddr, Default: DefaultHTTPAddr,
		Usage: "listen address of the Connect API, host:port"},
	{Flag: FlagShutdownTimeout, Env: EnvShutdownTimeout, Default: DefaultShutdownTimeout.String(),
		Usage: "how long in-flight requests get to finish on shutdown"},
	{Flag: FlagLoopInterval, Env: EnvLoopInterval, Default: "30s",
		Usage: "how often the background loop moves unsettled networks and clusters on"},
	{Flag: FlagLeaderElect, Env: EnvLeaderElect, Default: "true",
		Usage: "run the background loop behind leader election (a Lease), so one server replica runs it"},
	{Flag: FlagLeaseNamespace, Env: EnvLeaseNamespace, Default: DefaultLeaseNamespace,
		Usage: "namespace of the leader election Lease in the management cluster: the server's own namespace"},
	{Flag: FlagStore, Env: EnvStore, Default: StoreDatabase,
		Usage: "where networks, clusters and tenants are stored: database (the database-* settings) or kubernetes (CloudAccount, Network and Cluster objects in the tenants' namespaces of the management cluster; no database is opened)"},
	{Flag: FlagRenderTarget, Env: EnvRenderTarget, Default: "0",
		Usage: "renderer version existing networks and clusters are re-applied up to by the background loop; 0 is this build's version"},
	{Flag: FlagRenderRollout, Env: EnvRenderRollout,
		Usage: "comma-separated tenant, network or cluster IDs the renderer rollout is limited to; empty rolls out to all"},
	{Flag: FlagProviderPoll, Env: EnvProviderPoll, Default: "10m",
		Usage: "how often the AWS providers check each resource for drift (applies when the server creates their runtime config)"},
	{Flag: FlagProviderEC2Poll, Env: EnvProviderEC2Poll, Default: "30m",
		Usage: "the same for provider-aws-ec2 (networks: many resources that change rarely)"},
	{Flag: FlagProviderReconcileRate, Env: EnvProviderReconcileRate, Default: "10",
		Usage: "the AWS providers' maximum reconciles per second"},
	{Flag: FlagProviderCPURequest, Env: EnvProviderCPURequest, Default: "100m", Usage: "CPU request of an AWS provider"},
	{Flag: FlagProviderMemoryRequest, Env: EnvProviderMemoryRequest, Default: "512Mi", Usage: "memory request of an AWS provider"},
	{Flag: FlagProviderCPULimit, Env: EnvProviderCPULimit, Usage: "CPU limit of an AWS provider; empty for none"},
	{Flag: FlagProviderMemoryLimit, Env: EnvProviderMemoryLimit, Default: "2Gi", Usage: "memory limit of an AWS provider; empty for none"},
	{Flag: FlagInstallProviders, Env: EnvInstallProviders, Default: "true",
		Usage: "on startup, create the Crossplane AWS providers, provider-helm and the AWS runtime config when missing (existing ones are left as they are); false when they are managed elsewhere"},
}, slices.Concat(awsSettings, databaseSettings)...)

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
	// InstallProviders: create the Crossplane providers on startup.
	InstallProviders bool
	// LoopInterval is the background loop's period; LeaderElect and
	// LeaseNamespace its leader election.
	LoopInterval   time.Duration
	LeaderElect    bool
	LeaseNamespace string
	// Store is where the state is kept (StoreDatabase or StoreKubernetes).
	Store string
	// RenderTarget (0: this build's renderer version) and RenderRollout
	// (empty: all) drive the renderer rollout.
	RenderTarget  int64
	RenderRollout []string
	// Provider is how the AWS providers run (their runtime config).
	Provider ProviderRuntime
	AWS      AWS
	Database database.Config
}

// ProviderRuntime is how the Crossplane AWS providers run: drift check
// intervals, reconcile rate and container resources.
type ProviderRuntime struct {
	Poll, EC2Poll                                    time.Duration
	MaxReconcileRate                                 int
	CPURequest, MemoryRequest, CPULimit, MemoryLimit string
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
	cfg := Serve{
		HTTPAddr: v[FlagHTTPAddr],
		AWS: AWS{
			ManagementAccountID: v[FlagAWSManagementAccountID],
			ManagementRoleARN:   v[FlagAWSManagementRoleARN],
		},
	}
	cfg.AWS.StaticKeys = staticKeysFrom(env)
	timeout, err := parseTimeout(v[FlagShutdownTimeout])
	if err != nil {
		errs = append(errs, err)
	}
	cfg.ShutdownTimeout = timeout
	install, err := strconv.ParseBool(v[FlagInstallProviders])
	if err != nil {
		errs = append(errs, fmt.Errorf("%s must be true or false, got %q", setting(FlagInstallProviders), v[FlagInstallProviders]))
	}
	cfg.InstallProviders = install
	if cfg.LoopInterval, err = time.ParseDuration(v[FlagLoopInterval]); err != nil || cfg.LoopInterval <= 0 {
		errs = append(errs, fmt.Errorf("%s must be a positive duration, got %q", setting(FlagLoopInterval), v[FlagLoopInterval]))
	}
	if cfg.LeaderElect, err = strconv.ParseBool(v[FlagLeaderElect]); err != nil {
		errs = append(errs, fmt.Errorf("%s must be true or false, got %q", setting(FlagLeaderElect), v[FlagLeaderElect]))
	}
	cfg.LeaseNamespace = v[FlagLeaseNamespace]
	cfg.Store = v[FlagStore]
	if cfg.Store != "" && !slices.Contains(stores, cfg.Store) {
		errs = append(errs, fmt.Errorf("%s must be one of %v, got %q", setting(FlagStore), stores, cfg.Store))
	}
	if cfg.RenderTarget, err = strconv.ParseInt(v[FlagRenderTarget], 10, 64); err != nil || cfg.RenderTarget < 0 {
		errs = append(errs, fmt.Errorf("%s must be a non-negative integer, got %q", setting(FlagRenderTarget), v[FlagRenderTarget]))
	}
	cfg.Provider = ProviderRuntime{CPURequest: v[FlagProviderCPURequest], MemoryRequest: v[FlagProviderMemoryRequest],
		CPULimit: v[FlagProviderCPULimit], MemoryLimit: v[FlagProviderMemoryLimit]}
	if cfg.Provider.Poll, err = time.ParseDuration(v[FlagProviderPoll]); err != nil || cfg.Provider.Poll <= 0 {
		errs = append(errs, fmt.Errorf("%s must be a positive duration, got %q", setting(FlagProviderPoll), v[FlagProviderPoll]))
	}
	if cfg.Provider.EC2Poll, err = time.ParseDuration(v[FlagProviderEC2Poll]); err != nil || cfg.Provider.EC2Poll <= 0 {
		errs = append(errs, fmt.Errorf("%s must be a positive duration, got %q", setting(FlagProviderEC2Poll), v[FlagProviderEC2Poll]))
	}
	if cfg.Provider.MaxReconcileRate, err = strconv.Atoi(v[FlagProviderReconcileRate]); err != nil || cfg.Provider.MaxReconcileRate <= 0 {
		errs = append(errs, fmt.Errorf("%s must be a positive integer, got %q", setting(FlagProviderReconcileRate), v[FlagProviderReconcileRate]))
	}
	for _, id := range strings.Split(v[FlagRenderRollout], ",") {
		if id = strings.TrimSpace(id); id != "" {
			cfg.RenderRollout = append(cfg.RenderRollout, id)
		}
	}

	if cfg.HTTPAddr != "" {
		if _, _, err := net.SplitHostPort(cfg.HTTPAddr); err != nil {
			errs = append(errs, fmt.Errorf("%s must be host:port: %w", setting(FlagHTTPAddr), err))
		}
	}
	errs = append(errs, cfg.AWS.validate()...)
	db, dbErrs := loadDatabase(v)
	cfg.Database = db
	errs = append(errs, dbErrs...)
	if len(errs) > 0 {
		return Serve{}, invalid(errs)
	}
	return cfg, nil
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
