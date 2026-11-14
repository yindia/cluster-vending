package cluster

import (
	"testing"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// An automatic upgrade stores the next minor as an update of the version
// would: in the spec and the client's input, a new generation whose
// versions the reconciler resolves before it applies it, the cluster
// updating.
func TestSetAutomaticVersion(t *testing.T) {
	spec, err := Encode(&cloudv1.ClusterSpec{Region: "us-west-1", ControlPlane: &cloudv1.ControlPlaneSpec{KubernetesVersion: "1.36"},
		Provider: &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{}}})
	if err != nil {
		t.Fatal(err)
	}
	input, err := Encode(&cloudv1.Cluster{Name: "prod", Spec: &cloudv1.ClusterSpec{Region: "us-west-1"}})
	if err != nil {
		t.Fatal(err)
	}
	row := &model.Cluster{Resource: model.Resource{Spec: spec, Generation: 4, AppliedGeneration: 4, State: StateRunning},
		Input: input, ResolvedGeneration: 4}

	if err := SetAutomaticVersion(row, "1.37"); err != nil {
		t.Fatal(err)
	}

	got, err := SpecOf(row)
	if err != nil || got.GetControlPlane().GetKubernetesVersion() != "1.37" {
		t.Fatalf("spec version = %q, %v", got.GetControlPlane().GetKubernetesVersion(), err)
	}
	in := &cloudv1.Cluster{}
	if err := Lenient.Unmarshal(row.Input, in); err != nil || in.GetSpec().GetControlPlane().GetKubernetesVersion() != "1.37" {
		t.Errorf("input version = %q, %v", in.GetSpec().GetControlPlane().GetKubernetesVersion(), err)
	}
	if row.Generation != 5 || row.ResolvedGeneration != 4 || row.State != StateUpdating {
		t.Errorf("generation %d, resolved %d, state %s; want 5, 4, %s", row.Generation, row.ResolvedGeneration, row.State, StateUpdating)
	}
	if len(got.GetAws().GetAddons()) == 0 {
		t.Error("the add-ons are not merged with the defaults, to be resolved for the new minor")
	}
}
