package subtenant

import (
	"maps"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

const updatedAnnotation = "cloud.cluster-vending.io/subtenant-updated-at"
const requestAnnotation = "cloud.cluster-vending.io/subtenant-request-id"
const hashAnnotation = "cloud.cluster-vending.io/subtenant-create-hash"

func specToCR(in *v1.SubTenantSpec) cloud.SubTenantSpec {
	out := cloud.SubTenantSpec{Name: in.GetName(), DeletionProtection: ptr.To(in.DeletionProtection == nil || in.GetDeletionProtection()), DisplayName: in.GetDisplayName(), PolicyProfile: in.GetPolicyProfile(), Quota: cloud.SubTenantQuota{NamespaceCount: in.GetQuota().GetNamespaceCount(), Hard: maps.Clone(in.GetQuota().GetHard())}}
	for _, key := range in.GetNamespaceKeys() {
		out.NamespaceKeys = append(out.NamespaceKeys, cloud.SubTenantNamespaceKey(key))
	}
	if in.Access != nil {
		out.Access = &cloud.SubTenantAccess{AdminGroups: slices.Clone(in.Access.AdminGroups), ViewerGroups: slices.Clone(in.Access.ViewerGroups)}
		for _, binding := range in.Access.Bindings {
			out.Access.Bindings = append(out.Access.Bindings, cloud.SubTenantAccessBinding{IdentityRef: binding.IdentityRef, RoleProfile: binding.RoleProfile})
		}
	}
	return out
}
func specFromCR(in cloud.SubTenantSpec) *v1.SubTenantSpec {
	out := &v1.SubTenantSpec{Name: in.Name, DeletionProtection: ptr.To(in.DeletionProtection == nil || *in.DeletionProtection), DisplayName: in.DisplayName, PolicyProfile: in.PolicyProfile, Quota: &v1.SubTenantQuota{NamespaceCount: in.Quota.NamespaceCount, Hard: maps.Clone(in.Quota.Hard)}}
	for _, key := range in.NamespaceKeys {
		out.NamespaceKeys = append(out.NamespaceKeys, string(key))
	}
	if in.Access != nil {
		out.Access = &v1.SubTenantAccess{AdminGroups: slices.Clone(in.Access.AdminGroups), ViewerGroups: slices.Clone(in.Access.ViewerGroups)}
		for _, binding := range in.Access.Bindings {
			out.Access.Bindings = append(out.Access.Bindings, &v1.SubTenantAccessBinding{IdentityRef: binding.IdentityRef, RoleProfile: binding.RoleProfile})
		}
	}
	return out
}
func conditions(in []metav1.Condition) []*v1.Condition {
	out := make([]*v1.Condition, 0, len(in))
	for _, condition := range in {
		var status v1.ConditionStatus
		switch condition.Status {
		case metav1.ConditionTrue:
			status = v1.ConditionStatus_CONDITION_STATUS_TRUE
		case metav1.ConditionFalse:
			status = v1.ConditionStatus_CONDITION_STATUS_FALSE
		default:
			status = v1.ConditionStatus_CONDITION_STATUS_UNKNOWN
		}
		c := &v1.Condition{Type: condition.Type, Status: status, Reason: condition.Reason, Message: condition.Message}
		if !condition.LastTransitionTime.IsZero() {
			c.LastTransitionTime = timestamppb.New(condition.LastTransitionTime.Time)
		}
		out = append(out, c)
	}
	return out
}
func resourceView(obj *cloud.SubTenant) *v1.SubTenant {
	st := obj.Status
	state := v1.SubTenantState(v1.SubTenantState_value["SUB_TENANT_STATE_"+strings.ToUpper(string(st.State))])
	status := &v1.SubTenantStatus{ObservedGeneration: st.ObservedGeneration, State: state, Message: st.Message, Conditions: conditions(st.Conditions), RemoteCapsuleTenant: st.RemoteCapsuleTenant, RemoteCapsuleTenantUid: st.RemoteCapsuleTenantUID, TemplateRevision: st.TemplateRevision, DeletionBlockers: append([]string(nil), st.DeletionBlockers...)}
	for _, namespace := range st.Namespaces {
		status.Namespaces = append(status.Namespaces, &v1.SubTenantNamespaceStatus{Key: string(namespace.Key), Name: namespace.Name, Uid: namespace.UID, TemplateRevision: namespace.TemplateRevision, Conditions: conditions(namespace.Conditions)})
	}
	pending := state == v1.SubTenantState_SUB_TENANT_STATE_UNSPECIFIED || st.ObservedGeneration < obj.Generation
	if !obj.DeletionTimestamp.IsZero() {
		status.State = v1.SubTenantState_SUB_TENANT_STATE_DELETING
		status.Message = "Deletion requested. Require-empty reconciliation must confirm remote resources can be removed; this registration remains terminating until a controller completes it."
	} else if pending {
		status.State = v1.SubTenantState_SUB_TENANT_STATE_PENDING
		status.Message = "Desired allocation stored; awaiting controller reconciliation and isolation qualification. Persistence does not mean Ready."
	}
	if pending || !obj.DeletionTimestamp.IsZero() {
		filtered := status.Conditions[:0]
		for _, condition := range status.Conditions {
			if condition.Type != "Ready" {
				filtered = append(filtered, condition)
			}
		}
		filtered = append(filtered, &v1.Condition{Type: "Ready", Status: v1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: "AwaitingReconciliation", Message: status.Message})
		status.Conditions = filtered
	}
	out := &v1.SubTenant{TenantId: obj.Spec.TenantRef.Name, Id: obj.Name, ClusterId: obj.Spec.ClusterRef.ID, Spec: specFromCR(obj.Spec), Status: status, Etag: obj.ResourceVersion, Generation: obj.Generation}
	if !obj.CreationTimestamp.IsZero() {
		out.CreateTime = timestamppb.New(obj.CreationTimestamp.Time)
	}
	if updated, err := time.Parse(time.RFC3339Nano, obj.Annotations[updatedAnnotation]); err == nil {
		out.UpdateTime = timestamppb.New(updated)
	} else {
		out.UpdateTime = out.CreateTime
	}
	return out
}
