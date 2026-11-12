package idl_test

import (
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// existing_cluster (ClusterSpec field 7) was removed: its number and name
// are reserved so nothing reuses them, and a JSON request that still sends
// it decodes as the server's codec does (unknown fields dropped).
func TestExistingClusterIsReserved(t *testing.T) {
	d := (&cloudv1.ClusterSpec{}).ProtoReflect().Descriptor()
	if !d.ReservedRanges().Has(7) || !d.ReservedNames().Has("existing_cluster") {
		t.Error("ClusterSpec does not reserve field 7 / existing_cluster")
	}
	if d.Fields().ByNumber(7) != nil {
		t.Error("ClusterSpec still has a field 7")
	}
	in := []byte(`{"region":"us-east-1","existingCluster":{"name":"tf-prod"}}`)
	spec := &cloudv1.ClusterSpec{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(in, spec); err != nil || spec.GetRegion() != "us-east-1" {
		t.Errorf("JSON with existingCluster: %v, region %q", err, spec.GetRegion())
	}
	if err := protojson.Unmarshal(in, &cloudv1.ClusterSpec{}); err == nil {
		t.Error("strict JSON decoding accepted the removed field")
	}
}
