package config

import (
	"fmt"
	"net/url"
	"slices"
)

// Flag names and environment variables of the client settings.
const (
	FlagServer = "server"
	EnvServer  = "CV_SERVER"
)

// DefaultServer is the API address of `serve` with its default settings.
const DefaultServer = "http://127.0.0.1:8080"

var clientSettings = []Setting{
	{Flag: FlagServer, Env: EnvServer, Default: DefaultServer,
		Usage: "base URL of the cluster-vending API, http(s)://host:port"},
}

// ClientSettings returns the settings of the commands that call the API.
func ClientSettings() []Setting { return slices.Clone(clientSettings) }

// Client is the validated configuration of the API client commands.
type Client struct {
	// Server is the API base URL.
	Server string
}

// LoadClient resolves and validates the client settings from the flags and
// the environment. The client has no settings file: its --config is the
// request input.
func LoadClient(flags, env LookupFunc) (Client, error) {
	v, errs := resolve(clientSettings, flags, env, nil)
	cfg := Client{Server: v[FlagServer]}
	if cfg.Server != "" {
		u, err := url.Parse(cfg.Server)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("%s must be an http(s) URL such as %s, got %q",
				setting(FlagServer), DefaultServer, cfg.Server))
		}
	}
	if len(errs) > 0 {
		return Client{}, invalid(errs)
	}
	return cfg, nil
}
