package config

import (
	"bytes"
	"fmt"
	"os"

	"go.yaml.in/yaml/v3"

	"github.com/yindia/cluster-vending/pkg/auth"
)

// SectionAuth is the settings file's key of the API server's
// authentication (auth.Config). It is the one section that is not a flat
// list of settings: it holds lists, so it has no flags and no environment
// variables, and LoadFile leaves it to LoadAuth. It holds no secret.
const SectionAuth = "auth"

// AuthFrom reads the "auth" section of the file named by --config or
// CV_CONFIG. Without a file, or without the section, authentication is
// off (auth.ModeNone).
func AuthFrom(flags, env LookupFunc) (auth.Config, error) {
	path, ok := get(flags, FlagConfig)
	if !ok {
		path, ok = get(env, EnvConfig)
	}
	if !ok {
		return auth.Config{}.Resolved()
	}
	return LoadAuth(path)
}

// LoadAuth reads the "auth" section of a settings file, with its defaults
// and presets applied (auth.Config.Resolved). Unknown keys in the section
// are errors, and so is every problem of the configuration, reported
// together.
func LoadAuth(path string) (auth.Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return auth.Config{}, fmt.Errorf("config file: %w", err)
	}
	var file map[string]yaml.Node
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return auth.Config{}, fmt.Errorf("config file %s: invalid yaml: %w", path, err)
	}
	var cfg auth.Config
	if section, ok := file[SectionAuth]; ok {
		// Through the encoder: only a Decoder can refuse unknown keys.
		encoded, err := yaml.Marshal(&section)
		if err != nil {
			return auth.Config{}, fmt.Errorf("config file %s: %s: %w", path, SectionAuth, err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(encoded))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			return auth.Config{}, fmt.Errorf("config file %s: %s: %w", path, SectionAuth, err)
		}
	}
	resolved, err := cfg.Resolved()
	if err != nil {
		return auth.Config{}, fmt.Errorf("config file %s: invalid configuration: %w", path, err)
	}
	return resolved, nil
}
