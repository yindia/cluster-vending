package manager

import (
	"slices"
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/config"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// The providers section of every shipped settings file documents the
// platform's defaults: loaded and applied, it changes nothing.
func TestShippedProvidersSectionsAreTheDefaults(t *testing.T) {
	want := crossplaneaws.DefaultProviderRuntime()
	want.ManagementRoleARN = "arn:aws:iam::111122223333:role/r"
	for _, path := range []string{"../../../config/node-groups/platform.yaml", "../../../config/server/auth/platform.yaml",
		"../../../hack/config.local.yaml"} {
		section, err := config.LoadProviders(path)
		if err != nil {
			t.Fatal(err)
		}
		got := providerRuntime(config.AWS{ManagementRoleARN: want.ManagementRoleARN}, section)
		for _, name := range crossplaneaws.RuntimeProviders() {
			if got.For(name) != want.For(name) {
				t.Errorf("%s: %s runs with %+v, want the defaults %+v", path, name, got.For(name), want.For(name))
			}
		}
	}
}

func TestProviderRuntimeFromTheSection(t *testing.T) {
	rt := providerRuntime(config.AWS{}, config.Providers{
		Defaults:  config.ProviderSettings{Replicas: 2},
		Overrides: map[string]config.ProviderSettings{"provider-aws-kms": {Poll: time.Hour, MemoryLimit: "1Gi"}},
	})
	kms := rt.For("provider-aws-kms")
	if kms.Replicas != 2 || kms.Poll != time.Hour || kms.MemoryLimit != "1Gi" || kms.MemoryRequest != "256Mi" {
		t.Errorf("provider-aws-kms = %+v", kms)
	}
	if rt.For("provider-aws-iam").Replicas != 2 {
		t.Error("the section's default did not reach every provider")
	}
}

// config.KnownProviders, the keys the section accepts, are the providers
// the platform runs.
func TestKnownProvidersAreTheRuntimeProviders(t *testing.T) {
	if !slices.Equal(config.KnownProviders, crossplaneaws.RuntimeProviders()) {
		t.Errorf("config.KnownProviders %v, crossplaneaws.RuntimeProviders %v", config.KnownProviders, crossplaneaws.RuntimeProviders())
	}

}
