package cluster

import (
	"maps"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// What a cluster keeps when a definition moves to another cloud: anything
// something was installed for.
func TestStartedPlatform(t *testing.T) {
	st := &cloudv1.ClusterStatus{PlatformComponents: []*cloudv1.PlatformComponent{
		{Name: "waiting", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_PENDING},
		{Name: "ready", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_READY},
		{Name: "installing", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING},
		{Name: "refused", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED},
		{Name: "broke", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, Chart: "prod-1a2b-broke"},
	}}
	raw, err := protojson.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}

	got := startedPlatform(&model.Cluster{Resource: model.Resource{Status: raw}})

	want := map[string]bool{"ready": true, "installing": true, "refused": false, "broke": true}
	if !maps.Equal(got, want) {
		t.Fatalf("started %v, want %v", got, want)
	}
}
