package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/naas"
	platform "github.com/yindia/cluster-vending/pkg/reconcile"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=subtenants,verbs=get;list;watch

// guardSubTenantSafety stops native deletion and NaaS-disable edits before
// bump/deleting can schedule cloud or platform changes. It never restores spec.
func (r *ClusterReconciler) guardSubTenantSafety(ctx context.Context, obj *cloudv1alpha1.Cluster, frozen *model.Freeze) (ctrl.Result, bool, error) {
	if obj.DeletionTimestamp.IsZero() && ptr.Deref(obj.Spec.NaaS, false) {
		return ctrl.Result{}, false, r.clearSubTenantSafety(ctx, obj)
	}
	names, readErr := naas.ClusterDependents(ctx, r.deps.Direct, naas.ClusterIdentity{
		Namespace: obj.Namespace, Name: obj.Name, UID: obj.UID, ID: obj.Labels[cloudstore.LabelID],
	})
	if readErr == nil && len(names) == 0 {
		return ctrl.Result{}, false, r.clearSubTenantSafety(ctx, obj)
	}
	action := "NaaS disabling"
	if !obj.DeletionTimestamp.IsZero() {
		action = "Cluster deletion"
	}
	reason, message := naas.ReasonDependents, naas.ParentBlockedMessage(action, names)
	if readErr != nil {
		reason, message = naas.ReasonReadFailed, fmt.Sprintf("%s is blocked until SubTenant dependencies can be verified: %v", action, readErr)
	}
	previous := meta.FindStatusCondition(obj.Status.Conditions, naas.ConditionParentSafety)
	changed := previous == nil || previous.Reason != reason || previous.Message != message || previous.Status != metav1.ConditionFalse
	statusChanged := meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
		Type: naas.ConditionParentSafety, Status: metav1.ConditionFalse, Reason: reason, Message: message, ObservedGeneration: obj.Generation,
	})
	statusMessage := message
	if paused, note := platform.RowFreeze(frozen, nil); paused != nil {
		statusChanged = meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{Type: paused.Type, Status: metav1.ConditionTrue,
			Reason: paused.Reason, Message: paused.Message, ObservedGeneration: obj.Generation}) || statusChanged
		statusMessage += note
	} else {
		statusChanged = meta.RemoveStatusCondition(&obj.Status.Conditions, platform.ConditionFrozen) || statusChanged
	}
	statusChanged = obj.Status.Message != statusMessage || statusChanged
	obj.Status.Message = statusMessage
	contractChanged, statusErr := subTenantSafetyContract(obj, frozen)
	if statusErr == nil && (statusChanged || contractChanged) {
		statusErr = r.deps.Direct.Status().Update(ctx, obj)
	}
	if statusErr == nil && changed {
		r.deps.Emit(ctx, platform.Object{Kind: platform.KindCluster, Namespace: obj.Namespace, Name: obj.Name}, platform.EventWarning, reason, message)
	}
	return ctrl.Result{RequeueAfter: failedDelay}, true, errors.Join(readErr, statusErr)
}

// ContractStatus is the API's canonical view. Change only the safety/freeze
// fields, retaining both observed generations and all existing platform status.
func subTenantSafetyContract(obj *cloudv1alpha1.Cluster, frozen *model.Freeze) (bool, error) {
	st := &cloudv1.ClusterStatus{}
	var before []byte
	if obj.Status.ContractStatus != nil {
		before = obj.Status.ContractStatus.Raw
		if len(before) > 0 {
			if err := reconcilecluster.Lenient.Unmarshal(before, st); err != nil {
				return false, fmt.Errorf("decode cluster safety contract status: %w", err)
			}
		}
	}
	st.Message, st.Freeze = obj.Status.Message, platform.FreezeProto(frozen)
	st.Conditions = slices.DeleteFunc(st.Conditions, func(c *cloudv1.Condition) bool {
		return c.Type == naas.ConditionParentSafety || c.Type == platform.ConditionFrozen
	})
	for _, condition := range obj.Status.Conditions {
		if condition.Type != naas.ConditionParentSafety && condition.Type != platform.ConditionFrozen {
			continue
		}
		status := cloudv1.ConditionStatus_CONDITION_STATUS_FALSE
		if condition.Status == metav1.ConditionTrue {
			status = cloudv1.ConditionStatus_CONDITION_STATUS_TRUE
		}
		st.Conditions = append(st.Conditions, &cloudv1.Condition{Type: condition.Type, Status: status,
			Reason: condition.Reason, Message: condition.Message, LastTransitionTime: timestamppb.New(condition.LastTransitionTime.Time)})
	}
	raw, err := reconcilecluster.Encode(st)
	if err != nil {
		return false, err
	}
	obj.Status.ContractStatus = &runtime.RawExtension{Raw: raw}
	return !bytes.Equal(before, raw), nil
}

func (r *ClusterReconciler) clearSubTenantSafety(ctx context.Context, obj *cloudv1alpha1.Cluster) error {
	if !meta.RemoveStatusCondition(&obj.Status.Conditions, naas.ConditionParentSafety) {
		return nil
	}
	return r.deps.Direct.Status().Update(ctx, obj)
}
