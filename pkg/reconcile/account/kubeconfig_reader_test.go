package account

import (
	"reflect"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

func (w world) binding(t *testing.T) *rbacv1.RoleBinding {
	t.Helper()
	b := &rbacv1.RoleBinding{}
	err := w.kube.Get(t.Context(), client.ObjectKey{Namespace: namespace, Name: reconcile.KubeconfigReaderRole}, b)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// In the cluster, the tenant namespace binds the controller's service
// account to the get-only kubeconfig reader ClusterRole: the controller
// then reads Secrets there and in no other namespace.
func TestReconcileBindsTheKubeconfigReaderInTheTenantNamespace(t *testing.T) {
	w := newWorld(t)
	w.rec.deps.ControllerServiceAccount = &reconcile.ServiceAccount{Namespace: "cluster-vending-system", Name: "cluster-vending-controller-manager"}

	// Twice: every account reconcile applies it again.
	for range 2 {
		if err := w.rec.Reconcile(t.Context(), tenant, w.account.ID); err != nil {
			t.Fatal(err)
		}
	}

	b := w.binding(t)
	if b == nil {
		t.Fatal("no RoleBinding in the tenant namespace")
	}
	wantRef := rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-vending-kubeconfig-reader"}
	wantSubjects := []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: "cluster-vending-system", Name: "cluster-vending-controller-manager"}}
	if b.RoleRef != wantRef || !reflect.DeepEqual(b.Subjects, wantSubjects) {
		t.Errorf("binding = %+v to %+v, want %+v to %+v", b.Subjects, b.RoleRef, wantSubjects, wantRef)
	}
	if b.Labels[crossplane.LabelTenantID] != tenant {
		t.Errorf("binding labels = %v, want the tenant's", b.Labels)
	}
}

// The binding belongs to the namespace the tenant's accounts share: it is
// not one account's record, so deleting an account leaves it.
func TestKubeconfigReaderBindingIsNotTheAccounts(t *testing.T) {
	w := newWorld(t)
	w.rec.deps.ControllerServiceAccount = &reconcile.ServiceAccount{Namespace: "cluster-vending-system", Name: "controller"}
	if err := w.rec.Reconcile(t.Context(), tenant, w.account.ID); err != nil {
		t.Fatal(err)
	}

	records, err := w.store.ManagedResources().ListByOwner(t.Context(), model.OwnerCloudAccount, w.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range records {
		if rec.Kind == "RoleBinding" {
			t.Errorf("the binding is recorded as the account's: %+v", rec)
		}
	}
	if _, err := w.rec.Delete(t.Context(), w.account); err != nil {
		t.Fatal(err)
	}
	if w.binding(t) == nil {
		t.Error("deleting the account removed the namespace's binding")
	}
}

// Outside the cluster (no service account) nothing is bound.
func TestReconcileBindsNothingWithoutAServiceAccount(t *testing.T) {
	w := newWorld(t)

	if err := w.rec.Reconcile(t.Context(), tenant, w.account.ID); err != nil {
		t.Fatal(err)
	}

	if b := w.binding(t); b != nil {
		t.Errorf("binding %+v without a service account", b)
	}
}

// The tenant namespace also binds the controller to the usage-history
// writer: it keeps each cluster's recorded usage (resource
// recommendations) in a ConfigMap there, and writes ConfigMaps nowhere
// else.
func TestReconcileBindsTheUsageHistoryWriterInTheTenantNamespace(t *testing.T) {
	w := newWorld(t)
	w.rec.deps.ControllerServiceAccount = &reconcile.ServiceAccount{Namespace: "cluster-vending-system", Name: "cluster-vending-controller-manager"}

	if err := w.rec.Reconcile(t.Context(), tenant, w.account.ID); err != nil {
		t.Fatal(err)
	}

	b := &rbacv1.RoleBinding{}
	if err := w.kube.Get(t.Context(), client.ObjectKey{Namespace: namespace, Name: reconcile.UsageHistoryWriterRole}, b); err != nil {
		t.Fatalf("no usage-history RoleBinding: %v", err)
	}
	want := rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-vending-usage-history-writer"}
	if b.RoleRef != want || len(b.Subjects) != 1 || b.Subjects[0].Name != "cluster-vending-controller-manager" {
		t.Errorf("binding = %+v to %+v", b.Subjects, b.RoleRef)
	}
}
