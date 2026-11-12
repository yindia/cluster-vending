package cluster

import (
	"context"
	"errors"
	"testing"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// guard fails every write to the management cluster while it is on, and
// counts them: what a handler must never do.
type guard struct {
	on     bool
	writes []string
}

func (g *guard) deny(verb string) error {
	if !g.on {
		return nil
	}
	g.writes = append(g.writes, verb)
	return errors.New("a handler wrote to the management cluster")
}

// funcs are the client interceptors of the guard: every writing verb.
func (g *guard) funcs() interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := g.deny("create"); err != nil {
				return err
			}
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if err := g.deny("update"); err != nil {
				return err
			}
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if err := g.deny("patch"); err != nil {
				return err
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if err := g.deny("delete"); err != nil {
				return err
			}
			return c.Delete(ctx, obj, opts...)
		},
		DeleteAllOf: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteAllOfOption) error {
			if err := g.deny("deleteAllOf"); err != nil {
				return err
			}
			return c.DeleteAllOf(ctx, obj, opts...)
		},
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if err := g.deny("apply"); err != nil {
				return err
			}
			return c.Apply(ctx, obj, opts...)
		},
	}
}

// during runs a handler call with the guard on and fails the test if the
// call failed or wrote anything.
func (g *guard) during(t *testing.T, call string, fn func() error) {
	t.Helper()
	g.on, g.writes = true, nil
	err := fn()
	g.on = false
	if err != nil || len(g.writes) > 0 {
		t.Fatalf("%s: %v, writes to the management cluster %v; a handler only stores what was asked for", call, err, g.writes)
	}
}

// createRequest is a new cluster in the world's network.
func createRequest(w world, name string) *cloudv1.CreateClusterRequest {
	spec := minimalSpec()
	spec.Network.NetworkId = w.row.NetworkID
	return &cloudv1.CreateClusterRequest{TenantId: worldTenant, Cluster: &cloudv1.Cluster{Name: name, Spec: spec}}
}

// Every mutating call stores the desired state and returns: nothing is
// created, changed or deleted in the management cluster until the
// reconciler runs.
func TestHandlersWriteNothingToTheManagementCluster(t *testing.T) {
	g := &guard{}
	w := newWorld(t, g.funcs(), nil, withVersions, func(d *service.Deps) {
		d.AWSClusterDiscovery = fakeClusterDiscoverer{cluster: terraformCluster()}
	})
	ctx := t.Context()
	row := func(id string) *model.Cluster {
		t.Helper()
		r, err := w.store.Clusters().Get(ctx, worldTenant, id)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	role := func(id, name string) bool {
		t.Helper()
		err := w.kube.Get(ctx, client.ObjectKey{Namespace: worldNS, Name: crossplaneaws.ClusterPrefix(worldTenant, id, name) + "-cluster-role"}, &awsiam.Role{})
		if client.IgnoreNotFound(err) != nil {
			t.Fatal(err)
		}
		return err == nil
	}

	// Create: stored PROVISIONING, not applied.
	var created *cloudv1.Cluster
	g.during(t, "CreateCluster", func() error {
		res, err := w.svc.CreateCluster(ctx, createRequest(w, "second"))
		created = res.GetCluster()
		return err
	})
	if r := row(created.GetId()); r.State != reconcilecluster.StateProvisioning || r.AppliedGeneration >= r.Generation ||
		created.GetStatus().GetState() != cloudv1.ClusterState_CLUSTER_STATE_PROVISIONING || role(created.GetId(), "second") {
		t.Fatalf("after CreateCluster: row %s applied %d of %d, answer %s, role exists %t; want it stored, PROVISIONING and not applied",
			r.State, r.AppliedGeneration, r.Generation, created.GetStatus().GetState(), role(created.GetId(), "second"))
	}
	// The very next pass applies it: nothing waits for the call.
	reconcile(t, w, created.GetId())
	eksKey := client.ObjectKey{Namespace: worldNS, Name: crossplaneaws.ClusterPrefix(worldTenant, created.GetId(), "second")}
	if err := w.kube.Get(ctx, eksKey, &awseks.Cluster{}); err != nil || !role(created.GetId(), "second") {
		t.Fatalf("the reconciler did not apply the stored cluster: %v", err)
	}

	// Update: the spec is stored, the generation is behind.
	reconcile(t, w, w.row.ID) // the world's cluster, applied
	before := row(w.row.ID)
	g.during(t, "UpdateCluster", func() error {
		spec := minimalSpec()
		spec.DefaultNodePool.ScalingConfig.MaxSize = 9
		_, err := w.svc.UpdateCluster(ctx, updateRequest(w, spec, "spec.default_node_pool.scaling_config.max_size"))
		return err
	})
	if r := row(w.row.ID); r.Generation != before.Generation+1 || r.AppliedGeneration >= r.Generation || r.State != reconcilecluster.StateUpdating {
		t.Fatalf("after UpdateCluster: %s, applied %d of %d; want the change stored and not applied", r.State, r.AppliedGeneration, r.Generation)
	}
	reconcile(t, w, w.row.ID)
	group := &awseks.NodeGroup{}
	groupKey := client.ObjectKey{Namespace: worldNS, Name: crossplaneaws.ClusterPrefix(worldTenant, w.row.ID, w.row.Name) + "-default"}
	if err := w.kube.Get(ctx, groupKey, group); err != nil || *group.Spec.ForProvider.ScalingConfig.MaxSize != 9 {
		t.Errorf("the reconciler did not apply the stored update: %v (%v)", group.Spec.ForProvider.ScalingConfig, err)
	}

	// Import: the plan is in the answer, the objects are not created.
	var imported *cloudv1.Cluster
	g.during(t, "ImportCluster", func() error {
		res, err := w.svc.ImportCluster(ctx, &cloudv1.ImportClusterRequest{TenantId: worldTenant, Name: "tf", NetworkId: w.row.NetworkID,
			Provider: &cloudv1.ImportClusterRequest_Aws{Aws: &cloudv1.AwsImportCluster{DefaultNodeGroupName: "default-ng"}}})
		imported = res.GetCluster()
		return err
	})
	if st := imported.GetStatus(); st.GetState() != cloudv1.ClusterState_CLUSTER_STATE_IMPORTED || len(st.GetImport().GetResources()) == 0 ||
		len(objectsOf(t, w, imported.GetId())) != 0 {
		t.Fatalf("after ImportCluster: %s, %d plan lines, %d objects; want the plan in the answer and nothing applied",
			st.GetState(), len(st.GetImport().GetResources()), len(objectsOf(t, w, imported.GetId())))
	}
	// Not before its objects are there.
	takeOver := &cloudv1.TakeOverClusterRequest{TenantId: worldTenant, ClusterId: imported.GetId(), AcknowledgeNotImported: true}
	if _, err := w.svc.TakeOverCluster(ctx, takeOver); err == nil {
		t.Fatal("a cluster whose import is not applied yet was taken over")
	}
	reconcile(t, w, imported.GetId())
	objects := objectsOf(t, w, imported.GetId())
	if len(objects) == 0 {
		t.Fatal("the reconciler did not create the import's objects")
	}
	for name, u := range objects {
		if got := policies(u); len(got) == 0 || got[0] != "Observe" {
			t.Errorf("imported %s managementPolicies = %v, want Observe-only", name, got)
		}
	}

	// TakeOver: stored, nothing owned.
	g.during(t, "TakeOverCluster", func() error {
		_, err := w.svc.TakeOverCluster(ctx, takeOver)
		return err
	})
	if r := row(imported.GetId()); r.State == reconcilecluster.StateImported || r.AppliedGeneration >= r.Generation {
		t.Fatalf("after TakeOverCluster: %s, applied %d of %d; want the takeover stored and not applied", r.State, r.AppliedGeneration, r.Generation)
	}
	for name, u := range objectsOf(t, w, imported.GetId()) {
		if got := policies(u); len(got) == 0 || got[0] != "Observe" {
			t.Errorf("%s managementPolicies right after TakeOverCluster = %v: the call must not own", name, got)
		}
	}

	// Delete: stored DELETING, nothing deleted.
	g.during(t, "DeleteCluster", func() error {
		_, err := w.svc.DeleteCluster(ctx, &cloudv1.DeleteClusterRequest{TenantId: worldTenant, ClusterId: created.GetId()})
		return err
	})
	if r := row(created.GetId()); r.State != reconcilecluster.StateDeleting || !role(created.GetId(), "second") {
		t.Fatalf("after DeleteCluster: %s, role exists %t; want DELETING and everything still there", r.State, role(created.GetId(), "second"))
	}
	reconcile(t, w, created.GetId())
	if role(created.GetId(), "second") {
		t.Error("the reconciler did not start the stored deletion")
	}
}

// A cluster whose resources cannot be applied is stored all the same: the
// failure is its status, in what Get returns and in the live read, and the
// next pass tries again.
func TestCreateClusterWithoutProviderIsFailedNotRefused(t *testing.T) {
	missing := true
	w := newWorld(t, interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if missing {
				return &meta.NoKindMatchError{GroupKind: awseks.Cluster_GroupVersionKind.GroupKind()}
			}
			return c.Apply(ctx, obj, opts...)
		},
	}, nil, withVersions)
	ctx := t.Context()
	res, err := w.svc.CreateCluster(ctx, createRequest(w, "second"))
	if err != nil || res.GetCluster().GetStatus().GetState() != cloudv1.ClusterState_CLUSTER_STATE_PROVISIONING {
		t.Fatalf("CreateCluster = %s, %v; want the cluster stored as PROVISIONING", res.GetCluster().GetStatus().GetState(), err)
	}
	id := res.GetCluster().GetId()
	if err := w.svc.Reconcile(ctx, worldTenant, id); err == nil {
		t.Fatal("the pass reported no error although the provider is not installed")
	}
	const want = "a Crossplane AWS provider the cluster needs (eks, iam, ec2, kms, cloudwatchlogs) is not installed in the management cluster"
	get, err := w.svc.GetCluster(ctx, &cloudv1.GetClusterRequest{TenantId: worldTenant, ClusterId: id})
	if err != nil {
		t.Fatal(err)
	}
	for name, st := range map[string]*cloudv1.ClusterStatus{"stored": get.GetCluster().GetStatus(), "live": live(t, w, id).GetStatus()} {
		if st.GetState() != cloudv1.ClusterState_CLUSTER_STATE_FAILED || st.GetMessage() != want {
			t.Errorf("%s status = %s %q, want FAILED with the missing provider", name, st.GetState(), st.GetMessage())
		}
	}
	// Nothing was rolled back: the cluster is there to be fixed or deleted.
	if _, err := w.store.Clusters().Get(ctx, worldTenant, id); err != nil {
		t.Fatalf("the cluster's row after the failed pass: %v", err)
	}
	// It is still tried: once the provider is there the cluster is built.
	missing = false
	reconcile(t, w, id)
	if st := live(t, w, id).GetStatus(); st.GetState() == cloudv1.ClusterState_CLUSTER_STATE_FAILED || len(st.GetManagedResources()) == 0 {
		t.Errorf("after the provider is installed: %s %q, %d resources", st.GetState(), st.GetMessage(), len(st.GetManagedResources()))
	}
}
