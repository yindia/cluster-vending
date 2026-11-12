package cluster

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	protectionv1beta1 "github.com/crossplane/crossplane/apis/v2/protection/v1beta1"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database"
	"github.com/yindia/cluster-vending/pkg/database/implementation"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// world is a cluster in a real (SQLite) store on a network whose subnets
// exist in a fake management cluster, its EKS cluster ACTIVE.
type world struct {
	svc   *ClusterService
	store repository.Store
	kube  client.Client
	row   *model.Cluster
}

const worldTenant, worldNS = "acme", "tenant-acme"

func newWorld(t *testing.T, funcs interceptor.Funcs, reader client.Reader, opts ...func(*service.Deps)) world {
	t.Helper()
	ctx := t.Context()
	db, err := database.Open(ctx, database.Config{Driver: database.DriverSQLite, DSN: filepath.Join(t.TempDir(), "t.db"), MaxOpenConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(db) })
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := implementation.New(db)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(store.Tenants().Create(ctx, &model.Tenant{ID: worldTenant}))
	account := &model.CloudAccount{TenantID: worldTenant, Provider: model.ProviderAWS, AccountID: "444455556666",
		ProviderConfigName: "aws-0a1b2c3d", Namespace: worldNS}
	must(store.Tenants().PutCloudAccount(ctx, account))
	account, err = store.Tenants().GetCloudAccount(ctx, worldTenant, model.ProviderAWS, "444455556666")
	must(err)
	network := &model.Network{TenantID: worldTenant, Name: "net", CloudAccountID: account.ID, Provider: model.ProviderAWS,
		Region: "us-east-1", Resource: model.Resource{State: "NETWORK_STATE_PROVISIONING", Spec: model.JSON(`{}`)}}
	must(store.Networks().Create(ctx, network))

	scheme, err := kube.Scheme()
	must(err)
	var objects []client.Object
	var records []model.ManagedResource
	for i, zone := range []string{"use1-az1", "use1-az2"} {
		name := "net-private-" + zone
		subnet := &awsec2.Subnet{ObjectMeta: metav1.ObjectMeta{Namespace: worldNS, Name: name,
			Labels:      map[string]string{crossplane.LabelNetwork: network.ID, crossplane.LabelSubnetRole: "private", crossplane.LabelZone: zone},
			Annotations: map[string]string{crossplaneaws.ExternalNameAnnotation: "subnet-" + string(rune('a'+i))}}}
		subnet.Spec.ForProvider.Region = ptr.To("us-east-1")
		objects = append(objects, subnet)
		records = append(records, model.ManagedResource{TenantID: worldTenant, CloudAccountID: account.ID,
			OwnerKind: model.OwnerNetwork, OwnerID: network.ID, APIVersion: "ec2.aws.m.upbound.io/v1beta1", Kind: "Subnet",
			Namespace: worldNS, Name: name})
	}
	must(store.ManagedResources().Put(ctx, records))

	spec := minimalSpec()
	spec.Network.NetworkId = network.ID
	raw, err := encode(mustResolve(t, spec, []string{"use1-az1", "use1-az2"}))
	must(err)
	row := &model.Cluster{TenantID: worldTenant, Name: "prod", NetworkID: network.ID, Provider: model.ProviderAWS, Region: "us-east-1",
		Resource: model.Resource{State: stateUpdating, Generation: 2, AppliedGeneration: 1, Spec: raw}}
	must(store.Clusters().Create(ctx, row))

	prefix := crossplaneaws.ClusterPrefix(worldTenant, row.ID, row.Name)
	eks := &awseks.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: worldNS, Name: prefix,
		Labels: crossplane.ClusterLabels(worldTenant, row.ID)}}
	eks.Spec.ForProvider.Region = ptr.To("us-east-1")
	eks.Status.AtProvider.Status = ptr.To("ACTIVE")
	objects = append(objects, eks)
	must(store.ManagedResources().Put(ctx, []model.ManagedResource{{TenantID: worldTenant, CloudAccountID: account.ID,
		OwnerKind: model.OwnerCluster, OwnerID: row.ID, APIVersion: "eks.aws.m.upbound.io/v1beta1", Kind: "Cluster",
		Namespace: worldNS, Name: prefix}}))

	if funcs.Apply == nil {
		funcs.Apply = dryRunApply(scheme)
	}
	k := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithInterceptorFuncs(funcs).Build()
	deps := service.Deps{Store: store, Kube: k, KubeReader: reader}
	for _, opt := range opts {
		opt(&deps)
	}
	return world{svc: NewClusterService(deps), store: store, kube: k, row: row}
}

func (w world) cluster(t *testing.T) *model.Cluster {
	t.Helper()
	row, err := w.store.Clusters().Get(t.Context(), worldTenant, w.row.ID)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// A generation whose apply failed is applied again by the loop, and counts
// as applied only once every object went through.
func TestReconcileAppliesAgainAfterAFailedApply(t *testing.T) {
	failures := 1
	w := newWorld(t, interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if u, ok := obj.(interface{ GetKind() string }); ok && u.GetKind() == "NodeGroup" && failures > 0 {
				failures--
				return errors.New("apiserver unavailable")
			}
			return c.Apply(ctx, obj, opts...)
		},
	}, nil)
	ctx := t.Context()

	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err == nil {
		t.Fatal("the failed apply must surface")
	}
	if got := w.cluster(t); got.AppliedGeneration != 1 {
		t.Fatalf("applied generation %d after a failed apply, want 1", got.AppliedGeneration)
	}
	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	got := w.cluster(t)
	if got.AppliedGeneration != 2 || got.RenderVersion != crossplaneaws.RenderVersion {
		t.Errorf("applied generation %d render version %d, want 2 and %d", got.AppliedGeneration, got.RenderVersion, crossplaneaws.RenderVersion)
	}
	prefix := crossplaneaws.ClusterPrefix(worldTenant, w.row.ID, w.row.Name)
	if err := w.kube.Get(ctx, client.ObjectKey{Namespace: worldNS, Name: prefix + "-default"}, &awseks.NodeGroup{}); err != nil {
		t.Errorf("node group not applied: %v", err)
	}
	usages := &protectionv1beta1.UsageList{}
	if err := w.kube.List(ctx, usages, client.InNamespace(worldNS)); err != nil || len(usages.Items) != 13 {
		t.Errorf("%d usages (%v), want 13 (11 + the 2 subnets)", len(usages.Items), err)
	}
}

// A deletion finishes through Reconcile alone: no GetCluster needed.
func TestReconcileFinishesADeletionWithoutReads(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	ctx := t.Context()
	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil { // applies everything
		t.Fatal(err)
	}
	row := w.cluster(t)
	row.State = stateDeleting
	if err := w.store.Clusters().Update(ctx, row); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if _, err := w.store.Clusters().Get(ctx, worldTenant, w.row.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("cluster row after the deletion: %v, want not found", err)
	}
	roles := &awsiam.RoleList{}
	usages := &protectionv1beta1.UsageList{}
	if err := errors.Join(w.kube.List(ctx, roles), w.kube.List(ctx, usages)); err != nil || len(roles.Items)+len(usages.Items) != 0 {
		t.Errorf("%d roles and %d usages left (%v)", len(roles.Items), len(usages.Items), err)
	}
	subnets := &awsec2.SubnetList{}
	if err := w.kube.List(ctx, subnets); err != nil || len(subnets.Items) != 2 {
		t.Errorf("%d network subnets left (%v), want the network's 2 untouched", len(subnets.Items), err)
	}
}

// The cache saying "all gone" does not make the platform forget a cluster
// the API server still has objects of.
func TestDeletionWaitsForTheAPIServerToConfirm(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	direct := fake.NewClientBuilder().WithScheme(scheme).Build()
	w := newWorld(t, interceptor.Funcs{}, direct) // the cache (w.kube) and the API server (direct) differ
	ctx := t.Context()
	role := &awsiam.Role{ObjectMeta: metav1.ObjectMeta{Namespace: worldNS, Name: "leftover-role",
		Labels: crossplane.ClusterLabels(worldTenant, w.row.ID), Finalizers: []string{"finalizer.managedresource.crossplane.io"}}}
	if err := direct.Create(ctx, role); err != nil {
		t.Fatal(err)
	}
	row := w.cluster(t)
	row.State = stateDeleting
	if err := w.store.Clusters().Update(ctx, row); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if _, err := w.store.Clusters().Get(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatalf("cluster forgotten while the API server still has its role: %v", err)
	}
}

// Objects recorded a moment ago that the reader has not seen yet read as
// CREATING: a create or update response never says FAILED for them.
func TestFreshlyRecordedObjectsAreNotFailed(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	ctx := t.Context()
	records := []model.ManagedResource{{Base: model.Base{CreatedAt: time.Now()}, APIVersion: "iam.aws.m.upbound.io/v1beta1",
		Kind: "Role", Namespace: worldNS, Name: "not-there-yet"}}
	c, err := w.svc.viewOf(ctx, w.cluster(t), records)
	if err != nil {
		t.Fatal(err)
	}
	if st := c.GetStatus(); st.GetState() == cloudv1.ClusterState_CLUSTER_STATE_FAILED ||
		st.GetManagedResources()[0].GetState() != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_CREATING {
		t.Errorf("state %s, resource %s; want not FAILED and CREATING", st.GetState(), st.GetManagedResources()[0].GetState())
	}
	records[0].CreatedAt = time.Now().Add(-10 * time.Minute)
	c, err = w.svc.viewOf(ctx, w.cluster(t), records)
	if err != nil || c.GetStatus().GetState() != cloudv1.ClusterState_CLUSTER_STATE_FAILED {
		t.Errorf("an object missing for 10 minutes: state %s (%v), want FAILED", c.GetStatus().GetState(), err)
	}
}

// dryRunApply makes the fake client honour dry-run applies like the API
// server (the fake persists them): the apply runs against a scratch copy
// of the live object, so the result is the merged object and nothing is
// stored.
func dryRunApply(scheme *runtime.Scheme) func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
	return func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
		ao := &client.ApplyOptions{}
		ao.ApplyOptions(opts)
		if strings.HasPrefix(ao.FieldManager, kube.FieldOwner+"/cluster-") {
			// The cluster's tags on the network's subnets (tagSubnets): the
			// fake round-trips applies through the Go types, which send
			// forProvider.region as null and conflict; the API server does
			// not (see subnets_test.go).
			return nil
		}
		if !slices.Contains(ao.DryRun, metav1.DryRunAll) {
			return c.Apply(ctx, obj, opts...)
		}
		body, ok := obj.(client.Object)
		if !ok {
			return errors.New("dry-run apply of a typed apply configuration")
		}
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(body.GetObjectKind().GroupVersionKind())
		if err := c.Get(ctx, client.ObjectKeyFromObject(body), live); err != nil {
			return client.IgnoreNotFound(err)
		}
		live.SetResourceVersion("")
		scratch := fake.NewClientBuilder().WithScheme(scheme).WithObjects(live).Build()
		return scratch.Apply(ctx, obj, client.FieldOwner(ao.FieldManager), client.ForceOwnership)
	}
}

// History: a record keeps its object's cloud name once it has one, and
// when the object went away during a deletion.
func TestRecordsKeepExternalNameAndGoneAt(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	ctx := t.Context()
	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	find := func(kind string) model.ManagedResource {
		t.Helper()
		records, err := w.store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, w.row.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range records {
			if r.Kind == kind {
				return r
			}
		}
		t.Fatalf("no %s record", kind)
		return model.ManagedResource{}
	}
	if got := find("Cluster"); got.ExternalName != "prod" || got.GoneAt != nil {
		t.Errorf("EKS cluster record: external name %q, gone at %v; want the EKS name and not gone", got.ExternalName, got.GoneAt)
	}
	row := w.cluster(t)
	row.State = stateDeleting
	if err := w.store.Clusters().Update(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil { // deletes; Usages are still there
		t.Fatal(err)
	}
	if got := find("Cluster"); got.GoneAt == nil || got.ExternalName != "prod" {
		t.Errorf("deleted EKS cluster record: gone at %v, external name %q; want both kept", got.GoneAt, got.ExternalName)
	}
}

// While a replaced node group is retired the cluster stays pending: marked
// RUNNING, the loop would stop and leave the old group's record behind.
func TestReconcileStaysPendingWhileANodeGroupIsRetired(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	ctx := t.Context()
	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil { // applies everything
		t.Fatal(err)
	}
	old := &awseks.NodeGroup{ObjectMeta: metav1.ObjectMeta{Namespace: worldNS, Name: "prod-replacement",
		Labels: crossplane.ClusterLabels(worldTenant, w.row.ID)}}
	if err := w.kube.Create(ctx, old); err != nil {
		t.Fatal(err)
	}
	// A second node group, recorded after the first: its replacement.
	if err := w.store.ManagedResources().Put(ctx, []model.ManagedResource{{TenantID: worldTenant,
		OwnerKind: model.OwnerCluster, OwnerID: w.row.ID, APIVersion: "eks.aws.m.upbound.io/v1beta1", Kind: "NodeGroup",
		Namespace: worldNS, Name: old.Name}}); err != nil {
		t.Fatal(err)
	}
	records, err := w.store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, w.row.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Every object READY and reconciled.
	for _, r := range records {
		u := &unstructured.Unstructured{}
		u.SetAPIVersion(r.APIVersion)
		u.SetKind(r.Kind)
		if err := w.kube.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: r.Name}, u); err != nil {
			t.Fatal(err)
		}
		conds := []any{map[string]any{"type": "Ready", "status": "True"},
			map[string]any{"type": "Synced", "status": "True", "observedGeneration": u.GetGeneration()}}
		if err := unstructured.SetNestedSlice(u.Object, conds, "status", "conditions"); err != nil {
			t.Fatal(err)
		}
		if err := w.kube.Update(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	if got := w.cluster(t); got.State == stateRunning {
		t.Fatalf("cluster is %s with two node groups recorded, want it pending", got.State)
	}
}

// A create the server left half-way (it went away after applying the EKS
// Cluster, before the node group) is resumed by the loop: the first stage
// is applied again while EKS is still creating, the later stage (add-ons,
// access entries) only once it is ACTIVE, and the cluster reaches RUNNING.
func TestReconcileResumesAnInterruptedCreate(t *testing.T) {
	resumeAfter = 0
	t.Cleanup(func() { resumeAfter = 2 * time.Minute })
	w := newWorld(t, interceptor.Funcs{}, nil)
	ctx := t.Context()
	prefix := crossplaneaws.ClusterPrefix(worldTenant, w.row.ID, w.row.Name)
	eksKey := client.ObjectKey{Namespace: worldNS, Name: prefix}
	setEKS := func(status string) {
		t.Helper()
		eks := &awseks.Cluster{}
		if err := w.kube.Get(ctx, eksKey, eks); err != nil {
			t.Fatal(err)
		}
		eks.Status.AtProvider.Status = ptr.To(status)
		if err := w.kube.Update(ctx, eks); err != nil {
			t.Fatal(err)
		}
	}
	count := func(list client.ObjectList) int {
		t.Helper()
		if err := w.kube.List(ctx, list, client.InNamespace(worldNS), client.MatchingLabels{crossplane.LabelCluster: w.row.ID}); err != nil {
			t.Fatal(err)
		}
		items, _ := meta.ExtractList(list)
		return len(items)
	}
	// The interrupted create: a new cluster whose EKS cluster was applied
	// and is still creating; the node group is recorded (records are
	// written before the applies) but was never applied.
	setEKS("CREATING")
	row := w.cluster(t)
	row.State, row.Generation, row.AppliedGeneration = stateProvisioning, 1, 0
	if err := w.store.Clusters().Update(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := w.store.ManagedResources().Put(ctx, []model.ManagedResource{{TenantID: worldTenant, OwnerKind: model.OwnerCluster,
		OwnerID: w.row.ID, APIVersion: "eks.aws.m.upbound.io/v1beta1", Kind: "NodeGroup", Namespace: worldNS, Name: prefix + "-default"}}); err != nil {
		t.Fatal(err)
	}

	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if count(&awseks.NodeGroupList{}) != 1 || count(&awsiam.RoleList{}) == 0 {
		t.Fatalf("first stage not applied again: %d node groups, %d roles", count(&awseks.NodeGroupList{}), count(&awsiam.RoleList{}))
	}
	if n := count(&awseks.AddonList{}) + count(&awseks.AccessEntryList{}) + count(&awseks.PodIdentityAssociationList{}); n != 0 {
		t.Fatalf("%d later-stage objects applied while EKS is still creating", n)
	}
	if got := w.cluster(t); got.AppliedGeneration != 0 || got.State != stateProvisioning {
		t.Fatalf("applied generation %d, state %s: the create is not complete yet", got.AppliedGeneration, got.State)
	}
	// Nothing is missing any more: further passes leave the cluster alone
	// until EKS is ACTIVE.
	applies := 0
	w2 := w.svc.deps.Kube
	w.svc.deps.Kube = interceptor.NewClient(w2.(client.WithWatch), interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			applies++
			return c.Apply(ctx, obj, opts...)
		}})
	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil || applies != 0 {
		t.Fatalf("a complete first stage was applied again (%d applies, %v)", applies, err)
	}
	w.svc.deps.Kube = w2

	setEKS("ACTIVE")
	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	if got := w.cluster(t); count(&awseks.AddonList{}) == 0 || got.AppliedGeneration != got.Generation {
		t.Fatalf("after ACTIVE: %d add-ons, applied generation %d of %d", count(&awseks.AddonList{}), got.AppliedGeneration, got.Generation)
	}
	// Every object READY: the cluster is RUNNING.
	records, err := w.store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, w.row.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		u := &unstructured.Unstructured{}
		u.SetAPIVersion(r.APIVersion)
		u.SetKind(r.Kind)
		if err := w.kube.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: r.Name}, u); err != nil {
			t.Fatal(err)
		}
		conds := []any{map[string]any{"type": "Ready", "status": "True"},
			map[string]any{"type": "Synced", "status": "True", "observedGeneration": u.GetGeneration()}}
		if err := unstructured.SetNestedSlice(u.Object, conds, "status", "conditions"); err != nil {
			t.Fatal(err)
		}
		if err := w.kube.Update(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	if got := w.cluster(t); got.State != stateRunning {
		t.Errorf("state = %s, want %s", got.State, stateRunning)
	}
}

// What must not be resumed: a create another call is still working on, and
// a cluster past its first stage whose EKS cluster is updating.
func TestReconcileDoesNotResumeOtherClusters(t *testing.T) {
	for _, tt := range []struct {
		name       string
		grace      time.Duration
		state      string
		laterStage bool
	}{
		{"row written a moment ago", time.Hour, stateProvisioning, false},
		{"past the first stage", 0, stateProvisioning, true},
		{"an update", 0, stateUpdating, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resumeAfter = tt.grace
			t.Cleanup(func() { resumeAfter = 2 * time.Minute })
			w := newWorld(t, interceptor.Funcs{}, nil)
			ctx := t.Context()
			prefix := crossplaneaws.ClusterPrefix(worldTenant, w.row.ID, w.row.Name)
			eks := &awseks.Cluster{}
			if err := w.kube.Get(ctx, client.ObjectKey{Namespace: worldNS, Name: prefix}, eks); err != nil {
				t.Fatal(err)
			}
			eks.Status.AtProvider.Status = ptr.To("UPDATING")
			if err := w.kube.Update(ctx, eks); err != nil {
				t.Fatal(err)
			}
			row := w.cluster(t)
			row.State = tt.state
			if err := w.store.Clusters().Update(ctx, row); err != nil {
				t.Fatal(err)
			}
			missing := []model.ManagedResource{{TenantID: worldTenant, OwnerKind: model.OwnerCluster, OwnerID: w.row.ID,
				APIVersion: "eks.aws.m.upbound.io/v1beta1", Kind: "NodeGroup", Namespace: worldNS, Name: prefix + "-default"}}
			if tt.laterStage {
				missing = append(missing, model.ManagedResource{TenantID: worldTenant, OwnerKind: model.OwnerCluster, OwnerID: w.row.ID,
					APIVersion: "eks.aws.m.upbound.io/v1beta1", Kind: "Addon", Namespace: worldNS, Name: prefix + "-addon-coredns"})
			}
			if err := w.store.ManagedResources().Put(ctx, missing); err != nil {
				t.Fatal(err)
			}
			if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
				t.Fatal(err)
			}
			groups := &awseks.NodeGroupList{}
			if err := w.kube.List(ctx, groups, client.InNamespace(worldNS)); err != nil || len(groups.Items) != 0 {
				t.Errorf("%d node groups applied (%v), want none", len(groups.Items), err)
			}
		})
	}
}
