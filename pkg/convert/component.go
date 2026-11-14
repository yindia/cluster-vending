package convert

import (
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// prefixComponentPhase is the prefix of the contract's ComponentPhase names.
const prefixComponentPhase = "COMPONENT_PHASE_"

// ComponentStatusInto writes what a contract status says into the kind's
// status: phase, message, conditions, cluster name, versions, release and
// effective values. The fields the contract has no say in (inventory,
// observedGeneration) are left as they are. state is the phase's source
// when the status carries none (the stored phase name); at is the
// transition time of conditions that changed.
func ComponentStatusInto(p *cloudv1.ComponentStatus, state string, at metav1.Time, into *cloudv1alpha1.ComponentStatus) {
	into.Phase = phase(p.GetPhase(), prefixComponentPhase, state)
	into.Message = p.GetMessage()
	into.Conditions = conditions(p.GetConditions(), into.Conditions, at)
	into.Cluster = p.GetClusterName()
	into.InstalledVersion = p.GetInstalledVersion()
	into.TargetVersion = p.GetTargetVersion()
	into.Release = p.GetRelease()
	into.EffectiveValues = StructToRaw(p.GetEffectiveValues())
	into.Since = nil
	if t := p.GetSince(); t != nil {
		at := metav1.NewTime(t.AsTime())
		into.Since = &at
	}
	into.Chart, into.HelmState, into.LastError = p.GetChart(), p.GetHelmState(), p.GetLastError()
	into.Failures, into.Recoveries = p.GetFailures(), p.GetRecoveries()
}

// ComponentStatusFromAPI is the contract status the kind's status says:
// the reverse of ComponentStatusInto. observedGeneration is the kind's.
func ComponentStatusFromAPI(st *cloudv1alpha1.ComponentStatus) *cloudv1.ComponentStatus {
	out := &cloudv1.ComponentStatus{
		Phase:   cloudv1.ComponentPhase(uncamel(cloudv1.ComponentPhase(0).Descriptor(), prefixComponentPhase, st.Phase)),
		Message: st.Message, ObservedGeneration: st.ObservedGeneration, ClusterName: st.Cluster,
		InstalledVersion: st.InstalledVersion, TargetVersion: st.TargetVersion, Release: st.Release,
		Chart: st.Chart, HelmState: st.HelmState, LastError: st.LastError, Failures: st.Failures, Recoveries: st.Recoveries,
	}
	if st.Since != nil {
		out.Since = timestamppb.New(st.Since.Time)
	}
	for _, c := range st.Conditions {
		out.Conditions = append(out.Conditions, &cloudv1.Condition{Type: c.Type, Status: conditionStatusFromAPI(c.Status),
			Reason: c.Reason, Message: c.Message, LastTransitionTime: timestamppb.New(c.LastTransitionTime.Time)})
	}
	// Effective values that are not an object (written around the
	// controller) are left out: the status says nothing it cannot say.
	if values, err := RawToStruct(st.EffectiveValues); err == nil {
		out.EffectiveValues = values
	}
	return out
}

func conditionStatusFromAPI(s metav1.ConditionStatus) cloudv1.ConditionStatus {
	switch s {
	case metav1.ConditionTrue:
		return cloudv1.ConditionStatus_CONDITION_STATUS_TRUE
	case metav1.ConditionFalse:
		return cloudv1.ConditionStatus_CONDITION_STATUS_FALSE
	case metav1.ConditionUnknown:
		return cloudv1.ConditionStatus_CONDITION_STATUS_UNKNOWN
	default:
		return cloudv1.ConditionStatus_CONDITION_STATUS_UNSPECIFIED
	}
}

// StructToRaw is a Struct as the schemaless JSON object a kind keeps; nil
// for none.
func StructToRaw(s *structpb.Struct) *runtime.RawExtension {
	if s == nil {
		return nil
	}
	raw, err := protojson.Marshal(s)
	if err != nil {
		return nil // a Struct always marshals; NaN and infinities are its only failures
	}
	return &runtime.RawExtension{Raw: raw}
}

// RawToStruct is the reverse of StructToRaw: nil for none, an error when
// the document is not a JSON object.
func RawToStruct(raw *runtime.RawExtension) (*structpb.Struct, error) {
	if raw == nil || len(raw.Raw) == 0 {
		return nil, nil
	}
	out := &structpb.Struct{}
	if err := protojson.Unmarshal(raw.Raw, out); err != nil {
		return nil, err
	}
	return out, nil
}
