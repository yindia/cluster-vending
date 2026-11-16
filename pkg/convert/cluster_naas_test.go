package convert_test

import (
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"k8s.io/utils/ptr"

	"github.com/yindia/cluster-vending/pkg/convert"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func TestClusterNaaSRoundTripPreservesExplicitFalse(t *testing.T) {
	for _, value := range []*bool{nil, ptr.To(false), ptr.To(true)} {
		input := &cloudv1.ClusterSpec{Naas: value}
		api, remainder := convert.ClusterSpecToAPI(input, "network")
		copied := api.DeepCopy()
		output := convert.ClusterSpecFromAPI(*copied, "network-id", remainder)
		if (output.Naas == nil) != (value == nil) || output.GetNaas() != ptr.Deref(value, false) {
			t.Fatalf("NaaS presence/value changed: %v -> %v", input, output)
		}
		if value != nil {
			*copied.NaaS = !*value
			if *api.NaaS != *value {
				t.Fatal("deep copy aliases NaaS")
			}
		}
	}
	for _, raw := range []string{`{}`, `{"naas":false}`, `{"naas":true}`} {
		spec := &cloudv1.ClusterSpec{}
		if err := protojson.Unmarshal([]byte(raw), spec); err != nil {
			t.Fatal(err)
		}
		got, err := protojson.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != raw {
			t.Fatalf("proto JSON presence: got %s want %s", got, raw)
		}
	}
}
