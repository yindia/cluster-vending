package aws_test

import (
	"encoding/json"
	"reflect"
	"testing"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// An import keeps the node group it adopted with the adoption; it reads
// back as it was stored.
func TestParseAdoptionKeepsTheAdoptedNodeGroup(t *testing.T) {
	group := &crossplaneaws.NodePoolPlan{InstanceTypes: []string{"m6i.large"}, CapacityType: "SPOT", AMIType: "AL2023_ARM_64_STANDARD",
		MinSize: 1, MaxSize: 5, Desired: 3, Labels: map[string]string{"role": "core"},
		Taints:         []crossplaneaws.TaintPlan{{Key: "dedicated", Value: "core", Effect: "NO_SCHEDULE"}},
		LaunchTemplate: &crossplaneaws.LaunchTemplatePlan{ID: "lt-0123456789abcdef0", Version: "4"}}
	raw, err := json.Marshal(crossplaneaws.Adoption{NodeRoleName: "their-node-role", NodeGroup: group})
	if err != nil {
		t.Fatal(err)
	}

	got := crossplaneaws.ParseAdoption(raw)

	if !reflect.DeepEqual(got.NodeGroup, group) || got.NodeRoleName != "their-node-role" {
		t.Errorf("adoption = %+v, node group %+v", got, got.NodeGroup)
	}
}

// A cluster imported before the adoption kept its node group has it only in
// the spec its import derived, in the removed contract fields: it is read
// from there, in EKS spellings, so the cluster keeps rendering it.
func TestParseAdoptionReadsAnOlderImportsNodeGroupFromItsDerivedSpec(t *testing.T) {
	raw := []byte(`{"nodeRoleName":"their-node-role","externalNames":{"prod-default":"ng-1"},
		"importDiscovered":{"spec":{"region":"us-east-1",
			"defaultNodePool":{"instanceTypes":["m6i.large"],"capacityType":"CAPACITY_TYPE_ON_DEMAND",
				"scalingConfig":{"minSize":2,"maxSize":4,"desiredSize":3},"labels":{"role":"core"},
				"taints":[{"key":"dedicated","value":"core","effect":"TAINT_EFFECT_NO_EXECUTE"}]},
			"aws":{"defaultNodePool":{"amiType":"AWS_AMI_TYPE_BOTTLEROCKET_X86_64","launchTemplate":{"id":"lt-0123456789abcdef0","version":"2"}}}}}}`)

	got := crossplaneaws.ParseAdoption(raw).NodeGroup

	want := &crossplaneaws.NodePoolPlan{InstanceTypes: []string{"m6i.large"}, CapacityType: "ON_DEMAND", AMIType: "BOTTLEROCKET_x86_64",
		MinSize: 2, MaxSize: 4, Desired: 3, Labels: map[string]string{"role": "core"},
		Taints:         []crossplaneaws.TaintPlan{{Key: "dedicated", Value: "core", Effect: "NO_EXECUTE"}},
		LaunchTemplate: &crossplaneaws.LaunchTemplatePlan{ID: "lt-0123456789abcdef0", Version: "2"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("node group = %+v\nwant %+v", got, want)
	}
}

// A cluster the platform created has no adoption and no adopted group;
// what cannot be read is no adoption.
func TestParseAdoptionOfAPlatformCluster(t *testing.T) {
	for _, raw := range []string{"", "{}", "not json", `{"importDiscovered":{"spec":{"defaultNodePool":{"instanceTypes":["m6i.large"]}}}}`} {
		if got := crossplaneaws.ParseAdoption([]byte(raw)); got.Imported() || got.NodeGroup != nil {
			t.Errorf("%q: adoption = %+v", raw, got)
		}
	}
}
