package config

import (
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
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

var controllerSettings = append([]Setting{
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
	{Flag: FlagRenderTarget, Env: EnvRenderTarget, Default: "0",
		Usage: "renderer version existing networks and clusters are re-applied up to; 0 is this build's version"},
	{Flag: FlagRenderRollout, Env: EnvRenderRollout,
		Usage: "comma-separated tenant, network or cluster IDs the renderer rollout is limited to; empty for all"},
	awsSourceSetting,
}, slices.Concat(providerSettings, awsSettings)...)

// ControllerSettings returns the settings of the controller command.
func ControllerSettings() []Setting { return slices.Clone(controllerSettings) }

// Controller is the validated configuration of the controller command.
type Controller struct {
	// MetricsAddr and ProbeAddr are listen addresses; "0" is off.
	MetricsAddr, ProbeAddr string
	LeaderElect            bool
	LeaseNamespace         string
	// RenderTarget (0: this build's renderer version) and RenderRollout
	// (empty: all) drive the renderer rollout, as for the server.
	RenderTarget  int64
	RenderRollout []string
	// InstallProviders: create the Crossplane providers on startup, with
	// Provider as how the AWS providers run.
	InstallProviders bool
	Provider         ProviderRuntime
	// AWS is the management account the providers run in (required), the
	// optional role between the controller's own credentials and the
	// tenant roles, and whether keys were found in the environment.
	AWS AWS
}

// LoadController resolves and validates the controller settings from the
// flags, the environment and the config file (nil for none). All problems
// are reported together.
func LoadController(flags, env, file LookupFunc) (Controller, error) {
	v, errs := resolve(controllerSettings, flags, env, file)
	cfg := Controller{MetricsAddr: v[FlagMetricsAddr], ProbeAddr: v[FlagProbeAddr], LeaseNamespace: v[FlagLeaseNamespace]}
	cfg.AWS = AWS{ManagementAccountID: v[FlagAWSManagementAccountID], ManagementRoleARN: v[FlagAWSManagementRoleARN],
		SourceRoleARN: v[FlagAWSSourceRoleARN], StaticKeys: staticKeysFrom(env)}
	errs = append(errs, cfg.AWS.validate()...)
	provider, providerErrs := loadProviderRuntime(v)
	cfg.Provider = provider
	errs = append(errs, providerErrs...)
	var err error
	if cfg.InstallProviders, err = strconv.ParseBool(v[FlagInstallProviders]); err != nil {
		errs = append(errs, fmt.Errorf("%s must be true or false, got %q", setting(FlagInstallProviders), v[FlagInstallProviders]))
	}
	if cfg.LeaderElect, err = strconv.ParseBool(v[FlagLeaderElect]); err != nil {
		errs = append(errs, fmt.Errorf("%s must be true or false, got %q", setting(FlagLeaderElect), v[FlagLeaderElect]))
	}
	if cfg.RenderTarget, err = strconv.ParseInt(v[FlagRenderTarget], 10, 64); err != nil || cfg.RenderTarget < 0 {
		errs = append(errs, fmt.Errorf("%s must be a non-negative integer, got %q", setting(FlagRenderTarget), v[FlagRenderTarget]))
	}
	for _, id := range strings.Split(v[FlagRenderRollout], ",") {
		if id = strings.TrimSpace(id); id != "" {
			cfg.RenderRollout = append(cfg.RenderRollout, id)
		}
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
