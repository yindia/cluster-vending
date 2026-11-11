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
)

// Defaults used when neither the flag nor the variable is set.
const (
	DefaultLogLevel        = "info"
	DefaultLogFormat       = "json"
	DefaultHTTPAddr        = ":8080"
	DefaultShutdownTimeout = 30 * time.Second
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

var serveSettings = append([]Setting{
	{Flag: FlagConfig, Env: EnvConfig,
		Usage: "YAML file with any of these settings, keyed by flag name (nesting allowed: database: {driver: postgres}); flags and environment variables win over it"},
	{Flag: FlagHTTPAddr, Env: EnvHTTPAddr, Default: DefaultHTTPAddr,
		Usage: "listen address of the Connect API, host:port"},
	{Flag: FlagShutdownTimeout, Env: EnvShutdownTimeout, Default: DefaultShutdownTimeout.String(),
		Usage: "how long in-flight requests get to finish on shutdown"},
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
	AWS              AWS
	Database         database.Config
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
	for _, s := range slices.Concat(globalSettings, serveSettings, clientSettings) {
		if s.Flag == flag {
			return s
		}
	}
	return Setting{Flag: flag}
}

func invalid(errs []error) error {
	return fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
}
