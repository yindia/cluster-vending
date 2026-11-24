package cluster

import (
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	recon "github.com/yindia/cluster-vending/pkg/reconcile"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

// A Kubernetes version change is refused while an upgrade runs or is
// halted; the same version again is still accepted.
func TestVersionChangeRefusedDuringAnUpgrade(t *testing.T) {
	for name, halted := range map[string]bool{"in progress": false, "halted": true} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t, interceptor.Funcs{}, nil, withVersions, func(d *recon.Deps) { d.Releases = releases{} })
			ctx := t.Context()
			if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
				t.Fatal(err)
			}
			row := w.cluster(t)
			spec, err := reconcilecluster.SpecOf(row)
			if err != nil {
				t.Fatal(err)
			}
			current := spec.GetControlPlane().GetKubernetesVersion()
			status := &cloudv1.ClusterStatus{Upgrade: &cloudv1.ClusterUpgrade{FromVersion: "1.34", ToVersion: "1.35", Halted: halted}}
			if row.Status, err = reconcilecluster.Encode(status); err != nil {
				t.Fatal(err)
			}
			if err := w.store.Clusters().Record(ctx, row); err != nil {
				t.Fatal(err)
			}
			version := func(v string) *cloudv1.ClusterSpec {
				return &cloudv1.ClusterSpec{ControlPlane: &cloudv1.ControlPlaneSpec{KubernetesVersion: v}}
			}

			_, err = w.svc.UpdateCluster(ctx, updateRequest(w, version("1.36"), pathKubernetesVersion))

			if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "cluster retry") {
				t.Fatalf("version change = %v, want FAILED_PRECONDITION naming cluster retry", err)
			}
			if _, err := w.svc.UpdateCluster(ctx, updateRequest(w, version(current), pathKubernetesVersion)); err != nil {
				t.Errorf("same version = %v, want accepted", err)
			}
		})
	}
}

// A halted upgrade with no failed automatic rollout (a manual version
// change) is tried again by cluster retry: the request is recorded, so the
// controller clears the halt.
func TestRetryResumesAHaltedManualUpgrade(t *testing.T) {
	for name, asked := range map[string]bool{"everything": false, "upgrade": true} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t, interceptor.Funcs{}, nil)
			ctx := t.Context()
			row := w.cluster(t)
			status := &cloudv1.ClusterStatus{Upgrade: &cloudv1.ClusterUpgrade{FromVersion: "1.34", ToVersion: "1.35", Halted: true}}
			var err error
			if row.Status, err = reconcilecluster.Encode(status); err != nil {
				t.Fatal(err)
			}
			if err := w.store.Clusters().Record(ctx, row); err != nil {
				t.Fatal(err)
			}

			res, err := w.svc.RetryCluster(ctx, &cloudv1.RetryClusterRequest{TenantId: worldTenant, ClusterId: row.ID, Upgrade: asked})

			if err != nil {
				t.Fatal(err)
			}
			if up := byName(res.GetItems())[upgradeItem]; up.GetAction() != cloudv1.RetryAction_RETRY_ACTION_RETRY {
				t.Errorf("upgrade = %v, want RETRY", up)
			}
			if got := w.cluster(t).Retry; got == nil || !got.Upgrade {
				t.Errorf("recorded retry = %+v, want the upgrade", got)
			}
		})
	}
}

// A cluster on no channel runs the release's default minor when it names
// none, and a minor the stable channel's release does not offer is refused
// at create (its components could never be installed).
func TestCreateOnNoChannelChecksTheRelease(t *testing.T) {
	w := channelWorld(t, releases{})
	ctx := t.Context()
	none := cloudv1.ReleaseChannel_RELEASE_CHANNEL_UNSPECIFIED

	res, err := w.svc.CreateCluster(ctx, onChannel(w, "no-channel-default", none, ""))
	if err != nil {
		t.Fatal(err)
	}
	row, err := w.store.Clusters().Get(ctx, worldTenant, res.GetCluster().GetId())
	if err != nil {
		t.Fatal(err)
	}
	spec, err := reconcilecluster.SpecOf(row)
	if err != nil {
		t.Fatal(err)
	}
	if got := spec.GetControlPlane().GetKubernetesVersion(); got != "1.37" {
		t.Errorf("kubernetes_version = %q, want the release's default 1.37", got)
	}

	_, err = w.svc.CreateCluster(ctx, onChannel(w, "no-channel-old", none, "1.20"))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "1.37, 1.36, 1.35") {
		t.Errorf("1.20 = %v, want FAILED_PRECONDITION naming the offered minors", err)
	}
	if _, err := w.svc.CreateCluster(ctx, onChannel(w, "no-channel-ok", none, "1.36")); err != nil {
		t.Errorf("1.36 = %v, want accepted", err)
	}
}
