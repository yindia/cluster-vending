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
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: a.Namespace, Name: reconcile.KubeconfigReaderRole,
			Labels: crossplane.Labels(a.TenantID)},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: reconcile.KubeconfigReaderRole},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Namespace: sa.Namespace, Name: sa.Name}},
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
	if err := kube.Apply(ctx, r.deps.Kube, kubeconfigReaderBinding(a, *sa)); err != nil {
		r.logger().ErrorContext(ctx, "bind the kubeconfig reader role", "tenant_id", a.TenantID, "error", err)
		return fault.New(fault.Internal, "could not configure the management cluster")
	}
	return nil
}
