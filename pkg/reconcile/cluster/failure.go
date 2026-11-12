package cluster

import (
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// ConditionReconciled is the condition a cluster carries while the last
// attempt to move it towards its spec failed. The API only stores what
// was asked for; what goes wrong applying it is reported here, not as the
// answer of the call.
const ConditionReconciled = "Reconciled"

// Reasons of ConditionReconciled.
const (
	// ReasonFailed: the cluster cannot be built as it is (its network is
	// not owned, a provider is missing): it is FAILED until that changes.
	// It is still retried.
	ReasonFailed = "ReconcileFailed"
	// ReasonRetrying: something the platform depends on did not answer;
	// the next pass tries again.
	ReasonRetrying = "ReconcileRetrying"
)

// internalFailure is what a caller is told of a failure that is not a
// fault: its text may hold details of the management cluster.
const internalFailure = "the platform could not apply the change; it is retried (see the server log)"

// FailureCondition is the ConditionReconciled of a failed attempt; nil
// for none.
func FailureCondition(err error) *cloudv1.Condition {
	if err == nil {
		return nil
	}
	c := &cloudv1.Condition{Type: ConditionReconciled, Status: cloudv1.ConditionStatus_CONDITION_STATUS_FALSE,
		Reason: ReasonRetrying, Message: internalFailure}
	if f, ok := fault.As(err); ok {
		c.Message = f.Message
		switch f.Kind {
		case fault.FailedPrecondition, fault.Invalid, fault.Unimplemented, fault.NotFound, fault.AlreadyExists:
			// AlreadyExists: EKS has a cluster of the name already.
			c.Reason = ReasonFailed
		default:
		}
	}
	return c
}

// storedFailure is the failure recorded with the row's status, if any.
func storedFailure(row *model.Cluster) *cloudv1.Condition {
	if len(row.Status) == 0 {
		return nil
	}
	st := &cloudv1.ClusterStatus{}
	if err := Lenient.Unmarshal(row.Status, st); err != nil {
		return nil
	}
	for _, c := range st.GetConditions() {
		if c.GetType() == ConditionReconciled {
			return c
		}
	}
	return nil
}

// withFailure reports failure in st: the condition, and FAILED with its
// message when the cluster cannot be built as it is (not while it is
// deleted: that stays DELETING). A failure that is retried only adds its
// message.
func withFailure(st *cloudv1.ClusterStatus, failure *cloudv1.Condition) {
	if failure == nil {
		return
	}
	st.Conditions = append(st.Conditions, failure)
	switch {
	case failure.GetReason() == ReasonFailed && st.GetState() != cloudv1.ClusterState_CLUSTER_STATE_DELETING:
		st.State, st.Message = cloudv1.ClusterState_CLUSTER_STATE_FAILED, failure.GetMessage()
	case st.GetState() != cloudv1.ClusterState_CLUSTER_STATE_FAILED:
		st.Message = "retrying: " + failure.GetMessage()
	}
}

// ClearFailure drops the failure recorded with the row's status: a call
// that changes the cluster stores it with a clean slate, and the next
// pass reports how applying the change went.
func ClearFailure(row *model.Cluster) {
	if storedFailure(row) == nil {
		return
	}
	st := &cloudv1.ClusterStatus{}
	if err := Lenient.Unmarshal(row.Status, st); err != nil {
		return
	}
	kept := st.Conditions[:0]
	for _, c := range st.GetConditions() {
		if c.GetType() != ConditionReconciled {
			kept = append(kept, c)
		}
	}
	st.Conditions = kept
	if raw, err := Encode(st); err == nil {
		row.Status = raw
	}
}
