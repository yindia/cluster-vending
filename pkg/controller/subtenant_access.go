package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/config"
	"github.com/yindia/cluster-vending/pkg/convert"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/naas"
)

// The SubTenant access conditions (PROP-05 6.9). NetworkPolicyReady: the
// parent cluster enforces NetworkPolicy (its vpc-cni add-on), without
// which the sub-tenant's isolation policies hold nothing.
const (
	conditionGroupsAllowed      = "GroupsAllowed"
	conditionNetworkPolicyReady = "NetworkPolicyReady"
)

// subTenantAccessGates are the access conditions in the order they pass.
var subTenantAccessGates = []string{conditionGroupsAllowed, "OIDCReady", conditionNetworkPolicyReady}

// subTenantRetiredConditions are conditions of the removed qualification
// gate; every pass drops them from the status.
var subTenantRetiredConditions = []string{"AccessQualified", "ClusterQualified"}

// subTenantAccessClosed are the access reasons of a provisioned sub-tenant
// whose named groups have no access. One that had access goes Degraded.
var subTenantAccessClosed = map[string]bool{
	"GroupNotAllowed": true, "OIDCPending": true, "OIDCAssociationPending": true,
	"AccessBindingMismatch": true, "TenantSpecMismatch": true,
}

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

// subTenantAccessPrerequisite is an access error that leaves provisioning
// alone: the Tenant is rendered without access, which removes the generated
// RoleBindings, and access is reported closed.
func subTenantAccessPrerequisite(err error) bool {
	return errors.Is(err, naas.ErrAccessOIDCNotConfigured) || errors.Is(err, naas.ErrGroupNotAllowed)
}

func renderSubTenantAccess(obj *cloud.SubTenant, parent *cloud.Cluster, revision string, groups config.NaaS) (*unstructured.Unstructured, *naas.ResolvedAccess, *cloudv1.ClusterSpec, error, error) {
	spec := convert.ClusterSpecFromAPI(parent.Spec, parent.Labels[cloudstore.LabelNetworkID], nil)
	access, accessErr := naas.ResolveAccess(obj.Spec.Access, spec, groups)
	if accessErr != nil && !subTenantAccessPrerequisite(accessErr) {
		return nil, nil, spec, accessErr, accessErr
	}
	if accessErr != nil {
		// Missing OIDC or a group outside naas.allowedGroups is an access
		// prerequisite, not a provisioning failure: no binding is rendered for
		// any group. The original management intent is retained unchanged.
		base := obj.DeepCopy()
		base.Spec.Access = nil
		desired, err := naas.RenderTenantWithAccess(base, parent.Status.CurrentKubernetesVersion, revision, spec, groups)
		return desired, &naas.ResolvedAccess{}, spec, accessErr, err
	}
	desired, err := naas.RenderTenantWithAccess(obj, parent.Status.CurrentKubernetesVersion, revision, spec, groups)
	return desired, access, spec, nil, err
}

// subTenantAccessInput is what one pass knows when it decides access.
type subTenantAccessInput struct {
	parent    *cloud.Cluster
	desired   *unstructured.Unstructured
	access    *naas.ResolvedAccess
	spec      *cloudv1.ClusterSpec
	accessErr error
}

// closeAccess reports access closed for reason on every access condition
// from GroupsAllowed on that has not passed yet.
func closeAccess(st *cloud.SubTenantStatus, generation int64, from, reason, message string) {
	for _, kind := range subTenantAccessGates[indexOf(subTenantAccessGates, from):] {
		subTenantAccessCondition(st, generation, kind, false, reason, message)
	}
	subTenantAccessCondition(st, generation, "AccessReady", false, reason, message)
}

func indexOf(list []string, item string) int {
	for i, v := range list {
		if v == item {
			return i
		}
	}
	return 0
}

// reconcileSubTenantAccess opens access only when every named group is
// allowed, the cluster's OIDC association is current and the Tenant is
// exactly the render (namespaces and baselines were checked by the caller).
func (r *SubTenantReconciler) reconcileSubTenantAccess(ctx context.Context, obj *cloud.SubTenant, st *cloud.SubTenantStatus, guard *subTenantAccessGuard, in subTenantAccessInput) (string, string) {
	g := obj.Generation
	switch {
	case errors.Is(in.accessErr, naas.ErrGroupNotAllowed):
		closeAccess(st, g, conditionGroupsAllowed, "GroupNotAllowed", in.accessErr.Error())
		return "GroupNotAllowed", "Namespace allocation provisioned; access is closed because a named group is not allowed by the platform: " + in.accessErr.Error()
	case in.accessErr != nil:
		subTenantAccessCondition(st, g, conditionGroupsAllowed, true, "GroupsAllowed", "Every named group is allowed by naas.allowedGroups")
		closeAccess(st, g, "OIDCReady", "OIDCPending", in.accessErr.Error())
		return "OIDCPending", "Namespace allocation provisioned; group access is waiting for EKS OIDC configuration"
	case len(in.access.Bindings) == 0:
		closeAccess(st, g, conditionGroupsAllowed, "NoGroupsRequested", "Namespace allocation is provisioned; no human access groups were requested")
		return "Provisioned", "Namespace allocation provisioned; no human access groups were requested"
	}
	subTenantAccessCondition(st, g, conditionGroupsAllowed, true, "GroupsAllowed", "Every named group is allowed by naas.allowedGroups")
	associationUID, err := naas.ObserveAccessOIDC(ctx, r.deps.Direct, in.parent, in.spec)
	if err != nil || associationUID == "" {
		message := "the current ACTIVE EKS OIDC association is not observed"
		if err != nil {
			message = err.Error()
		}
		closeAccess(st, g, "OIDCReady", "OIDCAssociationPending", message)
		return "OIDCAssociationPending", "Namespace allocation provisioned; waiting for the current ACTIVE EKS OIDC association"
	}
	subTenantAccessCondition(st, g, "OIDCReady", true, "OIDCAssociationObserved", "Ready, Synced, ACTIVE EKS association matches the current cluster and OIDC mapping")
	if reason, message, waiting := r.awaitNetworkPolicy(ctx, st, g, guard, in.parent); waiting {
		return reason, message
	}
	if !naas.EqualTenantSpec(guard.remote, in.desired) || guard.remote.GetAnnotations()[naas.SpecAnnotation] != in.desired.GetAnnotations()[naas.SpecAnnotation] {
		subTenantAccessCondition(st, g, "AccessReady", false, "TenantSpecMismatch", "The observed Capsule Tenant does not match its render")
		return "TenantSpecMismatch", "Namespace allocation provisioned; the observed Tenant does not match its render, so access stays closed"
	}
	bindingsErr := naas.ObserveAccessBindings(ctx, guard.workload, obj, guard.remote, in.access)
	if bindingsErr != nil && !errors.Is(bindingsErr, naas.ErrAccessBindingsPending) {
		subTenantAccessCondition(st, g, "AccessReady", false, "AccessBindingMismatch", bindingsErr.Error())
		return "AccessBindingMismatch", "Namespace allocation provisioned; unexpected namespace RBAC prevents access"
	}
	if guard.remote.GetLabels()[naas.LabelAccessReady] != "true" {
		next := guard.remote.DeepCopy()
		labels := maps.Clone(next.GetLabels())
		labels[naas.LabelAccessReady] = "true"
		next.SetLabels(labels)
		if err := guard.workload.Update(ctx, next); err != nil {
			subTenantAccessCondition(st, g, "AccessReady", false, "AccessGrantPending", "Access gate update was not confirmed")
			return "AccessGrantPending", "Namespace allocation provisioned; group access update is pending"
		}
		guard.remote = next
		bindingsErr = naas.ErrAccessBindingsPending
	}
	guard.keep = true
	if bindingsErr != nil {
		subTenantAccessCondition(st, g, "AccessReady", false, "AccessBindingsPending", bindingsErr.Error())
		return "AccessBindingsPending", "Namespace allocation provisioned; waiting for Capsule namespace access bindings"
	}
	subTenantAccessCondition(st, g, "AccessReady", true, "AccessBindingsObserved", "Namespace-only group bindings and namespace readiness were freshly observed")
	return "AccessReady", fmt.Sprintf("Namespace allocation provisioned; %d group bindings are ready in every allocated namespace", len(in.access.Bindings))
}

// awaitNetworkPolicy holds access closed until the parent cluster's vpc-cni
// add-on enforces NetworkPolicy, Ready and Synced (true: waiting). Access
// already open stays open (the gate is kept, AccessReady reports the wait):
// an add-on update, or an older render that has not gained enforcement
// yet, would otherwise pull every sub-tenant's access fleet-wide.
func (r *SubTenantReconciler) awaitNetworkPolicy(ctx context.Context, st *cloud.SubTenantStatus, g int64, guard *subTenantAccessGuard, parent *cloud.Cluster) (string, string, bool) {
	err := naas.ObserveNetworkPolicyEnforcement(ctx, r.deps.Direct, parent)
	if err == nil {
		subTenantAccessCondition(st, g, conditionNetworkPolicyReady, true, "NetworkPolicyEnforced", "The cluster's vpc-cni add-on is Ready and Synced and enforces NetworkPolicy")
		return "", "", false
	}
	guard.keep = guard.remote.GetLabels()[naas.LabelAccessReady] == "true"
	closeAccess(st, g, conditionNetworkPolicyReady, "NetworkPolicyPending", err.Error())
	return "NetworkPolicyPending", "Namespace allocation provisioned; access waits for NetworkPolicy enforcement on the cluster: " + err.Error(), true
}

// subTenantGroupsNamed: the sub-tenant asks for human access.
func subTenantGroupsNamed(obj *cloud.SubTenant) bool {
	return obj.Spec.Access != nil && len(obj.Spec.Access.AdminGroups)+len(obj.Spec.Access.ViewerGroups) > 0
}

// subTenantReadyState is the state of a provisioned sub-tenant after its
// access pass: Degraded when access had been open (or the sub-tenant was
// already Degraded) and is now closed for reason, else Ready.
func subTenantReadyState(previous cloud.SubTenantStatus, reason string) cloud.SubTenantState {
	if !subTenantAccessClosed[reason] {
		return cloud.SubTenantStateReady
	}
	if previous.State == cloud.SubTenantStateDegraded || meta.IsStatusConditionTrue(previous.Conditions, "AccessReady") {
		return cloud.SubTenantStateDegraded
	}
	return cloud.SubTenantStateReady
}
