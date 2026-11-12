package management_test

import (
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
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

// The Helm Releases of a cluster's platform components are recorded too.
func TestPlatformComponentReleasesAreCached(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}

	cached, err := kube.GVKs(scheme, management.CachedObjects())

	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cached, crossplaneaws.HelmReleaseGVK) {
		t.Fatalf("%s is not in the informer cache", crossplaneaws.HelmReleaseGVK)
	}
}

// The platform source reads the catalog, what a channel offers and a
// release; what does not exist is empty, not an error.
func TestPlatformSource(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&cloudv1alpha1.ComponentDefinition{ObjectMeta: metav1.ObjectMeta{Name: "karpenter"}},
		&cloudv1alpha1.Release{ObjectMeta: metav1.ObjectMeta{Name: "2026-11-a"}},
		&cloudv1alpha1.ReleaseChannel{ObjectMeta: metav1.ObjectMeta{Name: "stable"},
			Status: cloudv1alpha1.ReleaseChannelStatus{Release: "2026-11-a"}},
	).Build()
	src := management.PlatformSource(c)
	ctx := t.Context()

	defs, err := src.Definitions(ctx)
	if err != nil || len(defs) != 1 || defs[0].Name != "karpenter" {
		t.Errorf("Definitions = %v, %v", defs, err)
	}
	if got, err := src.Offered(ctx, "stable"); err != nil || got != "2026-11-a" {
		t.Errorf("Offered(stable) = %q, %v", got, err)
	}
	if got, err := src.Offered(ctx, "alpha"); err != nil || got != "" {
		t.Errorf("Offered(alpha) = %q, %v", got, err)
	}
	if rel, err := src.Release(ctx, "2026-11-a"); err != nil || rel == nil || rel.Name != "2026-11-a" {
		t.Errorf("Release = %v, %v", rel, err)
	}
	if rel, err := src.Release(ctx, "2026-12-a"); err != nil || rel != nil {
		t.Errorf("a release that does not exist = %v, %v", rel, err)
	}
}
