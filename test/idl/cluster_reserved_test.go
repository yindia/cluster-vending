package idl_test

import (
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

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

// The node group fields were removed when the platform took over a
// cluster's node groups: ClusterSpec reserves default_node_pool (4) and
// node_groups (16), AwsClusterSpec default_node_pool (4), and their
// messages are gone from the contract.
func TestNodeGroupFieldsAreReserved(t *testing.T) {
	for _, tt := range []struct {
		d      protoreflect.MessageDescriptor
		number protoreflect.FieldNumber
		name   protoreflect.Name
	}{
		{(&cloudv1.ClusterSpec{}).ProtoReflect().Descriptor(), 4, "default_node_pool"},
		{(&cloudv1.ClusterSpec{}).ProtoReflect().Descriptor(), 16, "node_groups"},
		{(&cloudv1.AwsClusterSpec{}).ProtoReflect().Descriptor(), 4, "default_node_pool"},
	} {
		if !tt.d.ReservedRanges().Has(tt.number) || !tt.d.ReservedNames().Has(tt.name) {
			t.Errorf("%s does not reserve %d / %s", tt.d.FullName(), tt.number, tt.name)
		}
		if tt.d.Fields().ByNumber(tt.number) != nil || tt.d.Fields().ByName(tt.name) != nil {
			t.Errorf("%s still has field %d / %s", tt.d.FullName(), tt.number, tt.name)
		}
	}
	for _, name := range []protoreflect.FullName{"cloud.v1.NodePoolSpec", "cloud.v1.NodeGroupSpec", "cloud.v1.NodeGroupLimits",
		"cloud.v1.AwsNodePoolSpec", "cloud.v1.AwsLaunchTemplate", "cloud.v1.GetNodeGroupOptionsRequest"} {
		if _, err := protoregistry.GlobalFiles.FindDescriptorByName(name); err == nil {
			t.Errorf("%s is still in the contract", name)
		}
	}
	in := []byte(`{"region":"us-east-1","defaultNodePool":{"instanceTypes":["m6i.large"]},"nodeGroups":[{"name":"batch"}],` +
		`"aws":{"defaultNodePool":{"amiType":"AWS_AMI_TYPE_CUSTOM"}}}`)
	spec := &cloudv1.ClusterSpec{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(in, spec); err != nil || spec.GetRegion() != "us-east-1" {
		t.Errorf("JSON with the removed node group fields: %v, region %q", err, spec.GetRegion())
	}
}
