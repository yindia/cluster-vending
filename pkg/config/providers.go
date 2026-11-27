package config

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
	"k8s.io/apimachinery/pkg/api/resource"
)

// SectionProviders is the settings file's key of how the Crossplane
// providers run (their DeploymentRuntimeConfigs). Like "naas" it is no
// flag and no environment variable: LoadFile leaves it to LoadProviders,
// and only the controller reads it.
const SectionProviders = "providers"

// KnownProviders are the providers the platform installs and tunes, the
// keys of the section's overrides (crossplaneaws.RuntimeProviders; a test
// there keeps them equal).
var KnownProviders = []string{"provider-aws-ec2", "provider-aws-eks", "provider-aws-iam", "provider-aws-cloudwatchlogs",
	"provider-aws-kms", "provider-aws-sqs", "provider-aws-cloudwatchevents", "provider-aws-s3", "provider-helm"}

// Providers is the "providers" section: Defaults for every provider and
// Overrides by provider name, each field over Defaults. A zero field is
// not set: the platform's default (crossplaneaws.DefaultProviderRuntime).
// Without the section the providers run with the platform's defaults.
type Providers struct {
	Defaults  ProviderSettings
	Overrides map[string]ProviderSettings
}

// ProviderSettings is how one provider runs: Deployment replicas (leader
// election keeps one active), how often it checks each resource (poll),
// its reconciles a second at most, and its container's resources.
type ProviderSettings struct {
	Replicas                                         int32
	Poll                                             time.Duration
	MaxReconcileRate                                 int
	CPURequest, MemoryRequest, CPULimit, MemoryLimit string
}

// providerFile is a provider's entry as the file writes it.
type providerFile struct {
	Replicas         int32  `yaml:"replicas"`
	Poll             string `yaml:"poll"`
	MaxReconcileRate int    `yaml:"maxReconcileRate"`
	Resources        struct {
		Requests resourceFile `yaml:"requests"`
		Limits   resourceFile `yaml:"limits"`
	} `yaml:"resources"`
}

type resourceFile struct {
	CPU    string `yaml:"cpu"`
	Memory string `yaml:"memory"`
}

// ProvidersFrom reads the "providers" section of the file named by
// --config or CV_CONFIG; without a file, nothing configured.
func ProvidersFrom(flags, env LookupFunc) (Providers, error) {
	path, ok := get(flags, FlagConfig)
	if !ok {
		path, ok = get(env, EnvConfig)
	}
	if !ok {
		return Providers{}, nil
	}
	return LoadProviders(path)
}

// LoadProviders reads the "providers" section of a settings file. Unknown
// keys and providers, and malformed values, are errors, reported together.
func LoadProviders(path string) (Providers, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Providers{}, fmt.Errorf("config file: %w", err)
	}
	var file map[string]yaml.Node
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return Providers{}, fmt.Errorf("config file %s: invalid yaml: %w", path, err)
	}
	section, ok := file[SectionProviders]
	if !ok {
		return Providers{}, nil
	}
	var doc struct {
		Defaults  providerFile            `yaml:"defaults"`
		Overrides map[string]providerFile `yaml:"overrides"`
	}
	encoded, err := yaml.Marshal(&section)
	if err != nil {
		return Providers{}, fmt.Errorf("config file %s: %s: %w", path, SectionProviders, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(encoded))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return Providers{}, fmt.Errorf("config file %s: %s: %w", path, SectionProviders, err)
	}
	out, errs := Providers{}, []error{}
	var perr []error
	out.Defaults, perr = doc.Defaults.settings(SectionProviders + ".defaults")
	errs = append(errs, perr...)
	for _, name := range slices.Sorted(maps.Keys(doc.Overrides)) {
		entry := doc.Overrides[name]
		key := SectionProviders + ".overrides." + name
		if !slices.Contains(KnownProviders, name) {
			errs = append(errs, fmt.Errorf("%s: unknown provider %q (known: %s)", key, name, strings.Join(KnownProviders, ", ")))
			continue
		}
		settings, perr := entry.settings(key)
		errs = append(errs, perr...)
		if out.Overrides == nil {
			out.Overrides = map[string]ProviderSettings{}
		}
		out.Overrides[name] = settings
	}
	if err := errors.Join(errs...); err != nil {
		return Providers{}, fmt.Errorf("config file %s: %w", path, err)
	}
	return out, nil
}

// settings validates an entry; key names it in errors.
func (f providerFile) settings(key string) (ProviderSettings, []error) {
	var errs []error
	out := ProviderSettings{Replicas: f.Replicas, MaxReconcileRate: f.MaxReconcileRate,
		CPURequest: f.Resources.Requests.CPU, MemoryRequest: f.Resources.Requests.Memory,
		CPULimit: f.Resources.Limits.CPU, MemoryLimit: f.Resources.Limits.Memory}
	if f.Replicas < 0 {
		errs = append(errs, fmt.Errorf("%s.replicas must be at least 1 (or unset), got %d", key, f.Replicas))
	}
	if f.MaxReconcileRate < 0 {
		errs = append(errs, fmt.Errorf("%s.maxReconcileRate must be at least 1 (or unset), got %d", key, f.MaxReconcileRate))
	}
	if f.Poll != "" {
		poll, err := time.ParseDuration(f.Poll)
		if err != nil || poll <= 0 {
			errs = append(errs, fmt.Errorf("%s.poll must be a positive duration like 10m, got %q", key, f.Poll))
		}
		out.Poll = poll
	}
	errs = append(errs, checkResources(key, out)...)
	return out, errs
}

// checkResources parses every quantity set and refuses a request above
// its limit (the API server would refuse the provider's Deployment).
func checkResources(key string, s ProviderSettings) []error {
	var errs []error
	parsed := map[string]resource.Quantity{}
	for _, q := range []struct{ name, value string }{{"requests.cpu", s.CPURequest}, {"requests.memory", s.MemoryRequest},
		{"limits.cpu", s.CPULimit}, {"limits.memory", s.MemoryLimit}} {
		name, v := q.name, q.value
		if v == "" {
			continue
		}
		q, err := resource.ParseQuantity(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s.resources.%s must be a quantity like 500m or 1Gi, got %q", key, name, v))
			continue
		}
		parsed[name] = q
	}
	for _, res := range []string{"cpu", "memory"} {
		req, hasReq := parsed["requests."+res]
		limit, hasLimit := parsed["limits."+res]
		if hasReq && hasLimit && req.Cmp(limit) > 0 {
			errs = append(errs, fmt.Errorf("%s.resources: the %s request %s is above its limit %s", key, res, req.String(), limit.String()))
		}
	}
	return errs
}
