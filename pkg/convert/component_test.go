package convert_test

import (
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/convert"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// A component's contract status survives the kind's status: what the store
// writes is what it reads back.
func TestComponentStatusRoundTrips(t *testing.T) {
	values, err := structpb.NewStruct(map[string]any{"replicas": 2, "settings": map[string]any{"clusterName": "prod"}})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	in := &cloudv1.ComponentStatus{
		Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_DEGRADED, Message: "1 of 2 replicas ready", ObservedGeneration: 3,
		Conditions: []*cloudv1.Condition{{Type: "Ready", Status: cloudv1.ConditionStatus_CONDITION_STATUS_FALSE,
			Reason: "ReplicasUnavailable", Message: "1 of 2", LastTransitionTime: timestamppb.New(at)}},
		ClusterName: "prod", InstalledVersion: "1.8.1", TargetVersion: "1.8.2", Release: "2026-11-a",
		EffectiveValues: values,
	}
	into := &cloudv1alpha1.ComponentStatus{ObservedGeneration: 3,
		Inventory: []cloudv1alpha1.InventoryEntry{{Kind: "Release", Name: "prod-karpenter"}}}
	convert.ComponentStatusInto(in, "", metav1.NewTime(at), into)
	if into.Phase != "Degraded" || into.Cluster != "prod" || len(into.Inventory) != 1 {
		t.Errorf("kind status = %+v", into)
	}
	if out := convert.ComponentStatusFromAPI(into); !proto.Equal(out, in) {
		t.Errorf("round trip = %v, want %v", out, in)
	}
}

// Without a phase in the status, the stored phase name gives it.
func TestComponentPhaseFromStoredState(t *testing.T) {
	into := &cloudv1alpha1.ComponentStatus{}
	convert.ComponentStatusInto(&cloudv1.ComponentStatus{}, "COMPONENT_PHASE_DELETING", metav1.Now(), into)
	if into.Phase != "Deleting" {
		t.Errorf("phase = %q, want Deleting", into.Phase)
	}
}

func TestValuesConversion(t *testing.T) {
	if convert.StructToRaw(nil) != nil {
		t.Error("no values became a document")
	}
	if s, err := convert.RawToStruct(nil); s != nil || err != nil {
		t.Errorf("RawToStruct(nil) = %v, %v", s, err)
	}
	if _, err := convert.RawToStruct(&runtime.RawExtension{Raw: []byte(`[1]`)}); err == nil {
		t.Error("a list became a Struct")
	}
	s, err := convert.RawToStruct(&runtime.RawExtension{Raw: []byte(`{"a":{"b":true}}`)})
	if err != nil || !s.GetFields()["a"].GetStructValue().GetFields()["b"].GetBoolValue() {
		t.Errorf("RawToStruct = %v, %v", s, err)
	}
}
