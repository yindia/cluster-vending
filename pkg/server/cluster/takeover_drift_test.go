package cluster

import (
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	recon "github.com/yindia/cluster-vending/pkg/reconcile"
)

func takeOverRequest(c *cloudv1.Cluster) *cloudv1.TakeOverClusterRequest {
	return &cloudv1.TakeOverClusterRequest{TenantId: worldTenant, ClusterId: c.GetId(), AcknowledgeNotImported: true}
}

// observeAsImported makes the imported EKS cluster report what the plan
// holds (nothing drifted since the import).
func observeAsImported(t *testing.T, w world, c *cloudv1.Cluster) {
	t.Helper()
	eks := &awseks.Cluster{}
	key := client.ObjectKey{Namespace: worldNS, Name: crossplaneaws.ClusterPrefix(worldTenant, c.GetId(), "tf")}
	if err := w.kube.Get(t.Context(), key, eks); err != nil {
		t.Fatal(err)
	}
	eks.Status.AtProvider.Version = ptr.To("1.31")
	eks.Status.AtProvider.VPCConfig = &awseks.VPCConfigObservation{EndpointPrivateAccess: ptr.To(true),
		EndpointPublicAccess: ptr.To(true), PublicAccessCidrs: []*string{ptr.To("203.0.113.0/24")}}
	if err := w.kube.Update(t.Context(), eks); err != nil {
		t.Fatal(err)
	}
}

// A takeover applies the reviewed plan: when what the cluster reports now
// differs from it in a field full management writes, the takeover is
// refused with the differences and the cluster stays imported.
func TestTakeOverRefusesADriftedImport(t *testing.T) {
	cases := map[string]struct {
		change func(*awseks.Cluster)
		want   []string
	}{
		"version": {func(c *awseks.Cluster) { c.Status.AtProvider.Version = ptr.To("1.32") }, []string{"version", "1.31", "1.32"}},
		"public cidrs": {func(c *awseks.Cluster) {
			c.Status.AtProvider.VPCConfig.PublicAccessCidrs = []*string{ptr.To("198.51.100.0/24")}
		}, []string{"public access CIDRs", "198.51.100.0/24"}},
		"private flag": {func(c *awseks.Cluster) { c.Status.AtProvider.VPCConfig.EndpointPrivateAccess = ptr.To(false) }, []string{"private endpoint access"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			w := importWorld(t, terraformCluster())
			c := importTF(t, w)
			observeAsImported(t, w, c)
			eks := &awseks.Cluster{}
			key := client.ObjectKey{Namespace: worldNS, Name: crossplaneaws.ClusterPrefix(worldTenant, c.GetId(), "tf")}
			if err := w.kube.Get(t.Context(), key, eks); err != nil {
				t.Fatal(err)
			}
			tc.change(eks)
			if err := w.kube.Update(t.Context(), eks); err != nil {
				t.Fatal(err)
			}

			_, err := w.svc.TakeOverCluster(t.Context(), takeOverRequest(c))

			if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "import it again") {
				t.Fatalf("err = %v, want FAILED_PRECONDITION telling to import again", err)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err = %v, want it to mention %q", err, w)
				}
			}
		})
	}
}

func TestTakeOverRefusesADriftedNodeGroupAndAddon(t *testing.T) {
	w := importWorld(t, terraformCluster())
	c := importTF(t, w)
	observeAsImported(t, w, c)
	groups := &awseks.NodeGroupList{}
	if err := w.kube.List(t.Context(), groups); err != nil || len(groups.Items) == 0 {
		t.Fatalf("node groups: %v, %d", err, len(groups.Items))
	}
	g := groups.Items[0]
	g.Status.AtProvider.ScalingConfig = &awseks.ScalingConfigObservation{MinSize: ptr.To(2.0), MaxSize: ptr.To(9.0), DesiredSize: ptr.To(3.0)}
	if err := w.kube.Update(t.Context(), &g); err != nil {
		t.Fatal(err)
	}
	addons := &awseks.AddonList{}
	if err := w.kube.List(t.Context(), addons); err != nil || len(addons.Items) == 0 {
		t.Fatalf("addons: %v, %d", err, len(addons.Items))
	}
	a := addons.Items[0]
	a.Status.AtProvider.AddonVersion = ptr.To("v9.9.9-eksbuild.1")
	if err := w.kube.Update(t.Context(), &a); err != nil {
		t.Fatal(err)
	}

	_, err := w.svc.TakeOverCluster(t.Context(), takeOverRequest(c))

	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "max size") || !strings.Contains(err.Error(), "v9.9.9-eksbuild.1") {
		t.Fatalf("err = %v, want FAILED_PRECONDITION listing the node group scaling and the add-on version", err)
	}
}

func TestTakeOverProceedsWithoutDrift(t *testing.T) {
	w := importWorld(t, terraformCluster())
	c := importTF(t, w)
	observeAsImported(t, w, c)

	if _, err := w.svc.TakeOverCluster(t.Context(), takeOverRequest(c)); err != nil {
		t.Fatalf("TakeOverCluster without drift = %v, want accepted", err)
	}
}

// A takeover also needs the release the cluster will use to offer the
// minor it runs: otherwise every platform component fails.
func TestTakeOverChecksTheReleaseMinor(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil, withVersions, func(d *recon.Deps) {
		d.AWSClusterDiscovery = fakeClusterDiscoverer{cluster: terraformCluster()}
		d.Releases = releases{}
	})
	c := importTF(t, w)
	observeAsImported(t, w, c)

	_, err := w.svc.TakeOverCluster(t.Context(), takeOverRequest(c))

	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "1.31") || !strings.Contains(err.Error(), "1.37, 1.36, 1.35") {
		t.Fatalf("err = %v, want FAILED_PRECONDITION naming 1.31 and the offered minors", err)
	}
	req := takeOverRequest(c)
	req.SkipPlatformComponents = true
	if _, err := w.svc.TakeOverCluster(t.Context(), req); err != nil {
		t.Errorf("skip_platform_components = %v, want accepted: no components are installed", err)
	}
}

// A cluster that is CREATING or UPDATING when it is looked up stays
// DISCOVERING (tried again), and is planned once it is ACTIVE.
func TestImportOfATransientClusterWaits(t *testing.T) {
	found := terraformCluster()
	found.Status = "UPDATING"
	w := newWorld(t, interceptor.Funcs{}, nil, withVersions, func(d *recon.Deps) {
		d.AWSClusterDiscovery = fakeClusterDiscoverer{cluster: found}
	})
	res, err := w.svc.ImportCluster(t.Context(), &cloudv1.ImportClusterRequest{TenantId: worldTenant, Name: "tf", NetworkId: w.row.NetworkID,
		Provider: &cloudv1.ImportClusterRequest_Aws{Aws: &cloudv1.AwsImportCluster{DefaultNodeGroupName: "default-ng"}}})
	if err != nil {
		t.Fatal(err)
	}
	id := res.GetCluster().GetId()

	if err := w.svc.Reconcile(t.Context(), worldTenant, id); err == nil {
		t.Fatal("Reconcile of an UPDATING cluster = nil, want an error (tried again)")
	}
	if got := live(t, w, id).GetStatus().GetImport().GetPhase(); got != cloudv1.ImportPhase_IMPORT_PHASE_DISCOVERING {
		t.Fatalf("phase = %s, want DISCOVERING", got)
	}

	found.Status = "ACTIVE"
	reconcile(t, w, id)
	if got := live(t, w, id).GetStatus().GetImport().GetPhase(); got != cloudv1.ImportPhase_IMPORT_PHASE_PLANNED {
		t.Fatalf("phase = %s, want PLANNED once ACTIVE", got)
	}
}

// A takeover renders every planned object fully managed: one the customer
// deleted since the import (its object gone, or its Observe-only object
// reporting the cloud resource does not exist) would be created again, so
// the takeover is refused naming it and telling to import again. One
// Crossplane has not reported on yet is refused too, but only until it
// has: the message says to wait and retry, not to import again.
func TestTakeOverRefusesAnImportWhoseResourcesAreGone(t *testing.T) {
	association := func(t *testing.T, w world) awseks.AccessPolicyAssociation {
		t.Helper()
		list := &awseks.AccessPolicyAssociationList{}
		if err := w.kube.List(t.Context(), list); err != nil || len(list.Items) == 0 {
			t.Fatalf("access policy associations: %v, %d", err, len(list.Items))
		}
		return list.Items[0]
	}
	setConditions := func(t *testing.T, w world, a *awseks.AccessPolicyAssociation, conds ...xpv2.Condition) {
		t.Helper()
		a.Status.Conditions = nil
		a.SetConditions(conds...)
		if err := w.kube.Update(t.Context(), a); err != nil {
			t.Fatal(err)
		}
	}
	importAgain := "delete the import and import it again"
	waitRetry := "wait for the import to finish observing its resources, then retry the takeover"
	cases := map[string]struct {
		change  func(t *testing.T, w world) string
		want    []string
		notWant string
	}{
		"object deleted": {func(t *testing.T, w world) string {
			a := association(t, w)
			if err := w.kube.Delete(t.Context(), &a); err != nil {
				t.Fatal(err)
			}
			return a.Name
		}, []string{"gone from the management cluster", importAgain}, waitRetry},
		"external resource gone": {func(t *testing.T, w world) string {
			a := association(t, w)
			setConditions(t, w, &a, xpv2.Available(), xpv2.ReconcileError(errors.New("external resource does not exist")))
			return a.Name
		}, []string{"Crossplane reports its cloud resource missing or failing", "external resource does not exist", importAgain}, waitRetry},
		"not reported yet": {func(t *testing.T, w world) string {
			a := association(t, w)
			setConditions(t, w, &a)
			return a.Name
		}, []string{"Crossplane has not reported on its cloud resource yet", waitRetry}, importAgain},
		"synced unknown": {func(t *testing.T, w world) string {
			a := association(t, w)
			setConditions(t, w, &a, xpv2.Available(), xpv2.Condition{Type: xpv2.TypeSynced, Status: "Unknown", Reason: "Pending"})
			return a.Name
		}, []string{"Crossplane has not reported on its cloud resource yet", waitRetry}, importAgain},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			w := importWorld(t, terraformCluster())
			c := importTF(t, w)
			observeAsImported(t, w, c)
			gone := tc.change(t, w)

			_, err := w.svc.TakeOverCluster(t.Context(), takeOverRequest(c))

			if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "AccessPolicyAssociation "+gone) {
				t.Fatalf("err = %v, want FAILED_PRECONDITION naming AccessPolicyAssociation %s", err, gone)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want it to say %q", err, want)
				}
			}
			if strings.Contains(err.Error(), tc.notWant) || strings.Contains(err.Error(), "changed since it was imported") {
				t.Errorf("err = %v, want it not to say %q or that the cluster changed", err, tc.notWant)
			}
			if got := storedCluster(t, w, c.GetId()).GetStatus().GetState(); got != cloudv1.ClusterState_CLUSTER_STATE_IMPORTED {
				t.Errorf("state = %s, want still IMPORTED", got)
			}
		})
	}
}
