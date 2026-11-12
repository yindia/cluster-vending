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
}

// ControllerSettings returns the settings of the controller command.
func ControllerSettings() []Setting { return slices.Clone(controllerSettings) }

// Controller is the validated configuration of the controller command.
type Controller struct {
	// MetricsAddr and ProbeAddr are listen addresses; "0" is off.
	MetricsAddr, ProbeAddr string
	LeaderElect            bool
	LeaseNamespace         string
}

// LoadController resolves and validates the controller settings from the
// flags, the environment and the config file (nil for none). All problems
// are reported together.
func LoadController(flags, env, file LookupFunc) (Controller, error) {
	v, errs := resolve(controllerSettings, flags, env, file)
	cfg := Controller{MetricsAddr: v[FlagMetricsAddr], ProbeAddr: v[FlagProbeAddr], LeaseNamespace: v[FlagLeaseNamespace]}
	var err error
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
