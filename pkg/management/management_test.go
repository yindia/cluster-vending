package management_test

import (
	"slices"
	"testing"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/management"
)

// The cache refuses reads of kinds it has no informer for, so every kind a
// cluster records must be cached: an unread object blocks apply and delete.
func TestClusterHelmProviderConfigIsCached(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}

	cached, err := kube.GVKs(scheme, management.CachedObjects())

	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cached, crossplaneaws.HelmProviderConfigGVK) {
		t.Fatalf("%s is not in the informer cache", crossplaneaws.HelmProviderConfigGVK)
	}
}
