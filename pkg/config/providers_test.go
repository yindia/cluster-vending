package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/config"
)

func writeSettings(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "platform.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadProviders(t *testing.T) {
	path := writeSettings(t, `
leader-elect: false
providers:
  defaults:
    replicas: 2
    poll: 10m
    maxReconcileRate: 10
    resources:
      requests: {cpu: 200m, memory: 512Mi}
      limits: {cpu: "1", memory: 1Gi}
  overrides:
    provider-aws-ec2:
      poll: 30m
      resources:
        requests: {cpu: 500m}
`)
	got, err := config.LoadProviders(path)
	if err != nil {
		t.Fatal(err)
	}
	want := config.Providers{
		Defaults: config.ProviderSettings{Replicas: 2, Poll: 10 * time.Minute, MaxReconcileRate: 10,
			CPURequest: "200m", MemoryRequest: "512Mi", CPULimit: "1", MemoryLimit: "1Gi"},
		Overrides: map[string]config.ProviderSettings{"provider-aws-ec2": {Poll: 30 * time.Minute, CPURequest: "500m"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
	// The other commands' settings loader skips the section.
	if _, err := config.LoadFile(path); err != nil {
		t.Errorf("LoadFile: %v", err)
	}
}

func TestLoadProvidersWithoutTheSection(t *testing.T) {
	got, err := config.LoadProviders(writeSettings(t, "leader-elect: false\n"))
	if err != nil || !reflect.DeepEqual(got, config.Providers{}) {
		t.Errorf("got %+v, %v; want nothing configured", got, err)
	}
	got, err = config.ProvidersFrom(nil, nil)
	if err != nil || !reflect.DeepEqual(got, config.Providers{}) {
		t.Errorf("without a file: %+v, %v", got, err)
	}
}

func TestLoadProvidersRefusesBadValues(t *testing.T) {
	for name, tc := range map[string]struct{ section, want string }{
		"an unknown key":      {"defaults: {colour: blue}", "colour"},
		"a bad poll":          {"defaults: {poll: often}", "providers.defaults.poll"},
		"a zero poll":         {"defaults: {poll: 0s}", "providers.defaults.poll"},
		"negative replicas":   {"defaults: {replicas: -1}", "providers.defaults.replicas"},
		"a negative rate":     {"defaults: {maxReconcileRate: -5}", "providers.defaults.maxReconcileRate"},
		"a bad quantity":      {"overrides: {provider-aws-s3: {resources: {limits: {memory: lots}}}}", "providers.overrides.provider-aws-s3.resources.limits.memory"},
		"an unknown provider": {"overrides: {provider-aws-glacier: {replicas: 1}}", "provider-aws-glacier"},
		"request over limit":  {"defaults: {resources: {requests: {cpu: \"2\"}, limits: {cpu: \"1\"}}}", "providers.defaults.resources"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := config.LoadProviders(writeSettings(t, "providers:\n  "+tc.section+"\n"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %v, want one naming %s", err, tc.want)
			}
		})
	}
}
