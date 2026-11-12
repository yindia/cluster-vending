package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	recon "github.com/yindia/cluster-vending/pkg/reconcile"
)

// awsGate is every AWS lookup a cluster's reconciler makes (the EKS name,
// the versions, the discovery of an import). While closed (a handler is
// running) a lookup fails the test: only the reconciler may look at the
// cloud. It counts the lookups it lets through.
type awsGate struct {
	t         *testing.T
	closed    bool
	lookup    awscloud.ClusterLookup
	discovery awscloud.ClusterDiscoverer
	names     int
	versions  int
	discovers int
}

func newGate(t *testing.T) *awsGate {
	return &awsGate{t: t, lookup: fakeLookup{}, discovery: fakeClusterDiscoverer{cluster: terraformCluster()}}
}

// use gives the service the gate as all its AWS lookups.
func (g *awsGate) use(d *recon.Deps) { d.AWSClusters, d.AWSClusterDiscovery = g, g }

func (g *awsGate) refuse(what string) error {
	g.t.Helper()
	g.t.Errorf("a handler looked the cloud up (%s): only the reconciler may", what)
	return errors.New("no AWS in a handler")
}

func (g *awsGate) EKSClusterExists(ctx context.Context, a awscloud.Access, region, name string) (bool, error) {
	if g.closed {
		return false, g.refuse("EKS cluster name")
	}
	g.names++
	return g.lookup.EKSClusterExists(ctx, a, region, name)
}

func (g *awsGate) ResolveEKSVersions(ctx context.Context, a awscloud.Access, region string, req awscloud.VersionRequest) (awscloud.VersionResult, error) {
	if g.closed {
		return awscloud.VersionResult{}, g.refuse("EKS versions")
	}
	g.versions++
	return g.lookup.ResolveEKSVersions(ctx, a, region, req)
}

func (g *awsGate) DiscoverCluster(ctx context.Context, a awscloud.Access, region, name, nodeGroup string) (*awscloud.Cluster, error) {
	if g.closed {
		return nil, g.refuse("EKS cluster discovery")
	}
	g.discovers++
	return g.discovery.DiscoverCluster(ctx, a, region, name, nodeGroup)
}

func mask(paths ...string) *fieldmaskpb.FieldMask { return &fieldmaskpb.FieldMask{Paths: paths} }

// failingDiscoverer fails every discovery with err.
type failingDiscoverer struct{ err error }

func (f failingDiscoverer) DiscoverCluster(context.Context, awscloud.Access, string, string, string) (*awscloud.Cluster, error) {
	return nil, f.err
}

// storedCluster is the cluster as GetCluster returns it.
func storedCluster(t *testing.T, w world, id string) *cloudv1.Cluster {
	t.Helper()
	res, err := w.svc.GetCluster(t.Context(), &cloudv1.GetClusterRequest{TenantId: worldTenant, ClusterId: id})
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	return res.GetCluster()
}

// A name EKS already has does not fail the call: the cluster is stored,
// and the reconciler, which asks EKS before it creates anything, reports
// it as the cluster's status. Nothing is created.
func TestCreateClusterWithATakenEKSNameIsFailedNotRefused(t *testing.T) {
	gate := newGate(t)
	gate.lookup = fakeLookup{exists: true}
	w := newWorld(t, interceptor.Funcs{}, nil, gate.use)
	ctx := t.Context()
	gate.closed = true
	res, err := w.svc.CreateCluster(ctx, createRequest(w, "second"))
	gate.closed = false
	if err != nil || res.GetCluster().GetStatus().GetState() != cloudv1.ClusterState_CLUSTER_STATE_PROVISIONING {
		t.Fatalf("CreateCluster = %s, %v; want the cluster stored as PROVISIONING", res.GetCluster().GetStatus().GetState(), err)
	}
	id := res.GetCluster().GetId()
	if err := w.svc.Reconcile(ctx, worldTenant, id); err == nil {
		t.Fatal("the pass reported no error although EKS has a cluster of the name")
	}
	const want = `account 444455556666 already has an EKS cluster named "second" in us-east-1 (not created by the platform); choose another name`
	for name, st := range map[string]*cloudv1.ClusterStatus{"stored": storedCluster(t, w, id).GetStatus(), "live": live(t, w, id).GetStatus()} {
		if st.GetState() != cloudv1.ClusterState_CLUSTER_STATE_FAILED || st.GetMessage() != want {
			t.Errorf("%s status = %s %q, want FAILED with the taken name", name, st.GetState(), st.GetMessage())
		}
	}
	if objects := objectsOf(t, w, id); len(objects) != 0 {
		t.Errorf("%d objects were created for a cluster whose name is taken", len(objects))
	}
	// The platform's own cluster of the name is refused by the call: that
	// needs the store only.
	if _, err := w.svc.CreateCluster(ctx, createRequest(w, "second")); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("a second cluster of the name = %v, want ALREADY_EXISTS from the store", err)
	}
}

// The versions are resolved by the reconciler, once per generation that
// needs it, and written to the stored spec; the cluster as sent is not
// changed. A version EKS does not offer is the cluster's status.
func TestVersionsAreResolvedByTheReconciler(t *testing.T) {
	gate := newGate(t)
	w := newWorld(t, interceptor.Funcs{}, nil, gate.use)
	ctx := t.Context()
	req := createRequest(w, "second")
	req.GetCluster().GetSpec().GetAws().Addons = []*cloudv1.AwsAddon{{Name: "coredns", Version: "v0-1.34"}}
	gate.closed = true
	res, err := w.svc.CreateCluster(ctx, req)
	gate.closed = false
	if err != nil {
		t.Fatal(err)
	}
	id := res.GetCluster().GetId()
	if v := res.GetCluster().GetSpec().GetControlPlane().GetKubernetesVersion(); v != "" {
		t.Errorf("the call resolved the Kubernetes version (%s) without a lookup", v)
	}
	reconcile(t, w, id)
	reconcile(t, w, id)
	c := storedCluster(t, w, id)
	v := versionsOf(c.GetSpec())
	if c.GetSpec().GetControlPlane().GetKubernetesVersion() != "1.34" || v["coredns"] != "v0-1.34" || v["vpc-cni"] != "v1-1.34" {
		t.Errorf("stored spec: kubernetes %s, add-ons %v; want the EKS default, the pin and the recommended versions",
			c.GetSpec().GetControlPlane().GetKubernetesVersion(), v)
	}
	if gate.names != 1 || gate.versions != 1 {
		t.Errorf("%d name checks and %d version lookups over two passes, want one of each", gate.names, gate.versions)
	}
	row, err := w.store.Clusters().Get(ctx, worldTenant, id)
	if err != nil || row.ResolvedGeneration != row.Generation {
		t.Fatalf("resolved generation %d of %d (%v)", row.ResolvedGeneration, row.Generation, err)
	}
	eks := &awseks.Cluster{}
	if err := w.kube.Get(ctx, client.ObjectKey{Namespace: worldNS, Name: crossplaneaws.ClusterPrefix(worldTenant, id, "second")}, eks); err != nil ||
		eks.Spec.ForProvider.Version == nil || *eks.Spec.ForProvider.Version != "1.34" {
		t.Errorf("the EKS cluster was not rendered with the resolved version: %v (%v)", eks.Spec.ForProvider.Version, err)
	}

	// A change that has nothing to look up is not resolved again.
	spec := minimalSpec()
	spec.DefaultNodePool.ScalingConfig.MaxSize = 9
	if _, err := w.svc.UpdateCluster(ctx, &cloudv1.UpdateClusterRequest{TenantId: worldTenant,
		Cluster:    &cloudv1.Cluster{Id: id, Spec: spec},
		UpdateMask: mask("spec.default_node_pool.scaling_config.max_size")}); err != nil {
		t.Fatal(err)
	}
	reconcile(t, w, id)
	if gate.versions != 1 {
		t.Errorf("%d version lookups after a scaling change, want still one", gate.versions)
	}

	// An upgrade the pinned add-on does not fit: stored by the call,
	// FAILED by the reconciler, and the cluster is not touched.
	gate.closed = true
	_, err = w.svc.UpdateCluster(ctx, &cloudv1.UpdateClusterRequest{TenantId: worldTenant,
		Cluster:    &cloudv1.Cluster{Id: id, Spec: &cloudv1.ClusterSpec{ControlPlane: &cloudv1.ControlPlaneSpec{KubernetesVersion: "1.35"}}},
		UpdateMask: mask("spec.control_plane.kubernetes_version")})
	gate.closed = false
	if err != nil {
		t.Fatalf("UpdateCluster: %v; the call stores the change whatever EKS offers", err)
	}
	if err := w.svc.Reconcile(ctx, worldTenant, id); err == nil {
		t.Fatal("the pass reported no error although the pinned add-on does not fit 1.35")
	}
	st := storedCluster(t, w, id).GetStatus()
	if st.GetState() != cloudv1.ClusterState_CLUSTER_STATE_FAILED || !strings.Contains(st.GetMessage(), "coredns v0-1.34 not compatible with 1.35") {
		t.Errorf("status = %s %q, want FAILED with the add-on that does not fit", st.GetState(), st.GetMessage())
	}
	if err := w.kube.Get(ctx, client.ObjectKey{Namespace: worldNS, Name: crossplaneaws.ClusterPrefix(worldTenant, id, "second")}, eks); err != nil ||
		*eks.Spec.ForProvider.Version != "1.34" {
		t.Errorf("the EKS cluster was changed although the versions could not be resolved: %v (%v)", eks.Spec.ForProvider.Version, err)
	}
}

// What the discovery of an imported cluster finds wrong is the cluster's
// status, not the answer of the call; a lookup that did not answer is
// tried again, and the cluster is looked up once.
func TestImportClusterErrorsAreStatus(t *testing.T) {
	for name, tc := range map[string]struct {
		err     error
		state   cloudv1.ClusterState
		message string
	}{
		"lookup failed": {errors.New("timeout"), cloudv1.ClusterState_CLUSTER_STATE_IMPORTED, "retrying: the AWS lookup failed: timeout"},
		"access denied": {awscloud.ErrAccessDenied, cloudv1.ClusterState_CLUSTER_STATE_FAILED, "cannot read the cluster with the account's credentials"},
		"not found":     {awscloud.ErrNotFound, cloudv1.ClusterState_CLUSTER_STATE_FAILED, "not found"},
	} {
		t.Run(name, func(t *testing.T) {
			gate := newGate(t)
			w := newWorld(t, interceptor.Funcs{}, nil, gate.use)
			ctx := t.Context()
			gate.closed = true
			res, err := w.svc.ImportCluster(ctx, &cloudv1.ImportClusterRequest{TenantId: worldTenant, Name: "tf", NetworkId: w.row.NetworkID,
				Provider: &cloudv1.ImportClusterRequest_Aws{Aws: &cloudv1.AwsImportCluster{DefaultNodeGroupName: "default-ng"}}})
			gate.closed = false
			if err != nil {
				t.Fatalf("ImportCluster: %v; the call stores the import whatever the cloud says", err)
			}
			id := res.GetCluster().GetId()
			if st := res.GetCluster().GetStatus(); st.GetState() != cloudv1.ClusterState_CLUSTER_STATE_IMPORTED ||
				st.GetImport().GetPhase() != cloudv1.ImportPhase_IMPORT_PHASE_DISCOVERING {
				t.Errorf("right after the call = %s / %s, want IMPORTED and DISCOVERING", st.GetState(), st.GetImport().GetPhase())
			}
			gate.discovery = failingDiscoverer{tc.err}
			if err := w.svc.Reconcile(ctx, worldTenant, id); err == nil {
				t.Error("the pass reported no error")
			}
			for view, st := range map[string]*cloudv1.ClusterStatus{"stored": storedCluster(t, w, id).GetStatus(), "live": live(t, w, id).GetStatus()} {
				if st.GetState() != tc.state || !strings.Contains(st.GetMessage(), tc.message) ||
					st.GetImport().GetPhase() != cloudv1.ImportPhase_IMPORT_PHASE_DISCOVERING {
					t.Errorf("%s status = %s %q (%s), want %s with %q, still DISCOVERING", view, st.GetState(), st.GetMessage(),
						st.GetImport().GetPhase(), tc.state, tc.message)
				}
			}
			if objects := objectsOf(t, w, id); len(objects) != 0 {
				t.Errorf("a failed lookup created %d objects", len(objects))
			}
			// Once the cloud answers the import goes on, and is not looked
			// up again after that.
			gate.discovery = fakeClusterDiscoverer{cluster: terraformCluster()}
			before := gate.discovers
			reconcile(t, w, id)
			reconcile(t, w, id)
			if st := storedCluster(t, w, id).GetStatus(); st.GetImport().GetPhase() != cloudv1.ImportPhase_IMPORT_PHASE_PLANNED ||
				len(st.GetImport().GetResources()) == 0 {
				t.Errorf("after the lookup succeeded = %s / %s, %d plan lines", st.GetState(), st.GetImport().GetPhase(), len(st.GetImport().GetResources()))
			}
			if got := gate.discovers - before; got != 1 {
				t.Errorf("%d lookups of the cluster once the cloud answered, want one", got)
			}
			if len(objectsOf(t, w, id)) == 0 {
				t.Error("the reconciler did not create the import's objects")
			}
		})
	}
}
