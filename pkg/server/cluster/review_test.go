package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	awscwl "github.com/upbound/provider-aws/v2/apis/namespaced/cloudwatchlogs/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// An update that names an optional setting in its mask but leaves the
// message holding it out resets the setting; it must not crash.
func TestUpdateWithAMaskedFieldLeftOut(t *testing.T) {
	w := appliedWorld(t)
	for _, path := range []string{"spec.aws.irsa", "spec.control_plane.endpoint_access.endpoint_private_access",
		"spec.control_plane.endpoint_access.endpoint_public_access"} {
		for name, spec := range map[string]*cloudv1.ClusterSpec{"no spec": nil, "empty spec": {}} {
			if _, err := w.svc.UpdateCluster(t.Context(), updateRequest(w, spec, path)); err != nil {
				t.Errorf("%s with %s: %v", path, name, err)
			}
		}
	}
}

// The loop does not move a create on to its next stage while the call
// that stored it may still be applying or rolling back.
func TestStageWaitsForTheCreateToFinish(t *testing.T) {
	reconcilecluster.ResumeAfter, reconcilecluster.StageAfter = 0, time.Hour
	t.Cleanup(func() { reconcilecluster.ResumeAfter, reconcilecluster.StageAfter = 2*time.Minute, 15*time.Second })
	w := newWorld(t, interceptor.Funcs{}, nil)
	ctx := t.Context()
	prefix := crossplaneaws.ClusterPrefix(worldTenant, w.row.ID, w.row.Name)
	newCluster(t, w, func(s *cloudv1.ClusterSpec) {
		s.GetAws().ControlPlaneLogGroup = &cloudv1.AwsControlPlaneLogGroup{Enabled: ptr.To(true)}
	})
	// The create's own first apply (records exist, the EKS cluster is held).
	reconcilecluster.StageAfter = 0
	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	markReady(t, w, &awscwl.Group{}, prefix+"-logs")
	reconcilecluster.StageAfter = time.Hour
	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	if err := w.kube.Get(ctx, client.ObjectKey{Namespace: worldNS, Name: prefix}, &awseks.Cluster{}); err == nil {
		t.Fatal("the next stage was applied although the row was written a moment ago")
	}
	reconcilecluster.StageAfter = 0
	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	if err := w.kube.Get(ctx, client.ObjectKey{Namespace: worldNS, Name: prefix}, &awseks.Cluster{}); err != nil {
		t.Fatalf("the next stage was not applied once the row was left alone: %v", err)
	}
}

// A takeover that fails half-way leaves a cluster that is no longer
// "imported, read-only" (deleting that must leave the cloud alone), and
// the loop finishes it.
func TestTakeOverFailingHalfWayIsFinishedByTheLoop(t *testing.T) {
	fail := true
	w := newWorld(t, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if fail && obj.GetObjectKind().GroupVersionKind().Kind == "Group" {
				return errors.New("conflict with the provider's write")
			}
			return c.Update(ctx, obj, opts...)
		},
	}, nil, withVersions, func(d *service.Deps) { d.AWSClusterDiscovery = fakeClusterDiscoverer{cluster: terraformCluster()} })
	ctx := t.Context()
	c := importTF(t, w)
	if _, err := w.svc.TakeOverCluster(ctx, &cloudv1.TakeOverClusterRequest{TenantId: worldTenant, ClusterId: c.GetId(),
		AcknowledgeNotImported: true}); err == nil {
		t.Fatal("the takeover succeeded although an object could not be taken over")
	}
	row, err := w.store.Clusters().Get(ctx, worldTenant, c.GetId())
	if err != nil || row.State == reconcilecluster.StateImported {
		t.Fatalf("state %s (%v): a cluster with objects already owned must not read as imported, read-only", row.State, err)
	}
	fail = false
	// The imported EKS cluster is ACTIVE, as its provider reports.
	eks := &awseks.Cluster{}
	eksKey := client.ObjectKey{Namespace: worldNS, Name: crossplaneaws.ClusterPrefix(worldTenant, c.GetId(), "tf")}
	if err := w.kube.Get(ctx, eksKey, eks); err != nil {
		t.Fatal(err)
	}
	eks.Status.AtProvider.Status = ptr.To("ACTIVE")
	if err := w.kube.Update(ctx, eks); err != nil {
		t.Fatal(err)
	}
	if err := w.svc.Reconcile(ctx, worldTenant, c.GetId()); err != nil {
		t.Fatal(err)
	}
	for name, u := range objectsOf(t, w, c.GetId()) {
		if got := policies(u); len(got) == 2 && got[0] == "Observe" {
			t.Errorf("%s is still Observe-only after the loop's pass: %v", name, got)
		}
	}
	// A setting that would render an object the cluster carries over is
	// refused instead of rendering it without its cloud identity.
	req := &cloudv1.UpdateClusterRequest{TenantId: worldTenant, UpdateMask: updateRequest(w, nil, "spec.aws.irsa").GetUpdateMask(),
		Cluster: &cloudv1.Cluster{Id: c.GetId(), Spec: &cloudv1.ClusterSpec{
			Provider: &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{Irsa: ptr.To(true)}}}}}
	if _, err := w.svc.UpdateCluster(ctx, req); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("irsa on a cluster that carries its OIDC provider over = %v, want FAILED_PRECONDITION", err)
	}
}

// A cluster created without the creator as admin keeps that: EKS cannot
// change it, and rendering the platform's default would break the object.
func TestImportKeepsTheClusterCreatorSetting(t *testing.T) {
	found := terraformCluster()
	found.BootstrapAdmin = ptr.To(false)
	w := importWorld(t, found)
	ctx := t.Context()
	c := importTF(t, w)
	key := client.ObjectKey{Namespace: worldNS, Name: crossplaneaws.ClusterPrefix(worldTenant, c.GetId(), "tf")}
	check := func(when string) {
		t.Helper()
		eks := &awseks.Cluster{}
		if err := w.kube.Get(ctx, key, eks); err != nil {
			t.Fatal(err)
		}
		if got := eks.Spec.ForProvider.AccessConfig.BootstrapClusterCreatorAdminPermissions; got == nil || *got {
			t.Errorf("%s: bootstrapClusterCreatorAdminPermissions = %v, want false as the cluster was created", when, got)
		}
	}
	check("after the import")
	if len(linesBy(c, cloudv1.Disposition_DISPOSITION_BLOCKING)) != 0 {
		t.Errorf("blocking: %v", keys(linesBy(c, cloudv1.Disposition_DISPOSITION_BLOCKING)))
	}
	if _, err := w.svc.TakeOverCluster(ctx, &cloudv1.TakeOverClusterRequest{TenantId: worldTenant, ClusterId: c.GetId(),
		AcknowledgeNotImported: true}); err != nil {
		t.Fatal(err)
	}
	check("after the takeover")
}
