package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	recon "github.com/yindia/cluster-vending/pkg/reconcile"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

// releases offers one release on every channel: 1.37 alpha, 1.36 beta,
// 1.35 stable. err fails every read.
type releases struct{ err error }

const releaseName = "shipped-a"

func (r releases) Offered(context.Context, string) (string, error) { return releaseName, r.err }

func (r releases) Release(_ context.Context, name string) (*cloudv1alpha1.Release, error) {
	if r.err != nil {
		return nil, r.err
	}
	if name != releaseName {
		return nil, nil
	}
	return &cloudv1alpha1.Release{ObjectMeta: metav1.ObjectMeta{Name: releaseName},
		Spec: cloudv1alpha1.ReleaseSpec{Content: cloudv1alpha1.ReleaseContent{Kubernetes: []cloudv1alpha1.KubernetesRelease{
			{Minor: "1.37", Channel: "alpha", Default: true},
			{Minor: "1.36", Channel: "beta"},
			{Minor: "1.35", Channel: "stable"},
		}}}}, nil
}

func channelWorld(t *testing.T, r releases) world {
	t.Helper()
	return newWorld(t, interceptor.Funcs{}, nil, func(d *recon.Deps) { d.Releases = r })
}

func onChannel(w world, name string, channel cloudv1.ReleaseChannel, version string) *cloudv1.CreateClusterRequest {
	req := createRequest(w, name)
	req.Cluster.Spec.ReleaseChannel = channel
	req.Cluster.Spec.ControlPlane.KubernetesVersion = version
	return req
}

// A cluster created on a channel without a version runs the channel's
// minor; one asking for a newer minor than its channel's is refused, an
// older offered one is not.
func TestCreateOnAChannel(t *testing.T) {
	w := channelWorld(t, releases{})
	ctx := t.Context()

	res, err := w.svc.CreateCluster(ctx, onChannel(w, "beta-default", cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA, ""))
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
	if got := spec.GetControlPlane().GetKubernetesVersion(); got != "1.36" {
		t.Errorf("kubernetes_version = %q, want beta's 1.36", got)
	}

	_, err = w.svc.CreateCluster(ctx, onChannel(w, "stable-newer", cloudv1.ReleaseChannel_RELEASE_CHANNEL_STABLE, "1.36"))
	want := "channel stable offers Kubernetes 1.35; choose 1.35 or older, or channel beta (1.36) or alpha (1.37)"
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), want) {
		t.Errorf("stable asking for 1.36 = %v, want INVALID_ARGUMENT %q", err, want)
	}

	if _, err := w.svc.CreateCluster(ctx, onChannel(w, "alpha-older", cloudv1.ReleaseChannel_RELEASE_CHANNEL_ALPHA, "1.35")); err != nil {
		t.Errorf("alpha asking for 1.35 (older, offered) = %v, want accepted", err)
	}

	// No channel: the version stays the client's (EKS chooses when unset).
	res, err = w.svc.CreateCluster(ctx, onChannel(w, "no-channel", cloudv1.ReleaseChannel_RELEASE_CHANNEL_UNSPECIFIED, ""))
	if err != nil {
		t.Fatal(err)
	}
	row, err = w.store.Clusters().Get(ctx, worldTenant, res.GetCluster().GetId())
	if err != nil {
		t.Fatal(err)
	}
	if spec, _ := reconcilecluster.SpecOf(row); spec.GetControlPlane().GetKubernetesVersion() != "" {
		t.Errorf("no channel: kubernetes_version = %q, want unset", spec.GetControlPlane().GetKubernetesVersion())
	}
}

// When the releases cannot be read the create is refused, never let
// through unchecked.
func TestCreateOnAChannelNeedsTheRelease(t *testing.T) {
	w := channelWorld(t, releases{err: errors.New("releasechannels is forbidden")})
	_, err := w.svc.CreateCluster(t.Context(), onChannel(w, "beta", cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA, ""))
	if connect.CodeOf(err) != connect.CodeUnavailable || !strings.Contains(err.Error(), "cannot read release channel beta") {
		t.Fatalf("err = %v, want UNAVAILABLE naming the channel", err)
	}
	if strings.Contains(err.Error(), "forbidden") {
		t.Errorf("err = %v: the management cluster's error is the log's, not the caller's", err)
	}
}

// An update may not move the version past the channel's minor; moving
// to a channel whose minor is older than the cluster's is allowed: the
// cluster waits, it is never downgraded.
func TestUpdateOnAChannel(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil, withVersions, func(d *recon.Deps) { d.Releases = releases{} })
	ctx := t.Context()
	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	update := func(spec *cloudv1.ClusterSpec, paths ...string) error {
		t.Helper()
		_, err := w.svc.UpdateCluster(ctx, updateRequest(w, spec, paths...))
		return err
	}
	version := func(v string) *cloudv1.ClusterSpec {
		return &cloudv1.ClusterSpec{ControlPlane: &cloudv1.ControlPlaneSpec{KubernetesVersion: v}}
	}
	alpha := &cloudv1.ClusterSpec{ReleaseChannel: cloudv1.ReleaseChannel_RELEASE_CHANNEL_ALPHA}
	if err := update(alpha, pathReleaseChannel); err != nil {
		t.Fatalf("to alpha: %v", err)
	}
	for _, v := range []string{"1.35", "1.36"} {
		if err := update(version(v), pathKubernetesVersion); err != nil {
			t.Fatalf("alpha, %s: %v", v, err)
		}
	}

	stable := &cloudv1.ClusterSpec{ReleaseChannel: cloudv1.ReleaseChannel_RELEASE_CHANNEL_STABLE}
	if err := update(stable, pathReleaseChannel); err != nil {
		t.Fatalf("moving a 1.36 cluster to stable (1.35) = %v, want accepted: it waits", err)
	}

	err := update(version("1.37"), pathKubernetesVersion)
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "channel stable offers Kubernetes 1.35") {
		t.Fatalf("stable, 1.37 = %v, want INVALID_ARGUMENT", err)
	}
}
