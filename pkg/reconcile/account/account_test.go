package account

import (
	"context"
	"errors"
	"reflect"
	"testing"

	awsv1beta1 "github.com/upbound/provider-aws/v2/apis/namespaced/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/database/storetest"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

const (
	tenant    = "acme"
	namespace = "tenant-acme"
	identity  = "arn:aws:iam::444455556666:role/cluster-vending-provisioner"
)

// world is a Reconciler on a store (storetest.NewStore) and a fake management
// cluster, with one stored account of tenant acme.
type world struct {
	rec     *Reconciler
	store   repository.Store
	kube    client.Client
	account *model.CloudAccount
}

func newWorld(t *testing.T, funcs ...interceptor.Funcs) world {
	t.Helper()
	ctx := t.Context()
	store := storetest.NewStore(t)
	if err := store.Tenants().Create(ctx, &model.Tenant{ID: tenant}); err != nil {
		t.Fatal(err)
	}
	err := store.Tenants().PutCloudAccount(ctx, &model.CloudAccount{TenantID: tenant, Provider: model.ProviderAWS,
		AccountID: "444455556666", AccountName: "Acme production", Namespace: namespace, Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
	account, err := store.Tenants().GetCloudAccount(ctx, tenant, model.ProviderAWS, "444455556666")
	if err != nil {
		t.Fatal(err)
	}
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	b := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace, Labels: map[string]string{"cloud.cluster-vending.io/tenant": tenant, crossplane.LabelTenantID: tenant}}})
	for _, f := range funcs {
		b = b.WithInterceptorFuncs(f)
	}
	k := b.Build()
	return world{rec: New(reconcile.Deps{Store: store, Kube: k}), store: store, kube: k, account: account}
}

// have is the account's namespace and ProviderConfig as the management
// cluster has them (nil when missing), and the account's records.
func (w world) have(t *testing.T) (*corev1.Namespace, *awsv1beta1.ProviderConfig, []model.ManagedResource) {
	t.Helper()
	ctx := t.Context()
	ns := &corev1.Namespace{}
	if err := w.kube.Get(ctx, client.ObjectKey{Name: namespace}, ns); client.IgnoreNotFound(err) != nil {
		t.Fatal(err)
	} else if err != nil {
		ns = nil
	}
	pc := &awsv1beta1.ProviderConfig{}
	key := client.ObjectKey{Namespace: namespace, Name: w.account.ProviderConfigName}
	if err := w.kube.Get(ctx, key, pc); client.IgnoreNotFound(err) != nil {
		t.Fatal(err)
	} else if err != nil {
		pc = nil
	}
	records, err := w.store.ManagedResources().ListByOwner(ctx, model.OwnerCloudAccount, w.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	return ns, pc, records
}

// Reconcile applies the tenant namespace, then the ProviderConfig that
// assumes the tenant role, and records both as the account's.
func TestReconcileBootstrapsTheAccount(t *testing.T) {
	w := newWorld(t)
	if err := w.rec.Reconcile(t.Context(), tenant, w.account.ID); err != nil {
		t.Fatal(err)
	}
	ns, pc, records := w.have(t)
	if ns == nil || ns.Labels[crossplane.LabelTenantID] != tenant {
		t.Fatalf("namespace = %+v, want it labelled with the tenant", ns)
	}
	if pc == nil || len(pc.Spec.AssumeRoleChain) != 1 || *pc.Spec.AssumeRoleChain[0].RoleARN != identity {
		t.Fatalf("ProviderConfig = %+v, want it to assume %s", pc, identity)
	}
	if len(records) != 2 || records[0].Kind != "ProviderConfig" || records[0].Name != w.account.ProviderConfigName || records[0].CloudAccountID != w.account.ID || records[0].TenantID != tenant {
		t.Errorf("records = %+v, want the ProviderConfig and the live one", records)
	}
	// The second ProviderConfig assumes the reader role, for the live
	// reader's ClusterAuths (PROP-02 7.1).
	live := &awsv1beta1.ProviderConfig{}
	if err := w.kube.Get(t.Context(), client.ObjectKey{Namespace: pc.Namespace, Name: w.account.ProviderConfigName + "-live"}, live); err != nil {
		t.Fatal(err)
	}
	if len(records) == 2 && records[1].Name != live.Name {
		t.Errorf("records = %+v, want the live ProviderConfig recorded", records)
	}
	if len(live.Spec.AssumeRoleChain) != 1 || *live.Spec.AssumeRoleChain[0].RoleARN != "arn:aws:iam::444455556666:role/cluster-vending-reader" {
		t.Errorf("live ProviderConfig = %+v, want it to assume the reader role", live.Spec)
	}

}

// Reconciling an account again changes nothing: not its objects, not its
// records.
func TestReconcileTwiceChangesNothing(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	if err := w.rec.Reconcile(ctx, tenant, w.account.ID); err != nil {
		t.Fatal(err)
	}
	ns, pc, records := w.have(t)
	if err := w.rec.Reconcile(ctx, tenant, w.account.ID); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	ns2, pc2, records2 := w.have(t)
	if !reflect.DeepEqual(ns.Labels, ns2.Labels) || !reflect.DeepEqual(pc.Spec, pc2.Spec) ||
		!reflect.DeepEqual(pc.Labels, pc2.Labels) {
		t.Error("the second Reconcile changed the namespace or the ProviderConfig")
	}
	if !reflect.DeepEqual(records, records2) {
		t.Errorf("records after the second Reconcile = %+v, were %+v", records2, records)
	}
	configs := &awsv1beta1.ProviderConfigList{}
	if err := w.kube.List(ctx, configs); err != nil || len(configs.Items) != 2 {
		t.Errorf("%d ProviderConfigs (%v), want 2 (the account's and the live one)", len(configs.Items), err)
	}
}

func TestReconcileUnknownAccount(t *testing.T) {
	w := newWorld(t)
	err := w.rec.Reconcile(t.Context(), tenant, "0190f1a2-0000-7000-8000-00000000dead")
	if !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("err = %v, want not found", err)
	}
}

// Without the AWS provider the ProviderConfig kind does not exist: that is
// a precondition the platform has to meet, not an internal error.
func TestApplyWithoutAWSProvider(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if u, ok := obj.(interface{ GetKind() string }); ok && u.GetKind() == "ProviderConfig" {
				return &meta.NoKindMatchError{GroupKind: awsv1beta1.SchemeGroupVersion.WithKind("ProviderConfig").GroupKind()}
			}
			return c.Apply(ctx, obj, opts...)
		},
	})
	err := w.rec.Apply(t.Context(), w.account)
	if !fault.Is(err, fault.FailedPrecondition) {
		t.Fatalf("err = %v, want a FailedPrecondition fault", err)
	}
	if _, _, records := w.have(t); len(records) != 0 {
		t.Errorf("records = %+v, want none: nothing is recorded for an apply that failed", records)
	}
}

func TestApplyFailureIsInternalWithoutDetails(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{
		Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
			return errors.New("etcd: leader changed, token=secret")
		},
	})
	err := w.rec.Apply(t.Context(), w.account)
	if f, ok := fault.As(err); !ok || f.Kind != fault.Internal || f.Message != "could not configure the management cluster" {
		t.Errorf("err = %v, want an Internal fault that does not repeat the cause", err)
	}
}

// View is the stored account with the live state of every object under it.
func TestView(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	before, err := w.rec.View(ctx, w.account)
	if err != nil || len(before.GetStatus().GetManagedResources()) != 0 {
		t.Fatalf("view before bootstrap = %v (%v), want no objects", before.GetStatus().GetManagedResources(), err)
	}
	if err := w.rec.Apply(ctx, w.account); err != nil {
		t.Fatal(err)
	}
	got, err := w.rec.View(ctx, w.account)
	if err != nil {
		t.Fatal(err)
	}
	st := got.GetStatus()
	if got.GetId() != w.account.ID || got.GetAws().GetRole() != "cluster-vending-provisioner" ||
		st.GetNamespace() != namespace || st.GetProviderConfigName() != w.account.ProviderConfigName ||
		st.GetIdentity() != identity || st.GetState() != cloudv1.CloudAccountState_CLOUD_ACCOUNT_STATE_READY {
		t.Errorf("view = %v", got)
	}
	views := st.GetManagedResources()
	if len(views) != 2 || views[0].GetKind() != "ProviderConfig" || views[1].GetKind() != "ProviderConfig" ||
		views[0].GetState() != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY {
		t.Errorf("objects = %v, want the two ProviderConfigs, ready", views)
	}
}

// Delete removes the ProviderConfig and forgets the account's records;
// the namespace stays, other accounts of the tenant live in it. It is
// refused while a network of the account still has objects.
func TestDelete(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	if err := w.rec.Apply(ctx, w.account); err != nil {
		t.Fatal(err)
	}
	network := &model.Network{TenantID: tenant, Name: "prod", Provider: model.ProviderAWS, Region: "us-east-1",
		CloudAccountID: w.account.ID, Resource: model.Resource{State: "NETWORK_STATE_READY", Spec: storetest.Spec("network")}}
	if err := w.store.Networks().Create(ctx, network); err != nil {
		t.Fatal(err)
	}
	inUse := []model.ManagedResource{{TenantID: tenant, CloudAccountID: w.account.ID, OwnerKind: model.OwnerNetwork,
		OwnerID: network.ID, APIVersion: "ec2.aws.m.upbound.io/v1beta1", Kind: "VPC",
		Namespace: namespace, Name: "prod-vpc"}}
	if err := w.store.ManagedResources().Put(ctx, inUse); err != nil {
		t.Fatal(err)
	}
	if done, err := w.rec.Delete(ctx, w.account); done || !fault.Is(err, fault.FailedPrecondition) {
		t.Fatalf("Delete with a network's VPC = %t, %v; want a FailedPrecondition fault", done, err)
	}
	if _, pc, _ := w.have(t); pc == nil {
		t.Fatal("the ProviderConfig was deleted under a network that uses it")
	}
	if err := w.store.ManagedResources().DeleteByOwner(ctx, model.OwnerNetwork, inUse[0].OwnerID); err != nil {
		t.Fatal(err)
	}

	done, err := w.rec.Delete(ctx, w.account)
	if err != nil || !done {
		t.Fatalf("Delete = %t, %v; want done", done, err)
	}
	ns, pc, records := w.have(t)
	if pc != nil || ns == nil || len(records) != 0 {
		t.Errorf("after Delete: ProviderConfig %v, namespace %v, %d records; want only the namespace", pc != nil, ns != nil, len(records))
	}
	if done, err := w.rec.Delete(ctx, w.account); err != nil || !done {
		t.Errorf("Delete again = %t, %v; want done", done, err)
	}
}

func TestRoleNameFromIdentity(t *testing.T) {
	for identity, want := range map[string]string{
		identity: "cluster-vending-provisioner",
		"":       "", // a row from before identities were stored
	} {
		if got := RoleName(identity); got != want {
			t.Errorf("RoleName(%q) = %q, want %q", identity, got, want)
		}
	}
}

func TestApplyNeverCreatesMissingTenantNamespace(t *testing.T) {
	w := newWorld(t)
	if err := w.kube.Delete(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
		t.Fatal(err)
	}
	err := w.rec.Apply(t.Context(), w.account)
	if !fault.Is(err, fault.FailedPrecondition) {
		t.Fatalf("apply: %v", err)
	}
	ns, pc, records := w.have(t)
	if ns != nil || pc != nil || len(records) != 0 {
		t.Fatalf("missing namespace caused writes: %v %v %v", ns, pc, records)
	}
}

func TestApplyRejectsUnavailableNamespaceOwnership(t *testing.T) {
	for _, state := range []string{"owner reference", "deleting", "terminating"} {
		t.Run(state, func(t *testing.T) {
			w := newWorld(t, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if err := c.Get(ctx, key, obj, opts...); err != nil {
						return err
					}
					if ns, ok := obj.(*corev1.Namespace); ok {
						switch state {
						case "owner reference":
							ns.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "foreign-owner", UID: "foreign-owner"}}
						case "deleting":
							now := metav1.Now()
							ns.DeletionTimestamp = &now
						case "terminating":
							ns.Status.Phase = corev1.NamespaceTerminating
						}
					}
					return nil
				},
				Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
					t.Fatal("account applied resources despite invalid namespace")
					return nil
				},
			})
			err := w.rec.Apply(t.Context(), w.account)
			if !fault.Is(err, fault.FailedPrecondition) {
				t.Fatalf("apply: %v", err)
			}
			_, pc, records := w.have(t)
			if pc != nil || len(records) != 0 {
				t.Fatalf("invalid namespace caused writes: %v %v", pc, records)
			}
		})
	}
}
