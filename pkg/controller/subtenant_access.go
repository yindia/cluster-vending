package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/convert"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/naas"
)

type subTenantAccessGuard struct {
	workload client.Client
	remote   *unstructured.Unstructured
	keep     bool
}

func (g *subTenantAccessGuard) revoke(ctx context.Context) error {
	if g.keep || g.remote == nil || g.remote.GetLabels()[naas.LabelAccessReady] == "false" {
		return nil
	}
	next := g.remote.DeepCopy()
	labels := maps.Clone(next.GetLabels())
	labels[naas.LabelAccessReady] = "false"
	next.SetLabels(labels)
	if err := g.workload.Update(ctx, next); err != nil {
		return err
	}
	g.remote = next
	return nil
}

func subTenantAccessCondition(st *cloud.SubTenantStatus, generation int64, kind string, value bool, reason, message string) {
	status := metav1.ConditionFalse
	if value {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: kind, Status: status, Reason: reason, Message: message, ObservedGeneration: generation})
}

func subTenantProvisioned(obj *cloud.SubTenant, remote *unstructured.Unstructured, inventory []cloud.SubTenantNamespaceStatus) bool {
	return naas.TenantProvisioningReady(obj, remote, inventory)
}

func renderSubTenantAccess(obj *cloud.SubTenant, parent *cloud.Cluster, revision string) (*unstructured.Unstructured, *naas.ResolvedAccess, *cloudv1.ClusterSpec, error, error) {
	spec := convert.ClusterSpecFromAPI(parent.Spec, parent.Labels[cloudstore.LabelNetworkID], nil)
	access, accessErr := naas.ResolveAccess(obj.Spec.Access, spec)
	if accessErr != nil && !errors.Is(accessErr, naas.ErrAccessOIDCNotConfigured) {
		return nil, nil, spec, accessErr, accessErr
	}
	if accessErr != nil {
		// Missing OIDC is an access prerequisite, not a provisioning failure.
		// The original management intent is retained unchanged.
		base := obj.DeepCopy()
		base.Spec.Access = nil
		desired, err := naas.RenderTenantWithAccess(base, parent.Status.CurrentKubernetesVersion, revision, spec)
		return desired, &naas.ResolvedAccess{}, spec, accessErr, err
	}
	desired, err := naas.RenderTenantWithAccess(obj, parent.Status.CurrentKubernetesVersion, revision, spec)
	return desired, access, spec, nil, err
}

// reconcileSubTenantAccess never equates static baseline observation with runtime
// qualification. Only the protected operator attestation can authorize opening.
func (r *SubTenantReconciler) reconcileSubTenantAccess(ctx context.Context, obj *cloud.SubTenant, parent *cloud.Cluster, st *cloud.SubTenantStatus, guard *subTenantAccessGuard, bootstrap *naas.Bootstrap, access *naas.ResolvedAccess, spec *cloudv1.ClusterSpec, accessErr error) (string, string, time.Time) {
	if accessErr != nil {
		subTenantAccessCondition(st, obj.Generation, "OIDCReady", false, "OIDCPending", accessErr.Error())
		subTenantAccessCondition(st, obj.Generation, "AccessQualified", false, "OIDCPending", "Current EKS OIDC mapping is required before qualification")
		subTenantAccessCondition(st, obj.Generation, "AccessReady", false, "OIDCPending", accessErr.Error())
		return "OIDCPending", "Namespace allocation provisioned; group access is waiting for EKS OIDC configuration", time.Time{}
	}
	if len(access.Bindings) == 0 {
		subTenantAccessCondition(st, obj.Generation, "OIDCReady", false, "NoGroupsRequested", "OIDC group access was not requested")
		subTenantAccessCondition(st, obj.Generation, "AccessQualified", false, "NoGroupsRequested", "Human access qualification was not requested")
		subTenantAccessCondition(st, obj.Generation, "AccessReady", false, "NoGroupsRequested", "Namespace allocation is provisioned; no human access groups were requested")
		return "Provisioned", "Namespace allocation provisioned; no human access groups were requested", time.Time{}
	}
	associationUID, err := naas.ObserveAccessOIDC(ctx, r.deps.Direct, parent, spec)
	if err != nil {
		subTenantAccessCondition(st, obj.Generation, "OIDCReady", false, "OIDCAssociationPending", err.Error())
		subTenantAccessCondition(st, obj.Generation, "AccessQualified", false, "OIDCAssociationPending", "Current observed EKS OIDC mapping is required before qualification")
		subTenantAccessCondition(st, obj.Generation, "AccessReady", false, "OIDCAssociationPending", err.Error())
		return "OIDCAssociationPending", "Namespace allocation provisioned; waiting for the current ACTIVE EKS OIDC association", time.Time{}
	}
	subTenantAccessCondition(st, obj.Generation, "OIDCReady", true, "OIDCAssociationObserved", "Ready, Synced, ACTIVE EKS association matches the current cluster and OIDC mapping")
	binding, err := naas.BindAccessQualification(obj, parent, guard.remote, st.Namespaces, bootstrap.Revision(), access, associationUID)
	var expires time.Time
	if err == nil {
		expires, err = naas.VerifyAccessQualification(ctx, guard.workload, binding, time.Now())
	}
	if err != nil {
		subTenantAccessCondition(st, obj.Generation, "AccessQualified", false, "QualificationRequired", err.Error())
		subTenantAccessCondition(st, obj.Generation, "AccessReady", false, "QualificationRequired", err.Error())
		return "QualificationRequired", "Namespace allocation provisioned; current trusted runtime qualification is required before group access", time.Time{}
	}
	subTenantAccessCondition(st, obj.Generation, "AccessQualified", true, "QualificationVerified", "Protected, unexpired operator qualification matches current identities and revisions")
	bindingsErr := naas.ObserveAccessBindings(ctx, guard.workload, obj, guard.remote, access)
	if bindingsErr != nil && !errors.Is(bindingsErr, naas.ErrAccessBindingsPending) {
		subTenantAccessCondition(st, obj.Generation, "AccessReady", false, "AccessBindingMismatch", bindingsErr.Error())
		return "AccessBindingMismatch", "Namespace allocation provisioned; unexpected namespace RBAC prevents access", time.Time{}
	}
	if guard.remote.GetLabels()[naas.LabelAccessReady] != "true" {
		next := guard.remote.DeepCopy()
		labels := maps.Clone(next.GetLabels())
		labels[naas.LabelAccessReady] = "true"
		next.SetLabels(labels)
		if err := guard.workload.Update(ctx, next); err != nil {
			subTenantAccessCondition(st, obj.Generation, "AccessReady", false, "AccessGrantPending", "Qualified access gate update was not confirmed")
			return "AccessGrantPending", "Namespace allocation provisioned; qualified group access update is pending", time.Time{}
		}
		guard.remote = next
		bindingsErr = naas.ErrAccessBindingsPending
	}
	guard.keep = true
	if bindingsErr != nil {
		subTenantAccessCondition(st, obj.Generation, "AccessReady", false, "AccessBindingsPending", bindingsErr.Error())
		return "AccessBindingsPending", "Namespace allocation provisioned and qualified; waiting for Capsule namespace access bindings", expires
	}
	subTenantAccessCondition(st, obj.Generation, "AccessReady", true, "AccessBindingsObserved", "Qualified namespace-only group bindings and namespace readiness were freshly observed")
	return "AccessReady", fmt.Sprintf("Namespace allocation provisioned; %d qualified group bindings are ready in every allocated namespace", len(access.Bindings)), expires
}
