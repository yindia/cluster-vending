package cluster

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/naas"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// guardSubTenantSafety is also enforced at the reconciler entry point: callers
// that bypass the Cluster controller must not tear down cloud resources or
// uninstall Capsule. Only status/events are written while the request is held.
func (s *Reconciler) guardSubTenantSafety(ctx context.Context, row *model.Cluster) error {
	spec, err := SpecOf(row)
	if err != nil {
		return fmt.Errorf("read cluster spec for SubTenant safety: %w", err)
	}
	if row.State != StateDeleting && spec.GetNaas() {
		return nil
	}
	names, readErr := naas.ClusterDependents(ctx, s.deps.Reader(), naas.ClusterIdentity{
		Namespace: kube.TenantNamespace(row.TenantID), Name: row.Name, ID: row.ID,
	})
	if readErr == nil && len(names) == 0 {
		return nil
	}
	action := "NaaS disabling"
	if row.State == StateDeleting {
		action = "Cluster deletion"
	}
	reason, message := naas.ReasonDependents, naas.ParentBlockedMessage(action, names)
	if readErr != nil {
		reason, message = naas.ReasonReadFailed, fmt.Sprintf("%s is blocked until SubTenant dependencies can be verified: %v", action, readErr)
	}
	st := storedStatus(row)
	var previous *cloudv1.Condition
	for _, condition := range st.Conditions {
		if condition.Type == naas.ConditionParentSafety {
			previous = condition
			break
		}
	}
	changed := previous == nil || previous.Reason != reason || previous.Message != message
	condition := &cloudv1.Condition{Type: naas.ConditionParentSafety, Status: cloudv1.ConditionStatus_CONDITION_STATUS_FALSE,
		Reason: reason, Message: message, LastTransitionTime: timestamppb.New(now())}
	if !changed {
		condition.LastTransitionTime = previous.LastTransitionTime
	}
	st.Conditions = slices.DeleteFunc(st.Conditions, func(c *cloudv1.Condition) bool { return c.Type == naas.ConditionParentSafety })
	st.Conditions = append(st.Conditions, condition)
	st.State, st.Message = cloudv1.ClusterState(cloudv1.ClusterState_value[row.State]), message
	st.Freeze = reconcile.FreezeProto(row.Frozen)
	st.Conditions = slices.DeleteFunc(st.Conditions, func(c *cloudv1.Condition) bool { return c.Type == reconcile.ConditionFrozen })
	if paused, note := reconcile.RowFreeze(row.Frozen, st.ManagedResources); paused != nil {
		st.Conditions = append(st.Conditions, paused)
		st.Message += note
	}
	statusErr := s.rememberSubTenantSafety(ctx, row, st, reason, message)
	if statusErr == nil && changed {
		s.deps.Emit(ctx, object(row), reconcile.EventWarning, reason, message)
	}
	if readErr != nil {
		return errors.Join(readErr, statusErr)
	}
	return errors.Join(errors.New(message), statusErr)
}

// Ordinary store status writes acknowledge the native CR generation. A safety
// hold must leave that generation pending, so update only the safety status
// through a freshly read CR and preserve its observed generation verbatim.
func (s *Reconciler) rememberSubTenantSafety(ctx context.Context, row *model.Cluster, st *cloudv1.ClusterStatus, reason, message string) error {
	obj := &cloudv1alpha1.Cluster{}
	if err := s.deps.Reader().Get(ctx, client.ObjectKey{Namespace: kube.TenantNamespace(row.TenantID), Name: row.Name}, obj); err != nil {
		return fmt.Errorf("read cluster for safety status: %w", err)
	}
	raw, err := Encode(st)
	if err != nil {
		return err
	}
	meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{Type: naas.ConditionParentSafety, Status: metav1.ConditionFalse,
		Reason: reason, Message: message, ObservedGeneration: obj.Generation})
	meta.RemoveStatusCondition(&obj.Status.Conditions, reconcile.ConditionFrozen)
	for _, condition := range st.Conditions {
		if condition.Type == reconcile.ConditionFrozen {
			meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{Type: condition.Type, Status: metav1.ConditionTrue,
				Reason: condition.Reason, Message: condition.Message, ObservedGeneration: obj.Generation})
		}
	}
	obj.Status.Message = st.Message
	obj.Status.ContractStatus = &runtime.RawExtension{Raw: raw}
	if err := s.deps.Kube.Status().Update(ctx, obj); err != nil {
		return err
	}
	row.Status = raw
	return nil
}
