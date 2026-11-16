package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/naas"
	platform "github.com/yindia/cluster-vending/pkg/reconcile"
	tenantlife "github.com/yindia/cluster-vending/pkg/reconcile/tenant"
)

// SubTenantReconciler provisions only its owned Capsule Tenant. Shared Capsule
// resources own namespace/policy generation; guarded offboarding may delete an
// owned empty Namespace, but never directly deletes child resources.
type SubTenantReconciler struct{ deps Deps }

func NewSubTenantReconciler(d Deps) *SubTenantReconciler { return &SubTenantReconciler{deps: d} }

// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=subtenants,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=subtenants/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=subtenants/finalizers,verbs=update
// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=tenants;clusters;releaseholds,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

func (r *SubTenantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	leave := r.deps.FreezeBarrier.Enter()
	defer leave()
	obj := &cloud.SubTenant{}
	if err := r.deps.Direct.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	st := obj.DeepCopy().Status
	if !obj.DeletionTimestamp.IsZero() && naas.DeletionProtectionEnabled(obj.Spec.DeletionProtection) {
		return r.subTenantReport(ctx, obj, st, cloud.SubTenantStateDeleting, "DeletionProtected", "Deletion protection is enabled; disable it explicitly before offboarding. No workload changes were attempted", false)
	}
	parent, err := r.subTenantParent(ctx, obj)
	if err != nil {
		return r.subTenantReport(ctx, obj, st, cloud.SubTenantStatePending, "ParentBindingUnavailable", err.Error(), true)
	}
	d := r.deps.Deps
	d.Kube = r.deps.Direct
	d.KubeReader = r.deps.Direct
	frozen, err := d.SyncFreeze(ctx, platform.PauseScope{TenantID: obj.Spec.TenantRef.Name, ClusterID: obj.Spec.ClusterRef.ID, NetworkID: parent.Labels[cloudstore.LabelNetworkID]})
	if err != nil {
		return r.subTenantReport(ctx, obj, st, cloud.SubTenantStatePending, "FreezeObservationUnavailable", "Cannot verify effective freeze policy; no workload writes are allowed", true)
	}
	if frozen.Holds() || managed.IsPaused(parent) || managed.IsPaused(obj) {
		message := "Reconciliation is manually paused; no workload writes are allowed"
		if frozen.Holds() {
			message = fmt.Sprintf("%s is %s; no workload writes are allowed", frozen.Scope, platform.FrozenBy(frozen))
		}
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: platform.ConditionFrozen, Status: metav1.ConditionTrue, Reason: "ReconciliationPaused", Message: message, ObservedGeneration: obj.Generation})
		result, err := r.subTenantReport(ctx, obj, st, cloud.SubTenantStatePending, "ReconciliationPaused", message, false)
		return untilFreezeEnds(frozen, result, time.Now()), err
	}
	meta.RemoveStatusCondition(&st.Conditions, platform.ConditionFrozen)
	if obj.DeletionTimestamp.IsZero() {
		if reason, message, failed := subTenantPrerequisite(parent); reason != "" {
			return r.subTenantReport(ctx, obj, st, cloud.SubTenantStatePending, reason, message, failed)
		}
	}
	if r.deps.SubTenantWorkloads == nil || parent.Status.KubeconfigSecret == "" {
		return r.subTenantReport(ctx, obj, st, cloud.SubTenantStatePending, "WorkloadClientPending", "Waiting for the parent kubeconfig and configured workload client; no workload writes have been attempted", false)
	}
	workload, err := r.deps.SubTenantWorkloads.Client(ctx, parent.Namespace, parent.Status.KubeconfigSecret)
	if err != nil || workload == nil {
		return r.subTenantReport(ctx, obj, st, cloud.SubTenantStatePending, "WorkloadClientUnavailable", "Cannot connect to the parent workload cluster; credentials are not exposed in status", true)
	}
	name, err := naas.RemoteTenantName(obj)
	if err != nil {
		return r.subTenantReport(ctx, obj, st, cloud.SubTenantStatePending, "InvalidAllocation", err.Error(), true)
	}
	remote := &unstructured.Unstructured{}
	remote.SetGroupVersionKind(naas.TenantGVK)
	if err := workload.Get(ctx, client.ObjectKey{Name: name}, remote); apierrors.IsNotFound(err) {
		remote = nil
	} else if err != nil {
		return r.subTenantReport(ctx, obj, st, cloud.SubTenantStatePending, "CapsuleAPIUnavailable", "Cannot read Capsule Tenant API; install the supported Capsule bootstrap and restore workload connectivity", true)
	}
	if remote != nil && !naas.OwnTenant(remote, obj) {
		return r.subTenantReport(ctx, obj, st, cloud.SubTenantStatePending, "RemoteOwnershipMismatch", "A Capsule Tenant with this name has foreign ownership or a different recorded UID; adoption and writes are refused", true)
	}
	if !obj.DeletionTimestamp.IsZero() {
		return r.deleteSubTenant(ctx, obj, st, workload, remote)
	}
	// An already-owned remote object always requires management protection before
	// revoking access or repairing it, even if other prerequisites are now missing.
	if remote != nil && !controllerutil.ContainsFinalizer(obj, cloud.SubTenantFinalizer) {
		return r.protectSubTenant(ctx, obj, st)
	}
	if remote != nil && remote.GetLabels()[naas.LabelAccessReady] != "false" {
		next := remote.DeepCopy()
		labels := maps.Clone(remote.GetLabels())
		labels[naas.LabelAccessReady] = "false"
		next.SetLabels(labels)
		if err := workload.Update(ctx, next); err != nil {
			return r.subTenantReport(ctx, obj, st, cloud.SubTenantStatePending, "AccessRevocationPending", "Could not close the owned Tenant access gate; no new configuration was applied", true)
		}
		return r.subTenantReport(ctx, obj, st, cloud.SubTenantStateProvisioning, "AccessClosed", "Developer access gate was closed; current templates and isolation qualification must be observed before access can be considered", false)
	}
	bootstrap, err := naas.VerifyTenantBootstrap(ctx, workload)
	if err != nil {
		st.TemplateRevision = ""
		message := fmt.Sprintf("Waiting for the Capsule bootstrap installation on cluster %s: %s. Provisioning will resume automatically when prerequisites are ready", parent.Name, err)
		warning := false
		// Failed components can hold back the release that delivers bootstrap.
		// They do not gate allocations whose bootstrap is already installed.
		for _, component := range parent.Status.PlatformComponents {
			if strings.EqualFold(component.Phase, "Failed") {
				message += fmt.Sprintf("; platform release may be blocked by %s: %s", component.Name, component.Message)
				warning = true
			}
		}
		return r.subTenantReport(ctx, obj, st, cloud.SubTenantStatePending, "BootstrapPending", message, warning)
	}
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: "PrerequisitesReady", Status: metav1.ConditionTrue, Reason: "PrerequisitesObserved", Message: "Capsule and shared bootstrap prerequisites are ready; workload API access is available", ObservedGeneration: obj.Generation})
	desired, err := naas.RenderTenant(obj, parent.Status.CurrentKubernetesVersion, bootstrap.Revision())
	if err != nil {
		return r.subTenantReport(ctx, obj, st, cloud.SubTenantStatePending, "AllocationValidationFailed", err.Error(), true)
	}
	if !controllerutil.ContainsFinalizer(obj, cloud.SubTenantFinalizer) {
		return r.protectSubTenant(ctx, obj, st)
	}
	if remote == nil {
		if obj.Status.RemoteCapsuleTenantUID != "" {
			return r.subTenantReport(ctx, obj, st, cloud.SubTenantStatePending, "RemoteIdentityMissing", "Previously observed Capsule Tenant is missing; review its identity and namespaces before recreating it", true)
		}
		if err := workload.Create(ctx, desired); err != nil {
			return r.subTenantReport(ctx, obj, st, cloud.SubTenantStatePending, "RemoteCreatePending", "Capsule Tenant creation was not confirmed; it will be read again before any retry or adoption", true)
		}
		remote = desired
	} else {
		next := naas.MergeTenant(remote, desired)
		if !naas.EqualTenantSpec(remote, next) || !maps.Equal(remote.GetLabels(), next.GetLabels()) || !maps.Equal(remote.GetAnnotations(), next.GetAnnotations()) {
			if err := workload.Update(ctx, next); err != nil {
				return r.subTenantReport(ctx, obj, st, cloud.SubTenantStateProvisioning, "RemoteUpdatePending", "Owned Capsule Tenant update was not confirmed; current state will be read again", true)
			}
			remote = next
		}
	}
	st.RemoteCapsuleTenant = name
	st.RemoteCapsuleTenantUID = string(remote.GetUID())
	st.TemplateRevision = bootstrap.Revision()
	st.ObservedGeneration = obj.Generation
	inventory, _, err := naas.ObserveTenantNamespaces(ctx, workload, remote, obj, bootstrap)
	if err != nil {
		return r.subTenantReport(ctx, obj, st, cloud.SubTenantStateProvisioning, "NamespaceObservationUnavailable", "Cannot read generated namespaces; last observations are retained and access remains closed", true)
	}
	st.Namespaces = inventory
	ready := 0
	for _, ns := range inventory {
		if meta.IsStatusConditionTrue(ns.Conditions, "BaselineReady") {
			ready++
		}
	}
	reason, message := "BaselinePending", fmt.Sprintf("Capsule Tenant observed; %d/%d namespace baselines match current templates. Access remains closed and runtime isolation qualification is required", ready, len(obj.Spec.NamespaceKeys))
	if ready == len(obj.Spec.NamespaceKeys) {
		reason = "QualificationRequired"
	}
	return r.subTenantReport(ctx, obj, st, cloud.SubTenantStateProvisioning, reason, message, false)
}

func (r *SubTenantReconciler) subTenantParent(ctx context.Context, obj *cloud.SubTenant) (*cloud.Cluster, error) {
	ref := obj.Spec.TenantRef
	if !tenantlife.ValidID(ref.Name) || obj.Namespace != tenantlife.NamespaceFor(ref.Name) || obj.UID == "" {
		return nil, fmt.Errorf("SubTenant namespace, allocation UID and tenant registration must be valid")
	}
	tenant := &cloud.Tenant{}
	if err := r.deps.Direct.Get(ctx, client.ObjectKey{Name: ref.Name}, tenant); err != nil {
		return nil, fmt.Errorf("tenant registration is unavailable")
	}
	if string(tenant.UID) != ref.UID || ref.UID == "" || (!tenant.DeletionTimestamp.IsZero() && obj.DeletionTimestamp.IsZero()) {
		return nil, fmt.Errorf("tenant registration UID changed or the registration is deleting")
	}
	namespace := &corev1.Namespace{}
	if err := r.deps.Direct.Get(ctx, client.ObjectKey{Name: obj.Namespace}, namespace); err != nil {
		return nil, fmt.Errorf("owning tenant namespace is unavailable")
	}
	if namespace.Labels[cloudstore.LabelTenant] != ref.Name || namespace.Annotations[tenantlife.AnnotationRegistration] != ref.UID || (!namespace.DeletionTimestamp.IsZero() && obj.DeletionTimestamp.IsZero()) {
		return nil, fmt.Errorf("tenant namespace ownership changed or it is deleting")
	}
	binding := obj.Spec.ClusterRef
	if parsed, err := uuid.Parse(binding.ID); err != nil || parsed.String() != binding.ID {
		return nil, fmt.Errorf("parent cluster UUID is invalid")
	}
	parent := &cloud.Cluster{}
	if err := r.deps.Direct.Get(ctx, client.ObjectKey{Namespace: obj.Namespace, Name: binding.Name}, parent); err != nil {
		return nil, fmt.Errorf("parent cluster is unavailable")
	}
	if parent.Labels[cloudstore.LabelTenant] != ref.Name || parent.Labels[cloudstore.LabelID] != binding.ID || string(parent.UID) != binding.UID || binding.UID == "" {
		return nil, fmt.Errorf("parent cluster UUID, name or immutable UID does not match this tenant")
	}
	var metadata struct {
		State string `json:"state"`
	}
	if raw := parent.Annotations[cloudstore.AnnotationRow]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
			return nil, fmt.Errorf("parent lifecycle metadata cannot be verified")
		}
	}
	if obj.DeletionTimestamp.IsZero() && (!parent.DeletionTimestamp.IsZero() || parent.Status.Phase == cloud.ClusterPhaseDeleting || metadata.State == "CLUSTER_STATE_DELETING") {
		return nil, fmt.Errorf("parent cluster is deleting; workload writes are blocked")
	}
	if !controllerutil.ContainsFinalizer(parent, cloud.ClusterFinalizer) {
		return nil, fmt.Errorf("parent cluster lifecycle protection has not been established")
	}
	if obj.DeletionTimestamp.IsZero() && (parent.Spec.NaaS == nil || !*parent.Spec.NaaS) {
		return nil, fmt.Errorf("parent cluster has NaaS disabled; workload writes are blocked")
	}
	return parent, nil
}

func (r *SubTenantReconciler) protectSubTenant(ctx context.Context, obj *cloud.SubTenant, st cloud.SubTenantStatus) (ctrl.Result, error) {
	before := obj.DeepCopy()
	controllerutil.AddFinalizer(obj, cloud.SubTenantFinalizer)
	labels := maps.Clone(obj.Labels)
	if labels == nil {
		labels = crossplane.Labels(obj.Spec.TenantRef.Name)
	}
	labels[cloudstore.LabelTenant] = obj.Spec.TenantRef.Name
	labels[cloudstore.LabelID] = obj.Name
	labels[cloudstore.LabelClusterID] = obj.Spec.ClusterRef.ID
	obj.Labels = labels
	if err := r.deps.Direct.Patch(ctx, obj, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	result, err := r.subTenantReport(ctx, obj, st, cloud.SubTenantStatePending, "ControllerProtectionAcquired", "Controller protection acquired; no workload write occurs until the next reconciliation", false)
	result.RequeueAfter = soon
	return result, err
}

func (r *SubTenantReconciler) subTenantReport(ctx context.Context, obj *cloud.SubTenant, st cloud.SubTenantStatus, state cloud.SubTenantState, reason, message string, warning bool) (ctrl.Result, error) {
	if !obj.DeletionTimestamp.IsZero() {
		state = cloud.SubTenantStateDeleting
	}
	st.State, st.Message = state, message
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: reason, Message: message, ObservedGeneration: obj.Generation})
	if subTenantPrerequisiteReason(reason) {
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: "PrerequisitesReady", Status: metav1.ConditionFalse, Reason: reason, Message: message, ObservedGeneration: obj.Generation})
	}
	accessReason, accessMessage := "QualificationRequired", "No identity resolver or runtime isolation qualification is configured; developer access remains closed"
	if !meta.IsStatusConditionTrue(st.Conditions, "PrerequisitesReady") {
		accessReason, accessMessage = "PrerequisitesPending", message
	}
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: "AccessReady", Status: metav1.ConditionFalse, Reason: accessReason, Message: accessMessage, ObservedGeneration: obj.Generation})
	for i := range st.Namespaces {
		ns := &st.Namespaces[i]
		previous := slices.IndexFunc(obj.Status.Namespaces, func(old cloud.SubTenantNamespaceStatus) bool { return old.Key == ns.Key && old.UID == ns.UID })
		conditions := append([]metav1.Condition(nil), ns.Conditions...)
		ns.Conditions = nil
		if previous >= 0 {
			ns.Conditions = append(ns.Conditions, obj.Status.Namespaces[previous].Conditions...)
		}
		for _, condition := range conditions {
			condition.ObservedGeneration = obj.Generation
			meta.SetStatusCondition(&ns.Conditions, condition)
		}
	}
	changed := obj.Status.State != state || obj.Status.Message != message || meta.FindStatusCondition(obj.Status.Conditions, "Ready") == nil || meta.FindStatusCondition(obj.Status.Conditions, "Ready").Reason != reason
	if !equality.Semantic.DeepEqual(obj.Status, st) {
		before := obj.DeepCopy()
		obj.Status = st
		if err := r.deps.Direct.Status().Patch(ctx, obj, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		if changed {
			eventType := platform.EventNormal
			if warning {
				eventType = platform.EventWarning
			}
			r.deps.Emit(ctx, platform.Object{Kind: platform.KindSubTenant, Namespace: obj.Namespace, Name: obj.Name}, eventType, reason, message)
		}
	}
	return ctrl.Result{RequeueAfter: pendingDelay}, nil
}

func (r *SubTenantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	all := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []ctrl.Request {
		return enqueueAll(ctx, r.deps.Direct, &cloud.SubTenantList{})
	})
	return ctrl.NewControllerManagedBy(mgr).For(&cloud.SubTenant{}, forObject()).Named("subtenant").WithOptions(objectOptions(r.deps)).Watches(&cloud.Cluster{}, all).Watches(&cloud.Tenant{}, all).Watches(&cloud.ReleaseHold{}, all).Complete(r)
}

func (r *SubTenantReconciler) deleteSubTenant(ctx context.Context, obj *cloud.SubTenant, st cloud.SubTenantStatus, workload client.Client, remote *unstructured.Unstructured) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(obj, cloud.SubTenantFinalizer) {
		return ctrl.Result{}, nil
	}
	if remote != nil {
		st.RemoteCapsuleTenant = remote.GetName()
		st.RemoteCapsuleTenantUID = string(remote.GetUID())
		if remote.GetLabels()[naas.LabelAccessReady] != "false" {
			next := remote.DeepCopy()
			labels := maps.Clone(next.GetLabels())
			labels[naas.LabelAccessReady] = "false"
			next.SetLabels(labels)
			if err := workload.Update(ctx, next); err != nil {
				return r.subTenantReport(ctx, obj, st, cloud.SubTenantStateDeleting, "AccessRevocationPending", "Cannot close access before offboarding; protection is retained", true)
			}
			remote = next
		}
	}
	if remote == nil {
		namespaces, blockers, err := naas.PreflightNamespaceCleanup(ctx, workload, remote, obj)
		if err != nil || len(namespaces) > 0 || len(blockers) > 0 {
			st.DeletionBlockers = blockers
			return r.subTenantReport(ctx, obj, st, cloud.SubTenantStateDeleting, "DeletionBlocked", "Capsule Tenant is absent but namespace absence/ownership cannot be established; management protection is retained", true)
		}
		result, err := r.subTenantReport(ctx, obj, st, cloud.SubTenantStateDeleting, "RemoteCleanupObserved", "Capsule Tenant absence and namespace emptiness were observed; releasing only this controller's protection", false)
		if err != nil {
			return result, err
		}
		before := obj.DeepCopy()
		controllerutil.RemoveFinalizer(obj, cloud.SubTenantFinalizer)
		return ctrl.Result{}, client.IgnoreNotFound(r.deps.Direct.Patch(ctx, obj, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})))
	}
	if !remote.GetDeletionTimestamp().IsZero() {
		return r.subTenantReport(ctx, obj, st, cloud.SubTenantStateDeleting, "RemoteDeletionPending", "Waiting for Capsule Tenant deletion; all controller finalizers are retained", false)
	}
	if remote.GetAnnotations()[naas.DeletionAnnotation] != "true" || remote.GetLabels()[naas.LabelProfile] != "terminating" {
		// Let the active shared namespace template observe the closed access
		// gate before deselecting it. Management never rewrites namespace labels.
		list := &corev1.NamespaceList{}
		if err := workload.List(ctx, list); err != nil {
			return r.subTenantReport(ctx, obj, st, cloud.SubTenantStateDeleting, "AccessObservationPending", "Cannot observe namespace access-gate closure", true)
		}
		for _, ns := range list.Items {
			if (ns.Labels[naas.LabelSubTenantID] == obj.Name || strings.HasPrefix(ns.Name, remote.GetName()+"-")) && ns.Labels["cloud.cluster-vending.io/naas-ready"] != "false" {
				return r.subTenantReport(ctx, obj, st, cloud.SubTenantStateDeleting, "AccessObservationPending", "Waiting for shared Capsule templates to close namespace readiness before withdrawal", false)
			}
		}
		next := naas.WithdrawTenantGeneration(remote)
		if err := unstructured.SetNestedField(next.Object, false, "spec", "cordoned"); err != nil {
			return ctrl.Result{}, err
		}
		if err := workload.Update(ctx, next); err != nil {
			return r.subTenantReport(ctx, obj, st, cloud.SubTenantStateDeleting, "NamespaceWithdrawalPending", "Cannot withdraw shared namespace generation; Tenant deletion remains protected", true)
		}
		return r.subTenantReport(ctx, obj, st, cloud.SubTenantStateDeleting, "NamespaceGenerationStopping", "Namespace generation was withdrawn on the owned Tenant; waiting for current Capsule/GTR observations before deletion", false)
	}
	if remote.GetAnnotations()[naas.DeletionFenceAnnotation] == "" {
		fence, err := naas.CaptureTenantDeletionFence(ctx, workload)
		if err != nil {
			return r.subTenantReport(ctx, obj, st, cloud.SubTenantStateDeleting, "GenerationWithdrawalUnconfirmed", err.Error(), false)
		}
		next := remote.DeepCopy()
		annotations := maps.Clone(next.GetAnnotations())
		annotations[naas.DeletionFenceAnnotation] = fence
		next.SetAnnotations(annotations)
		if err := workload.Update(ctx, next); err != nil {
			return r.subTenantReport(ctx, obj, st, cloud.SubTenantStateDeleting, "GenerationWithdrawalUnconfirmed", "Cannot record a post-withdrawal observation fence", true)
		}
		return r.subTenantReport(ctx, obj, st, cloud.SubTenantStateDeleting, "GenerationWithdrawalUnconfirmed", "Waiting for fresh shared GTR observations after namespace-generation withdrawal", false)
	}
	if err := naas.TenantDeletionAcknowledged(ctx, workload, remote); err != nil {
		return r.subTenantReport(ctx, obj, st, cloud.SubTenantStateDeleting, "GenerationWithdrawalUnconfirmed", err.Error(), false)
	}
	// Only fresh, deselected Capsule/GTR observations permit the complete
	// inventory scan. Unknown content or partial discovery blocks all cleanup.
	namespaces, blockers, err := naas.PreflightNamespaceCleanup(ctx, workload, remote, obj)
	if err != nil {
		return r.subTenantReport(ctx, obj, st, cloud.SubTenantStateDeleting, "DeletionObservationUnavailable", "Complete namespace content inspection failed: "+err.Error(), true)
	}
	st.DeletionBlockers = blockers
	if len(blockers) > 0 {
		// Cordon blocks DELETE as well as CREATE/UPDATE. Keep generation
		// withdrawn, but permit user remediation of newly discovered content.
		if cordoned, _, _ := unstructured.NestedBool(remote.Object, "spec", "cordoned"); cordoned {
			next := remote.DeepCopy()
			if err := unstructured.SetNestedField(next.Object, false, "spec", "cordoned"); err != nil {
				return ctrl.Result{}, err
			}
			if err := workload.Update(ctx, next); err != nil {
				return r.subTenantReport(ctx, obj, st, cloud.SubTenantStateDeleting, "RemediationPending", "User content remains; cannot release the cordon for remediation. No user objects are deleted", true)
			}
		}
		return r.subTenantReport(ctx, obj, st, cloud.SubTenantStateDeleting, "DeletionBlocked", fmt.Sprintf("%d content/ownership blockers remain; no user objects are deleted. Remove the reported content before retrying", len(blockers)), true)
	}
	if len(namespaces) > 0 {
		if cordoned, _, _ := unstructured.NestedBool(remote.Object, "spec", "cordoned"); !cordoned {
			next := remote.DeepCopy()
			if err := unstructured.SetNestedField(next.Object, true, "spec", "cordoned"); err != nil {
				return ctrl.Result{}, err
			}
			next.SetGeneration(max(int64(1), remote.GetGeneration()+1))
			if err := workload.Update(ctx, next); err != nil {
				return r.subTenantReport(ctx, obj, st, cloud.SubTenantStateDeleting, "NamespaceCleanupPending", "Cannot establish the empty-namespace write barrier", true)
			}
			return r.subTenantReport(ctx, obj, st, cloud.SubTenantStateDeleting, "NamespaceCleanupPending", "Empty-content preflight passed; waiting for observed Capsule cordoning before a fresh final content check", false)
		}
		for i := range namespaces {
			if !namespaces[i].DeletionTimestamp.IsZero() {
				continue
			}
			if err := naas.DeleteEmptyNamespace(ctx, workload, remote, obj, &namespaces[i]); err != nil {
				return r.subTenantReport(ctx, obj, st, cloud.SubTenantStateDeleting, "NamespaceCleanupPending", err.Error(), true)
			}
			return r.subTenantReport(ctx, obj, st, cloud.SubTenantStateDeleting, "NamespaceCleanupPending", "Requested deletion of one owned, cordoned namespace after complete empty-content inspection; waiting for namespace absence", false)
		}
		return r.subTenantReport(ctx, obj, st, cloud.SubTenantStateDeleting, "NamespaceCleanupPending", "Waiting for Capsule/Kubernetes namespace finalization; no finalizers are stripped", false)
	}
	protected, _, _ := unstructured.NestedBool(remote.Object, "spec", "preventDeletion")
	if protected {
		next := remote.DeepCopy()
		if err := unstructured.SetNestedField(next.Object, false, "spec", "preventDeletion"); err != nil {
			return ctrl.Result{}, err
		}
		// This change creates another generation that Capsule must observe before
		// delete, and a later pass performs another namespace check.
		next.SetGeneration(max(int64(1), remote.GetGeneration()+1))
		if err := workload.Update(ctx, next); err != nil {
			return r.subTenantReport(ctx, obj, st, cloud.SubTenantStateDeleting, "RemoteDeletionPending", "Cannot release the owned empty Tenant's deletion protection", true)
		}
		return r.subTenantReport(ctx, obj, st, cloud.SubTenantStateDeleting, "RemoteDeletionPending", "Owned empty Tenant deletion protection released; waiting for current observation before delete", false)
	}
	uid, rv := remote.GetUID(), remote.GetResourceVersion()
	if err := workload.Delete(ctx, remote, client.Preconditions{UID: &uid, ResourceVersion: &rv}, client.PropagationPolicy(metav1.DeletePropagationOrphan)); err != nil && !apierrors.IsNotFound(err) {
		return r.subTenantReport(ctx, obj, st, cloud.SubTenantStateDeleting, "RemoteDeletionPending", "Owned empty Capsule Tenant deletion was not confirmed", true)
	}
	return r.subTenantReport(ctx, obj, st, cloud.SubTenantStateDeleting, "RemoteDeletionPending", "Only the owned Capsule Tenant was requested for deletion; absence and namespace inventory must be re-observed before releasing management protection", false)
}
