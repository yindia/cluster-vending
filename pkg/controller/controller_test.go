package controller

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsv1beta1 "github.com/upbound/provider-aws/v2/apis/namespaced/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	platform "github.com/yindia/cluster-vending/pkg/reconcile"
	netreconcile "github.com/yindia/cluster-vending/pkg/reconcile/network"
	"github.com/yindia/cluster-vending/pkg/server/network"
)

const (
	tenant    = "acme"
	namespace = "tenant-acme"
)

// world is a management cluster (a fake API server) with the store on the
// platform's kinds, a tenant and one bootstrapped account.
type world struct {
	k       client.Client
	store   repository.Store
	deps    Deps
	account *model.CloudAccount
	// writes counts every change sent to the API server.
	writes *atomic.Int64
}

// counting counts the writes a client makes; applies of a cluster's
// subnet tags are let through unsent (the fake client round-trips them
// through the Go types and refuses what the API server takes).
func counting(writes *atomic.Int64) interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			writes.Add(1)
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			writes.Add(1)
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			writes.Add(1)
			return c.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			writes.Add(1)
			return c.Delete(ctx, obj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			writes.Add(1)
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			ao := &client.ApplyOptions{}
			ao.ApplyOptions(opts)
			if strings.HasPrefix(ao.FieldManager, kube.FieldOwner+"/cluster-") {
				return nil
			}
			return c.Apply(ctx, obj, opts...)
		},
	}
}

func newWorld(t *testing.T) world {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	writes := &atomic.Int64{}
	k := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&cloudv1alpha1.CloudAccount{}, &cloudv1alpha1.Network{}, &cloudv1alpha1.Cluster{},
			&awsec2.InternetGateway{}, &awseks.Cluster{}).
		WithInterceptorFuncs(counting(writes)).Build()
	store := cloudstore.New(k)
	ctx := t.Context()
	if err := store.Tenants().Create(ctx, &model.Tenant{ID: tenant}); err != nil {
		t.Fatal(err)
	}
	account := &model.CloudAccount{TenantID: tenant, Provider: model.ProviderAWS, AccountID: "444455556666",
		ProviderConfigName: "aws-0a1b2c3d", Namespace: namespace, Identity: "arn:aws:iam::444455556666:role/provisioner"}
	if err := store.Tenants().PutCloudAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	return world{k: k, store: store, account: account, writes: writes,
		deps: Deps{Direct: k, Deps: platform.Deps{Store: store, Kube: k}}}
}

func request(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKey{Namespace: namespace, Name: name}}
}

func (w world) must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// network reads the Network object name.
func (w world) network(t *testing.T, name string) *cloudv1alpha1.Network {
	t.Helper()
	obj := &cloudv1alpha1.Network{}
	w.must(t, w.k.Get(t.Context(), client.ObjectKey{Namespace: namespace, Name: name}, obj))
	return obj
}

// handMadeNetwork is a Network as someone writes it for kubectl: no
// labels, no annotations, the layout left to the defaults.
func handMadeNetwork(name, account string) *cloudv1alpha1.Network {
	return &cloudv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: cloudv1alpha1.NetworkSpec{AccountRef: cloudv1alpha1.LocalReference{Name: account}, Region: "us-east-1",
			Tags: cloudv1alpha1.Tags{"team": "platform"}, Layout: &cloudv1alpha1.NetworkLayout{}},
	}
}

// adoptedNetwork creates a hand-made network and reconciles it until its
// objects are applied; it returns the controller and the row ID.
func (w world) adoptedNetwork(t *testing.T, name string) (*NetworkReconciler, string) {
	t.Helper()
	ctx := t.Context()
	w.must(t, w.k.Create(ctx, handMadeNetwork(name, "aws-444455556666")))
	r := NewNetworkReconciler(w.deps)
	for range 2 { // adopted, then applied
		if _, err := r.Reconcile(ctx, request(name)); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	id := w.network(t, name).Labels[cloudstore.LabelID]
	if id == "" {
		t.Fatalf("network %s was not adopted: %+v", name, w.network(t, name).Status.Conditions)
	}
	return r, id
}

func (w world) count(t *testing.T, list client.ObjectList, label, id string) int {
	t.Helper()
	w.must(t, w.k.List(t.Context(), list, client.InNamespace(namespace), client.MatchingLabels{label: id}))
	return meta.LenList(list)
}

// A Network created with kubectl becomes the row the API would have
// stored (an ID, PROVISIONING, owned by its account), gets its finalizer,
// and is applied: its provider objects exist and its status says so. A
// change of one of those objects finds it.
func TestNetworkCreatedByHandIsAdoptedAndApplied(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	r, id := w.adoptedNetwork(t, "prod")
	obj := w.network(t, "prod")
	if obj.Labels[cloudstore.LabelTenant] != tenant || obj.Labels[cloudstore.LabelAccountID] != w.account.ID ||
		len(obj.OwnerReferences) != 1 || obj.OwnerReferences[0].Kind != "CloudAccount" ||
		!ptr.Deref(obj.OwnerReferences[0].Controller, false) || !ptr.Deref(obj.OwnerReferences[0].BlockOwnerDeletion, false) {
		t.Errorf("labels %v, owners %v", obj.Labels, obj.OwnerReferences)
	}
	if len(obj.Finalizers) != 1 || obj.Finalizers[0] != cloudv1alpha1.NetworkFinalizer {
		t.Errorf("finalizers %v", obj.Finalizers)
	}
	if obj.Spec.Layout == nil || len(obj.Spec.Layout.Subnets) != 0 {
		t.Errorf("the adoption rewrote the layout someone wrote: %+v", obj.Spec.Layout)
	}
	if w.count(t, &awsec2.VPCList{}, crossplane.LabelNetwork, id) != 1 || w.count(t, &awsec2.SubnetList{}, crossplane.LabelNetwork, id) == 0 {
		t.Fatal("the network's provider objects were not applied")
	}
	if obj.Status.Phase == "" || len(obj.Status.Inventory) == 0 || obj.Status.ObservedGeneration != obj.Generation {
		t.Errorf("status: phase %q, %d inventory entries, observed generation %d of %d", obj.Status.Phase,
			len(obj.Status.Inventory), obj.Status.ObservedGeneration, obj.Generation)
	}
	// The NAT gateways wait for the internet gateway: looked at again soon.
	res, err := r.Reconcile(ctx, request("prod"))
	if err != nil || res.RequeueAfter != applyingDelay {
		t.Errorf("a network that is not applied in full: requeue after %s (%v), want %s", res.RequeueAfter, err, applyingDelay)
	}
	vpcs := &awsec2.VPCList{}
	w.must(t, w.k.List(ctx, vpcs, client.MatchingLabels{crossplane.LabelNetwork: id}))
	for name, o := range map[string]*owners{"remembered": r.owners, "looked up": {}} {
		reqs := ownerRequests(o, w.k, crossplane.LabelNetwork, func() client.ObjectList { return &cloudv1alpha1.NetworkList{} })(ctx, &vpcs.Items[0])
		if len(reqs) != 1 || reqs[0] != request("prod") {
			t.Errorf("%s: the VPC's owner = %v, want the network", name, reqs)
		}
	}
	other := &awsec2.VPC{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "x", Labels: map[string]string{crossplane.LabelNetwork: "no-such-id"}}}
	if reqs := ownerRequests(r.owners, w.k, crossplane.LabelNetwork, func() client.ObjectList { return &cloudv1alpha1.NetworkList{} })(ctx, other); len(reqs) != 0 {
		t.Errorf("a provider object of no network woke %v", reqs)
	}
}

// What the API would have refused is a condition, not a row: an account
// that does not exist, an import, a namespace that is no tenant's.
func TestNetworkThatCannotBeAdoptedSaysWhy(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	importing := handMadeNetwork("imported", "aws-444455556666")
	importing.Spec.Layout = nil
	importing.Spec.ImportFrom = &cloudv1alpha1.NetworkImport{AWS: &cloudv1alpha1.AWSImportNetwork{VPCID: "vpc-0123456789abcdef0"}}
	elsewhere := handMadeNetwork("elsewhere", "aws-444455556666")
	elsewhere.Namespace = "default"
	w.must(t, w.k.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}))
	for name, tt := range map[string]struct {
		obj    *cloudv1alpha1.Network
		reason string
	}{
		"no such account": {handMadeNetwork("orphan", "aws-000000000000"), reasonWaiting},
		"an import":       {importing, reasonNotThroughAPI},
		"no tenant":       {elsewhere, reasonInvalid},
		"an unknown region": {func() *cloudv1alpha1.Network {
			n := handMadeNetwork("far", "aws-444455556666")
			n.Spec.Region = "mars-1"
			return n
		}(), reasonInvalid},
	} {
		w.must(t, w.k.Create(ctx, tt.obj))
		req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tt.obj)}
		res, err := NewNetworkReconciler(w.deps).Reconcile(ctx, req)
		if err != nil || res.RequeueAfter != failedDelay {
			t.Errorf("%s: %+v, %v; want another look in %s", name, res, err, failedDelay)
		}
		got := &cloudv1alpha1.Network{}
		w.must(t, w.k.Get(ctx, req.NamespacedName, got))
		cond := meta.FindStatusCondition(got.Status.Conditions, ConditionAdopted)
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != tt.reason || cond.Message == "" {
			t.Errorf("%s: condition %+v, want Adopted=False with reason %s", name, cond, tt.reason)
		}
		if got.Labels[cloudstore.LabelID] != "" || len(got.Finalizers) != 0 {
			t.Errorf("%s: it became a row anyway: labels %v, finalizers %v", name, got.Labels, got.Finalizers)
		}
	}
}

// Reconciling an object whose row and resources did not change writes
// nothing: the controller's own writes do not keep it busy. And the
// events it acts on are changes of the spec, labels, annotations and a
// deletion, not of the status.
func TestSettledObjectIsNotWritten(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	r, _ := w.adoptedNetwork(t, "prod")
	if _, err := r.Reconcile(ctx, request("prod")); err != nil {
		t.Fatal(err)
	}
	before := w.writes.Load()
	for range 2 {
		if _, err := r.Reconcile(ctx, request("prod")); err != nil {
			t.Fatal(err)
		}
	}
	if n := w.writes.Load() - before; n != 0 {
		t.Errorf("%d writes for an object that did not change", n)
	}
	old := w.network(t, "prod")
	changed := func(edit func(*cloudv1alpha1.Network)) bool {
		obj := old.DeepCopy()
		edit(obj)
		return rowChanged().Update(event.UpdateEvent{ObjectOld: old, ObjectNew: obj})
	}
	for name, tt := range map[string]struct {
		edit func(*cloudv1alpha1.Network)
		want bool
	}{
		"status only": {func(n *cloudv1alpha1.Network) { n.Status.Phase = "Ready"; n.ResourceVersion = "999" }, false},
		"spec":        {func(n *cloudv1alpha1.Network) { n.Generation++ }, true},
		"row annotation": {func(n *cloudv1alpha1.Network) {
			n.Annotations[cloudstore.AnnotationRow] = `{"etag":"x","state":"NETWORK_STATE_DELETING"}`
		}, true},
		"deletion": {func(n *cloudv1alpha1.Network) { n.DeletionTimestamp = ptr.To(metav1.Now()) }, true},
	} {
		if got := changed(tt.edit); got != tt.want {
			t.Errorf("%s: acted on = %t, want %t", name, got, tt.want)
		}
	}
}

// An edit of the spec on the object is applied: the row's generation
// moves on and the next pass renders the change.
func TestSpecEditIsApplied(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	r, id := w.adoptedNetwork(t, "prod")
	// The internet gateway is attached, the network applied in full.
	gateways := &awsec2.InternetGatewayList{}
	w.must(t, w.k.List(ctx, gateways, client.MatchingLabels{crossplane.LabelNetwork: id}))
	gateways.Items[0].SetConditions(xpv2.Available(), xpv2.ReconcileSuccess())
	w.must(t, w.k.Status().Update(ctx, &gateways.Items[0]))
	if _, err := r.Reconcile(ctx, request("prod")); err != nil {
		t.Fatal(err)
	}
	before, err := w.store.Networks().Get(ctx, tenant, id)
	if err != nil || before.AppliedGeneration != before.Generation {
		t.Fatalf("applied %d of %d (%v)", before.AppliedGeneration, before.Generation, err)
	}
	if w.count(t, &awsec2.FlowLogList{}, crossplane.LabelNetwork, id) != 0 {
		t.Fatal("flow logs before they were turned on")
	}

	// The fake API server does not count generations: the test does what
	// the API server does. First the generation the object was created
	// with, which the next status write records as seen.
	obj := w.network(t, "prod")
	obj.Generation = 1
	w.must(t, w.k.Update(ctx, obj))
	if _, err := r.Reconcile(ctx, request("prod")); err != nil {
		t.Fatal(err)
	}
	if got, _ := w.store.Networks().Get(ctx, tenant, id); got.Generation != before.Generation {
		t.Fatalf("a network nobody edited moved to generation %d", got.Generation)
	}
	obj = w.network(t, "prod")
	if obj.Status.ObservedGeneration != 1 {
		t.Fatalf("observed generation %d, want 1", obj.Status.ObservedGeneration)
	}
	// Then the edit.
	obj.Spec.FlowLogs = &cloudv1alpha1.NetworkFlowLogs{Enabled: ptr.To(true)}
	obj.Generation = 2
	w.must(t, w.k.Update(ctx, obj))
	if _, err := r.Reconcile(ctx, request("prod")); err != nil {
		t.Fatal(err)
	}
	after, err := w.store.Networks().Get(ctx, tenant, id)
	if err != nil || after.Generation != before.Generation+1 || after.AppliedGeneration != after.Generation {
		t.Fatalf("generation %d (was %d), applied %d (%v)", after.Generation, before.Generation, after.AppliedGeneration, err)
	}
	if w.count(t, &awsec2.FlowLogList{}, crossplane.LabelNetwork, id) != 1 {
		t.Error("the edit was not applied: no flow log")
	}
	if got := w.network(t, "prod"); edited(got.Generation, got.Status.ObservedGeneration) {
		t.Error("the edit still counts as not seen: it would be applied again and again")
	}
}

// A deleted Network keeps its finalizer while the reconciler deletes its
// cloud resources, and goes with its row.
func TestDeletedNetworkGoesWhenItsResourcesAreGone(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	r, id := w.adoptedNetwork(t, "prod")
	w.must(t, w.k.Delete(ctx, w.network(t, "prod")))
	if obj := w.network(t, "prod"); obj.DeletionTimestamp.IsZero() || len(obj.Finalizers) == 0 {
		t.Fatalf("after the delete: deletion %v, finalizers %v", obj.DeletionTimestamp, obj.Finalizers)
	}
	res, err := r.Reconcile(ctx, request("prod"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := w.store.Networks().Get(ctx, tenant, id)
	if err != nil || row.State != netreconcile.StateDeleting || res.RequeueAfter == 0 {
		t.Fatalf("after the first pass: state %s, requeue %s (%v); want DELETING and another look", row.State, res.RequeueAfter, err)
	}
	for range 10 { // tier by tier
		if _, err := r.Reconcile(ctx, request("prod")); err != nil {
			t.Fatal(err)
		}
	}
	if w.count(t, &awsec2.VPCList{}, crossplane.LabelNetwork, id) != 0 {
		t.Error("the VPC object is still there")
	}
	err = w.k.Get(ctx, request("prod").NamespacedName, &cloudv1alpha1.Network{})
	if err == nil || client.IgnoreNotFound(err) != nil {
		t.Errorf("the Network object after its resources are gone: %v, want it gone", err)
	}
}

// An owner is not deleted while it has dependents: a network with a
// cluster, an account with a network. Nothing of it is touched, and its
// status says what blocks the deletion.
func TestOwnerWithDependentsIsNotDeleted(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	r, id := w.adoptedNetwork(t, "prod")
	cluster := &cloudv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "apps"}}
	cluster.Spec.Network.NetworkRef.Name = "prod"
	w.must(t, w.k.Create(ctx, cluster))

	w.must(t, w.k.Delete(ctx, w.network(t, "prod")))
	res, err := r.Reconcile(ctx, request("prod"))
	if err != nil || res.RequeueAfter != failedDelay {
		t.Fatalf("%+v, %v", res, err)
	}
	obj := w.network(t, "prod")
	cond := meta.FindStatusCondition(obj.Status.Conditions, ConditionDeletionBlocked)
	if cond == nil || cond.Status != metav1.ConditionTrue || !strings.Contains(cond.Message, "apps") {
		t.Errorf("condition %+v, want DeletionBlocked naming the cluster", cond)
	}
	row, err := w.store.Networks().Get(ctx, tenant, id)
	if err != nil || row.State == netreconcile.StateDeleting || w.count(t, &awsec2.VPCList{}, crossplane.LabelNetwork, id) != 1 {
		t.Errorf("state %s (%v): the deletion started under a cluster", row.State, err)
	}

	accounts := NewCloudAccountReconciler(w.deps)
	account := &cloudv1alpha1.CloudAccount{}
	key := client.ObjectKey{Namespace: namespace, Name: "aws-444455556666"}
	if _, err := accounts.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	w.must(t, w.k.Get(ctx, key, account))
	w.must(t, w.k.Delete(ctx, account))
	if _, err := accounts.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	w.must(t, w.k.Get(ctx, key, account))
	cond = meta.FindStatusCondition(account.Status.Conditions, ConditionDeletionBlocked)
	if cond == nil || !strings.Contains(cond.Message, "prod") || len(account.Finalizers) == 0 {
		t.Errorf("account: condition %+v, finalizers %v; want the deletion blocked by the network", cond, account.Finalizers)
	}
	pc := &awsv1beta1.ProviderConfig{}
	if err := w.k.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "aws-0a1b2c3d"}, pc); err != nil {
		t.Errorf("the account's ProviderConfig under a network: %v", err)
	}
}

// An account bootstrapped through the API is kept in the management
// cluster (namespace, ProviderConfig under its stored name) and reports
// Ready; one created by hand is marked; a deleted one goes once its
// ProviderConfig is gone.
func TestCloudAccount(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	r := NewCloudAccountReconciler(w.deps)
	key := client.ObjectKey{Namespace: namespace, Name: "aws-444455556666"}
	req := ctrl.Request{NamespacedName: key}
	res, err := r.Reconcile(ctx, req)
	if err != nil || res.RequeueAfter != resyncDelay {
		t.Fatalf("%+v, %v", res, err)
	}
	obj := &cloudv1alpha1.CloudAccount{}
	w.must(t, w.k.Get(ctx, key, obj))
	cond := meta.FindStatusCondition(obj.Status.Conditions, ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionTrue || obj.Status.Phase != cloudv1alpha1.CloudAccountPhaseReady ||
		obj.Status.ProviderConfigName != "aws-0a1b2c3d" || len(obj.Finalizers) != 1 || len(obj.Status.Inventory) != 2 {
		t.Errorf("status %+v, finalizers %v", obj.Status, obj.Finalizers)
	}
	pcKey := client.ObjectKey{Namespace: namespace, Name: "aws-0a1b2c3d"}
	w.must(t, w.k.Get(ctx, pcKey, &awsv1beta1.ProviderConfig{}))
	before := w.writes.Load()
	if _, err := r.Reconcile(ctx, req); err != nil || w.writes.Load() != before {
		t.Errorf("a second pass made %d writes (%v)", w.writes.Load()-before, err)
	}

	byHand := &cloudv1alpha1.CloudAccount{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "mine"},
		Spec: cloudv1alpha1.CloudAccountSpec{AWS: &cloudv1alpha1.AWSAccount{AccountID: "999988887777"}}}
	w.must(t, w.k.Create(ctx, byHand))
	if _, err := r.Reconcile(ctx, request("mine")); err != nil {
		t.Fatal(err)
	}
	w.must(t, w.k.Get(ctx, client.ObjectKeyFromObject(byHand), byHand))
	if cond := meta.FindStatusCondition(byHand.Status.Conditions, ConditionAdopted); cond == nil || cond.Reason != reasonNotThroughAPI {
		t.Errorf("an account created by hand: condition %+v", cond)
	}

	w.must(t, w.k.Delete(ctx, obj))
	for range 3 {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.k.Get(ctx, pcKey, &awsv1beta1.ProviderConfig{}); err == nil {
		t.Error("the ProviderConfig of a deleted account is still there")
	}
	if err := w.k.Get(ctx, key, obj); err == nil || client.IgnoreNotFound(err) != nil {
		t.Errorf("the CloudAccount after its deletion: %v, want it gone", err)
	}
	if err := w.k.Get(ctx, client.ObjectKey{Name: namespace}, &corev1.Namespace{}); err != nil {
		t.Errorf("the tenant namespace went with one account: %v", err)
	}
}

// A network created and deleted through the API (the handlers write the
// object only) is applied and removed by the controller: its events are
// changes of the object's annotations, where the store keeps the row.
func TestNetworkWrittenByTheAPIIsReconciled(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	api := network.NewNetworkService(w.deps.Deps)
	created, err := api.CreateNetwork(ctx, &cloudv1.CreateNetworkRequest{TenantId: tenant, Network: &cloudv1.Network{
		Name: "prod", Spec: &cloudv1.NetworkSpec{Region: "us-east-1", BootstrapId: w.account.ID, Layout: &cloudv1.NetworkLayout{}}}})
	w.must(t, err)
	id := created.GetNetwork().GetId()
	if w.count(t, &awsec2.VPCList{}, crossplane.LabelNetwork, id) != 0 {
		t.Fatal("the API applied the network itself")
	}
	r := NewNetworkReconciler(w.deps)
	if _, err := r.Reconcile(ctx, request("prod")); err != nil {
		t.Fatal(err)
	}
	obj := w.network(t, "prod")
	if w.count(t, &awsec2.VPCList{}, crossplane.LabelNetwork, id) != 1 || len(obj.Finalizers) != 1 || obj.Status.Phase == "" {
		t.Fatalf("after a pass: finalizers %v, phase %q", obj.Finalizers, obj.Status.Phase)
	}
	if _, err := api.DeleteNetwork(ctx, &cloudv1.DeleteNetworkRequest{TenantId: tenant, NetworkId: id}); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if _, err := r.Reconcile(ctx, request("prod")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.k.Get(ctx, request("prod").NamespacedName, &cloudv1alpha1.Network{}); err == nil || client.IgnoreNotFound(err) != nil {
		t.Errorf("the Network object after a delete through the API: %v, want it gone", err)
	}
	if w.count(t, &awsec2.VPCList{}, crossplane.LabelNetwork, id) != 0 {
		t.Error("the VPC object is still there")
	}
}
