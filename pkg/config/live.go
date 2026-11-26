package config

import (
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"time"
)

// The live reader's settings (PROP-02 5.5): the API server's, where it
// reaches the reader and whether live reads run without authentication, and
// the `live` command's own listen address. In the config file they are the
// "live" section:
//
//	live:
//	  url: http://cluster-vending-live:8090   # live-url
//	  allowWithoutAuth: true                  # live-allowWithoutAuth (local only)
//	  addr: :8090                             # live-addr (the live command)
//	  metricsAddr: :8091                      # live-metricsAddr (the live command)
const (
	FlagLiveURL              = "live-url"
	EnvLiveURL               = "CV_LIVE_URL"
	FlagLiveAllowWithoutAuth = "live-allowWithoutAuth"
	EnvLiveAllowWithoutAuth  = "CV_LIVE_ALLOW_WITHOUT_AUTH"
	FlagLiveAddr             = "live-addr"
	EnvLiveAddr              = "CV_LIVE_ADDR"
	// DefaultLiveAddr is the live reader's listen address.
	DefaultLiveAddr        = ":8090"
	FlagLiveMetricsAddr    = "live-metricsAddr"
	EnvLiveMetricsAddr     = "CV_LIVE_METRICS_ADDR"
	DefaultLiveMetricsAddr = "0"
)

// liveServeSettings are the API server's settings of the live reader.
var liveServeSettings = []Setting{
	{Flag: FlagLiveURL, Env: EnvLiveURL,
		Usage: "URL of the live reader (`cluster-vending live`, its internal Service): node lists and node totals are then read live from the workload clusters through it. Empty: the stored node lists, and GetNodeResources is UNIMPLEMENTED"},
	{Flag: FlagLiveAllowWithoutAuth, Env: EnvLiveAllowWithoutAuth, Default: "false",
		Usage: "let live reads run with auth.mode none, where any caller can name any tenant; for local development only (hack/config.local.yaml), never in a deployment"},
}

// liveSettings are the `live` command's.
var liveSettings = []Setting{
	{Flag: FlagConfig, Env: EnvConfig,
		Usage: "YAML file with any of these settings, keyed by flag name; flags and environment variables win over it"},
	{Flag: FlagLiveAddr, Env: EnvLiveAddr, Default: DefaultLiveAddr,
		Usage: "listen address of the live reader's internal Connect endpoint, host:port"},
	{Flag: FlagLiveMetricsAddr, Env: EnvLiveMetricsAddr, Default: DefaultLiveMetricsAddr,
		Usage: "a second listen address serving only /metrics, /healthz and /readyz, host:port, so a scraper need not reach the internal endpoint; 0: none (/metrics stays on live-addr either way)"},
	{Flag: FlagShutdownTimeout, Env: EnvShutdownTimeout, Default: DefaultShutdownTimeout.String(),
		Usage: "how long in-flight requests get to finish on shutdown"},
}

// LiveSettings returns the settings of the live command.
func LiveSettings() []Setting { return slices.Clone(liveSettings) }

// Live is the validated configuration of the live reader.
type Live struct {
	Addr string
	// MetricsAddr is the metrics-only listener; empty: none.
	MetricsAddr     string
	ShutdownTimeout time.Duration
}

// LoadLive resolves and validates the live command's settings.
func LoadLive(flags, env, file LookupFunc) (Live, error) {
	v, errs := resolve(liveSettings, flags, env, file)
	cfg := Live{Addr: v[FlagLiveAddr]}
	timeout, err := parseTimeout(v[FlagShutdownTimeout])
	if err != nil {
		errs = append(errs, err)
	}
	cfg.ShutdownTimeout = timeout
	if _, _, err := net.SplitHostPort(cfg.Addr); err != nil {
		errs = append(errs, fmt.Errorf("%s must be host:port: %w", setting(FlagLiveAddr), err))
	}
	if m := v[FlagLiveMetricsAddr]; m != "" && m != "0" {
		if _, _, err := net.SplitHostPort(m); err != nil {
			errs = append(errs, fmt.Errorf("%s must be host:port or 0: %w", setting(FlagLiveMetricsAddr), err))
		}
		cfg.MetricsAddr = m
	}
	if len(errs) > 0 {
		return Live{}, invalid(errs)
	}
	return cfg, nil
}

// liveServeFrom are the serve command's live settings from the resolved
// values.
func liveServeFrom(v map[string]string) (string, bool, []error) {
	var errs []error
	u := v[FlagLiveURL]
	if u != "" {
		parsed, err := url.Parse(u)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			errs = append(errs, fmt.Errorf("%s must be an http(s) URL, got %q", setting(FlagLiveURL), u))
		}
	}
	allow, err := strconv.ParseBool(v[FlagLiveAllowWithoutAuth])
	if err != nil {
		errs = append(errs, fmt.Errorf("%s must be true or false, got %q", setting(FlagLiveAllowWithoutAuth), v[FlagLiveAllowWithoutAuth]))
	}
	return u, allow, errs
}
