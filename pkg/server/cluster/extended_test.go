package cluster

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	recon "github.com/yindia/cluster-vending/pkg/reconcile"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

// extendedReleases offers 1.37 alpha, 1.36 beta, 1.35 stable and 1.34 on
// no channel (extended support). moved, once set, is the release after
// the channels moved on: 1.38 alpha, 1.37 beta, 1.36 stable, 1.35 and 1.34
// on no channel.
type extendedReleases struct{ moved *atomic.Bool }

func (r extendedReleases) Offered(context.Context, string) (string, error) {
	if r.moved != nil && r.moved.Load() {
		return "moved", nil
	}
	return "extended", nil
}

func (r extendedReleases) Release(_ context.Context, name string) (*cloudv1alpha1.Release, error) {
	minors := map[string][]cloudv1alpha1.KubernetesRelease{
		"extended": {
			{Minor: "1.37", Channel: "alpha", Default: true},
			{Minor: "1.36", Channel: "beta", UpgradeTo: "1.37"},
			{Minor: "1.35", Channel: "stable", UpgradeTo: "1.36"},
			{Minor: "1.34", UpgradeTo: "1.35"},
		},
		"moved": {
			{Minor: "1.38", Channel: "alpha", Default: true},
			{Minor: "1.37", Channel: "beta", UpgradeTo: "1.38"},
			{Minor: "1.36", Channel: "stable", UpgradeTo: "1.37"},
			{Minor: "1.35", UpgradeTo: "1.36"},
			{Minor: "1.34", UpgradeTo: "1.35"},
		},
	}[name]
	if minors == nil {
		return nil, nil
	}
	return &cloudv1alpha1.Release{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: cloudv1alpha1.ReleaseSpec{Content: cloudv1alpha1.ReleaseContent{Kubernetes: minors}}}, nil
}

func extendedWorld(t *testing.T, moved *atomic.Bool) world {
	t.Helper()
	return newWorld(t, interceptor.Funcs{}, nil, withVersions, func(d *recon.Deps) { d.Releases = extendedReleases{moved: moved} })
}

type extendedCreate struct {
	mode    cloudv1.UpgradeMode
	channel cloudv1.ReleaseChannel
	version string
	support cloudv1.AwsSupportType
}

func (c extendedCreate) request(w world, name string) *cloudv1.CreateClusterRequest {
	req := onChannel(w, name, c.channel, c.version)
	req.Cluster.Spec.UpgradeMode = c.mode
	if c.support != cloudv1.AwsSupportType_AWS_SUPPORT_TYPE_UNSPECIFIED {
		req.Cluster.Spec.GetAws().UpgradePolicy = &cloudv1.AwsUpgradePolicy{SupportType: c.support}
	}
	return req
}

const (
	manual    = cloudv1.UpgradeMode_UPGRADE_MODE_MANUAL
	automatic = cloudv1.UpgradeMode_UPGRADE_MODE_AUTOMATIC
	standard  = cloudv1.AwsSupportType_AWS_SUPPORT_TYPE_STANDARD
	extended  = cloudv1.AwsSupportType_AWS_SUPPORT_TYPE_EXTENDED
)

// A minor on no channel takes Manual clusters only, on extended support;
// the channels' minors are as before.
func TestCreateOnAnExtendedMinor(t *testing.T) {
	w := extendedWorld(t, nil)
	stable := cloudv1.ReleaseChannel_RELEASE_CHANNEL_STABLE
	cases := []struct {
		name string
		in   extendedCreate
		code connect.Code // 0: accepted
		want string
	}{
		{"manual", extendedCreate{mode: manual, version: "1.34"}, 0, ""},
		{"unset-mode", extendedCreate{version: "1.34"}, 0, ""},
		{"manual-extended", extendedCreate{mode: manual, version: "1.34", support: extended}, 0, ""},
		{"automatic", extendedCreate{mode: automatic, version: "1.34"}, connect.CodeInvalidArgument,
			"Kubernetes 1.34 is an extended-support version (the release offers it on no channel): it runs with upgradeMode Manual only; " +
				"set upgradeMode UPGRADE_MODE_MANUAL, or choose 1.35 or later"},
		{"automatic-on-stable", extendedCreate{mode: automatic, channel: stable, version: "1.34"}, connect.CodeInvalidArgument,
			"it runs with upgradeMode Manual only"},
		{"standard", extendedCreate{mode: manual, version: "1.34", support: standard}, connect.CodeInvalidArgument,
			"AWS_SUPPORT_TYPE_STANDARD would have AWS upgrade the control plane by force when standard support for 1.34 ends"},
		{"channel-automatic", extendedCreate{mode: automatic, channel: stable, version: "1.35"}, 0, ""},
		{"channel-standard", extendedCreate{mode: manual, version: "1.36", support: standard}, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := w.svc.CreateCluster(t.Context(), c.in.request(w, c.name))
			if c.code == 0 {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if connect.CodeOf(err) != c.code || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %v %q", err, c.code, c.want)
			}
		})
	}
}

// extendedUpdate is an update of the world's cluster.
func extendedUpdate(t *testing.T, w world, spec *cloudv1.ClusterSpec, paths ...string) error {
	t.Helper()
	_, err := w.svc.UpdateCluster(t.Context(), updateRequest(w, spec, paths...))
	return err
}

func onVersion(v string) *cloudv1.ClusterSpec {
	return &cloudv1.ClusterSpec{ControlPlane: &cloudv1.ControlPlaneSpec{KubernetesVersion: v}}
}

// The world's cluster, Manual on 1.34, reconciled.
func manualOnExtended(t *testing.T, moved *atomic.Bool) world {
	t.Helper()
	w := extendedWorld(t, moved)
	if err := extendedUpdate(t, w, onVersion("1.34"), pathKubernetesVersion); err != nil {
		t.Fatalf("manual, to 1.34: %v", err)
	}
	if err := w.svc.Reconcile(t.Context(), worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	return w
}

// A cluster on an extended minor stays Manual (it is told to upgrade
// first) and on extended support.
func TestUpdateOnAnExtendedMinor(t *testing.T) {
	w := manualOnExtended(t, nil)

	err := extendedUpdate(t, w, &cloudv1.ClusterSpec{UpgradeMode: automatic}, pathUpgradeMode)
	want := "Kubernetes 1.34 is an extended-support version (the release offers it on no channel): it runs with upgradeMode Manual only; " +
		"upgrade to 1.35 first (or keep Manual)"
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), want) {
		t.Fatalf("to automatic on 1.34 = %v, want FAILED_PRECONDITION %q", err, want)
	}
	// Planned (validate_only), too.
	plan := planRequest(w, &cloudv1.ClusterSpec{UpgradeMode: automatic}, pathUpgradeMode)
	if _, err := w.svc.UpdateCluster(t.Context(), plan); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("plan to automatic on 1.34 = %v, want FAILED_PRECONDITION", err)
	}

	standardSpec := &cloudv1.ClusterSpec{Provider: &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{
		UpgradePolicy: &cloudv1.AwsUpgradePolicy{SupportType: standard}}}}
	err = extendedUpdate(t, w, standardSpec, "spec.aws.upgrade_policy.support_type")
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "AWS_SUPPORT_TYPE_STANDARD") {
		t.Fatalf("STANDARD on 1.34 = %v, want INVALID_ARGUMENT", err)
	}

	// Moving to the next minor and automatic at once: the version is the
	// channel's then (left out of the update), so the cluster stays on 1.34
	// and is refused.
	both := &cloudv1.ClusterSpec{UpgradeMode: automatic, ControlPlane: &cloudv1.ControlPlaneSpec{KubernetesVersion: "1.35"}}
	if err := extendedUpdate(t, w, both, pathUpgradeMode, pathKubernetesVersion); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("automatic with 1.35 from 1.34 = %v, want FAILED_PRECONDITION (upgrade first)", err)
	}

	// Upgraded to a channel minor, it may be automatic.
	if err := extendedUpdate(t, w, onVersion("1.35"), pathKubernetesVersion); err != nil {
		t.Fatalf("manual 1.34 → 1.35: %v", err)
	}
	if err := extendedUpdate(t, w, &cloudv1.ClusterSpec{UpgradeMode: automatic}, pathUpgradeMode); err != nil {
		t.Errorf("to automatic on 1.35 = %v, want accepted", err)
	}
}

// supportType unset renders EXTENDED on an extended minor only; the
// stored spec is not rewritten.
func TestExtendedMinorRendersExtendedSupport(t *testing.T) {
	ctx := t.Context()
	w := manualOnExtended(t, nil)
	rendered := func() string {
		t.Helper()
		spec, err := reconcilecluster.SpecOf(w.cluster(t))
		if err != nil {
			t.Fatal(err)
		}
		if st := spec.GetAws().GetUpgradePolicy().GetSupportType(); st != cloudv1.AwsSupportType_AWS_SUPPORT_TYPE_UNSPECIFIED {
			t.Errorf("stored supportType = %v, want unset (a render default)", st)
		}
		target, err := w.svc.Target(ctx, worldTenant, spec)
		if err != nil {
			t.Fatal(err)
		}
		return w.svc.RenderPlan(spec, w.cluster(t), target).SupportType
	}
	if got := rendered(); got != "EXTENDED" {
		t.Errorf("1.34: rendered supportType %q, want EXTENDED", got)
	}
	if err := extendedUpdate(t, w, onVersion("1.35"), pathKubernetesVersion); err != nil {
		t.Fatal(err)
	}
	if got := rendered(); got != "" {
		t.Errorf("1.35 (stable): rendered supportType %q, want unset as before", got)
	}
}

// When the channels move past a minor, the clusters on it are not
// broken: an automatic one reconciles and takes unrelated updates (its
// rollout upgrades it), a manual one keeps running with the warning.
func TestChannelsMovingPastAMinor(t *testing.T) {
	ctx := t.Context()
	moved := &atomic.Bool{}
	w := extendedWorld(t, moved)
	if err := extendedUpdate(t, w, onVersion("1.35"), pathKubernetesVersion); err != nil {
		t.Fatal(err)
	}
	if err := extendedUpdate(t, w, &cloudv1.ClusterSpec{UpgradeMode: automatic}, pathUpgradeMode); err != nil {
		t.Fatalf("automatic on stable's 1.35: %v", err)
	}
	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	manualReq := extendedCreate{mode: manual, version: "1.35"}.request(w, "manual")
	created, err := w.svc.CreateCluster(ctx, manualReq)
	if err != nil {
		t.Fatal(err)
	}

	moved.Store(true) // 1.35 is on no channel now

	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Errorf("automatic cluster left on 1.35: reconcile = %v, want it to go on", err)
	}
	cidrs := &cloudv1.ClusterSpec{ControlPlane: &cloudv1.ControlPlaneSpec{
		EndpointAccess: &cloudv1.EndpointAccess{PublicAccessCidrs: []string{"198.51.100.0/24"}}}}
	if err := extendedUpdate(t, w, cidrs, "spec.control_plane.endpoint_access.public_access_cidrs"); err != nil {
		t.Errorf("automatic cluster left on 1.35, unrelated update = %v, want accepted", err)
	}
	row, err := w.store.Clusters().Get(ctx, worldTenant, created.GetCluster().GetId())
	if err != nil {
		t.Fatal(err)
	}
	got, err := w.svc.View(ctx, row)
	if err != nil {
		t.Fatal(err)
	}
	if k := got.GetStatus().GetKubernetes(); !k.GetEnding() || !strings.Contains(k.GetMessage(), "move to 1.36") {
		t.Errorf("manual cluster left on 1.35: status.kubernetes = %v, want ending, move to 1.36", k)
	}
}

// The rule on its own: a write moving an automatic spec onto an extended
// minor is refused (the API leaves an automatic cluster's version out of
// an update, so only an edit of the object or the rule's own callers get
// here); one that leaves a breach as it was passes.
func TestCheckExtendedSupportOfAMove(t *testing.T) {
	w := extendedWorld(t, nil)
	spec := func(mode cloudv1.UpgradeMode, version string) *cloudv1.ClusterSpec {
		return &cloudv1.ClusterSpec{UpgradeMode: mode, ControlPlane: &cloudv1.ControlPlaneSpec{KubernetesVersion: version}}
	}
	err := w.svc.CheckExtendedSupport(t.Context(), spec(automatic, "1.34"), spec(automatic, "1.35"))
	if connect.CodeOf(rpcError(err)) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "or choose 1.35 or later") {
		t.Errorf("automatic 1.35 → 1.34 = %v, want INVALID_ARGUMENT", err)
	}
	if err := w.svc.CheckExtendedSupport(t.Context(), spec(automatic, "1.34"), spec(automatic, "1.34")); err != nil {
		t.Errorf("automatic, left on 1.34, unrelated write = %v, want accepted", err)
	}
	if err := w.svc.CheckExtendedSupport(t.Context(), spec(manual, "1.34"), spec(manual, "1.35")); err != nil {
		t.Errorf("manual 1.35 → 1.34 = %v, want accepted by this rule", err)
	}
}
