package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	awscwl "github.com/upbound/provider-aws/v2/apis/namespaced/cloudwatchlogs/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	recon "github.com/yindia/cluster-vending/pkg/reconcile"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

// An update that names an optional setting in its mask but leaves the
// message holding it out resets the setting; it must not crash.
func TestUpdateWithAMaskedFieldLeftOut(t *testing.T) {
	w := appliedWorld(t)
	for _, path := range []string{"spec.aws.irsa", "spec.control_plane.endpoint_access.endpoint_private_access",
		"spec.control_plane.endpoint_access.endpoint_public_access"} {
		for name, spec := range map[string]*cloudv1.ClusterSpec{"no spec": nil, "empty spec": {}} {
			if _, err := update(t, w, updateRequest(w, spec, path)); err != nil {
				t.Errorf("%s with %s: %v", path, name, err)
			}
		}
	}
}

// A stored cluster is applied by the very next pass, and so is each of its
// stages: nothing waits for the call that stored it, which applies
// nothing.
func TestStagesMoveOnWithEveryPass(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	ctx := t.Context()
	prefix := crossplaneaws.ClusterPrefix(worldTenant, w.row.ID, w.row.Name)
	newCluster(t, w, func(s *cloudv1.ClusterSpec) {
		s.GetAws().ControlPlaneLogGroup = &cloudv1.AwsControlPlaneLogGroup{Enabled: ptr.To(true)}
	})
	eks := client.ObjectKey{Namespace: worldNS, Name: prefix}
	// The first pass: the stage before the EKS cluster.
	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	if err := w.kube.Get(ctx, client.ObjectKey{Namespace: worldNS, Name: prefix + "-logs"}, &awscwl.Group{}); err != nil {
		t.Fatalf("the first pass did not apply the log group: %v", err)
	}
	if err := w.kube.Get(ctx, eks, &awseks.Cluster{}); err == nil {
		t.Fatal("the EKS cluster was applied before its log group is Ready")
	}
	markReady(t, w, &awscwl.Group{}, prefix+"-logs")
	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	if err := w.kube.Get(ctx, eks, &awseks.Cluster{}); err != nil {
		t.Fatalf("the next stage was not applied by the next pass: %v", err)
	}
}

// A takeover is stored first and carried out by the reconciler. One that
// fails half-way leaves a cluster that is no longer "imported, read-only"
// (deleting that must leave the cloud alone): the failure is its status,
// and the next pass finishes it.
func TestTakeOverFailingHalfWayIsFinishedByTheLoop(t *testing.T) {
	fail := false
	w := newWorld(t, interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if body, err := json.Marshal(obj); fail && err == nil && strings.Contains(string(body), `"kind":"Group"`) {
				return errors.New("the API server went away")
			}
			return c.Apply(ctx, obj, opts...)
		},
	}, nil, withVersions, func(d *recon.Deps) { d.AWSClusterDiscovery = fakeClusterDiscoverer{cluster: terraformCluster()} })
	ctx := t.Context()
	c := importTF(t, w)
	if _, err := w.svc.TakeOverCluster(ctx, &cloudv1.TakeOverClusterRequest{TenantId: worldTenant, ClusterId: c.GetId(),
		AcknowledgeNotImported: true}); err != nil {
		t.Fatalf("TakeOverCluster only stores the takeover: %v", err)
	}
	fail = true
	if err := w.svc.Reconcile(ctx, worldTenant, c.GetId()); err == nil {
		t.Fatal("the pass succeeded although an object could not be taken over")
	}
	row, err := w.store.Clusters().Get(ctx, worldTenant, c.GetId())
	if err != nil || row.State == reconcilecluster.StateImported || row.AppliedGeneration >= row.Generation {
		t.Fatalf("state %s, applied %d of %d (%v): a cluster with objects already owned must not read as imported, read-only",
			row.State, row.AppliedGeneration, row.Generation, err)
	}
	st, err := w.svc.GetClusterStatus(ctx, &cloudv1.GetClusterStatusRequest{TenantId: worldTenant, ClusterId: c.GetId()})
	if err != nil || st.GetCluster().GetStatus().GetMessage() != "retrying: could not create the cluster's resources" {
		t.Errorf("status after the failed pass = %q (%v), want the failure being retried", st.GetCluster().GetStatus().GetMessage(), err)
	}
	fail = false
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
	if _, err := update(t, w, req); connect.CodeOf(err) != connect.CodeFailedPrecondition {
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
	reconcile(t, w, c.GetId())
	check("after the takeover")
}
