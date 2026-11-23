package account

import (
	"context"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/fault"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// kubeconfigReaderBinding lets the controller get Secrets in one tenant
// namespace: the kubeconfig Secrets of that tenant's workload clusters
// (RBAC cannot select Secrets by a name suffix). It binds the pre-created
// ClusterRole reconcile.KubeconfigReaderRole, which is bound nowhere else,
// so the controller reads no Secret outside tenant namespaces.
func kubeconfigReaderBinding(a *model.CloudAccount, sa reconcile.ServiceAccount) *rbacv1.RoleBinding {
	return tenantBinding(a, sa, reconcile.KubeconfigReaderRole)
}

// liveReaderBinding is the second binding of reconcile.KubeconfigReaderRole
// in the tenant namespace (PROP-02 7.1): the live reader's service account,
// next to the controller's, gets Secrets there. It reads only the clusters'
// "<prefix>-live-kubeconfig" Secrets (RBAC cannot narrow it to those).
func liveReaderBinding(a *model.CloudAccount, sa reconcile.ServiceAccount) *rbacv1.RoleBinding {
	b := tenantBinding(a, sa, reconcile.KubeconfigReaderRole)
	b.Name = reconcile.KubeconfigReaderRole + "-live"
	return b
}

// tenantBinding binds the controller to the pre-created ClusterRole role
// in the tenant namespace only, with a RoleBinding of the role's name.
func tenantBinding(a *model.CloudAccount, sa reconcile.ServiceAccount, role string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: a.Namespace, Name: role, Labels: crossplane.Labels(a.TenantID)},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Namespace: sa.Namespace, Name: sa.Name}},
	}
}

// bindKubeconfigReader applies the tenant namespace's binding. It belongs
// to the namespace, not to the account (a tenant's accounts share the
// namespace), so it is not recorded as the account's and is not deleted
// with it: it goes with the namespace. Without the controller's service
// account (a run outside the cluster) nothing is bound.
func (r *Reconciler) bindKubeconfigReader(ctx context.Context, a *model.CloudAccount) error {
	sa := r.deps.ControllerServiceAccount
	if sa == nil {
		return nil
	}
	// The kubeconfig reader (workload cluster scans) and the usage-history
	// writer (resource recommendations).
	bindings := []*rbacv1.RoleBinding{kubeconfigReaderBinding(a, *sa), tenantBinding(a, *sa, reconcile.UsageHistoryWriterRole),
		liveReaderBinding(a, reconcile.ServiceAccount{Namespace: sa.Namespace, Name: reconcile.LiveReaderServiceAccount})}
	for _, b := range bindings {
		if err := kube.Apply(ctx, r.deps.Kube, b); err != nil {
			r.logger().ErrorContext(ctx, "bind a tenant namespace role", "tenant_id", a.TenantID, "role", b.RoleRef.Name, "error", err)
			return fault.New(fault.Internal, "could not configure the management cluster")
		}
	}
	return nil
}
