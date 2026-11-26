package cluster

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	recon "github.com/yindia/cluster-vending/pkg/reconcile"
)

// importReleases offers 1.34 alpha, 1.33 beta, 1.32 stable and the
// Terraform cluster's 1.31: on no channel (extended support) unless
// channeled, then on stable (and 1.32 on no channel).
type importReleases struct{ channeled bool }

func (importReleases) Offered(context.Context, string) (string, error) { return "import", nil }

func (r importReleases) Release(_ context.Context, name string) (*cloudv1alpha1.Release, error) {
	if name != "import" {
		return nil, nil
	}
	minors := []cloudv1alpha1.KubernetesRelease{
		{Minor: "1.34", Channel: "alpha", Default: true}, {Minor: "1.33", Channel: "beta"},
		{Minor: "1.32", Channel: "stable"}, {Minor: "1.31", UpgradeTo: "1.32"},
	}
	if r.channeled {
		minors[2].Channel, minors[3].Channel = "", "stable"
	}
	return &cloudv1alpha1.Release{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: cloudv1alpha1.ReleaseSpec{Content: cloudv1alpha1.ReleaseContent{Kubernetes: minors}}}, nil
}

func extendedImportWorld(t *testing.T, found *awscloud.Cluster, r importReleases) world {
	t.Helper()
	return newWorld(t, interceptor.Funcs{}, nil, withVersions, func(d *recon.Deps) {
		d.AWSClusterDiscovery = fakeClusterDiscoverer{cluster: found}
		d.Releases = r
	})
}

// Importing a cluster on an extended-support minor read-only is fine;
// taking it over (the platform renders it then) needs extended support:
// STANDARD is refused as on a create, with where to change it.
func TestTakeOverOnAnExtendedMinor(t *testing.T) {
	takeOver := func(t *testing.T, w world) error {
		t.Helper()
		c := importTF(t, w) // read-only: accepted whatever the minor
		_, err := w.svc.TakeOverCluster(t.Context(), &cloudv1.TakeOverClusterRequest{TenantId: worldTenant, ClusterId: c.GetId(),
			AcknowledgeNotImported: true})
		return err
	}
	t.Run("standard", func(t *testing.T) {
		err := takeOver(t, extendedImportWorld(t, terraformCluster(), importReleases{}))
		for _, want := range []string{
			"Kubernetes 1.31 is an extended-support version (the release offers it on no channel)",
			"AWS_SUPPORT_TYPE_STANDARD would have AWS upgrade the control plane by force when standard support for 1.31 ends",
			"change them in EKS",
		} {
			if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), want) {
				t.Fatalf("takeover of a STANDARD 1.31 cluster = %v, want INVALID_ARGUMENT %q", err, want)
			}
		}
	})
	t.Run("extended", func(t *testing.T) {
		found := terraformCluster()
		found.SupportType = "EXTENDED"
		if err := takeOver(t, extendedImportWorld(t, found, importReleases{})); err != nil {
			t.Fatalf("takeover of an EXTENDED 1.31 cluster = %v, want accepted", err)
		}
	})
	t.Run("channel-minor", func(t *testing.T) {
		if err := takeOver(t, extendedImportWorld(t, terraformCluster(), importReleases{channeled: true})); err != nil {
			t.Fatalf("takeover of a STANDARD cluster on stable's 1.31 = %v, want accepted", err)
		}
	})
}
