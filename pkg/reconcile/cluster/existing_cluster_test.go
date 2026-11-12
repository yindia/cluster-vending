package cluster

import (
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// A binary request from a client built against the old contract, with
// ClusterSpec field 7 (existing_cluster, now reserved), is refused: it asks
// for an import, and creating a new cluster of the name instead would be
// wrong. Unknown fields of other numbers are not.
func TestRemovedExistingClusterIsRefused(t *testing.T) {
	base := &cloudv1.ClusterSpec{Region: "us-east-1", Provider: &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{}}}
	raw, err := proto.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	withField := func(number protowire.Number) *cloudv1.ClusterSpec {
		b := protowire.AppendTag(raw, number, protowire.BytesType)
		b = protowire.AppendBytes(b, []byte{0x0a, 0x07, 't', 'f', '-', 'p', 'r', 'o', 'd'}) // ExistingCluster{name: "tf-prod"}
		out := &cloudv1.ClusterSpec{}
		if err := proto.Unmarshal(b, out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if err := supported(withField(existingClusterField)); !fault.Is(err, fault.Unimplemented) {
		t.Errorf("spec with field 7: %v, want an Unimplemented fault", err)
	}
	if err := supported(withField(99)); err != nil {
		t.Errorf("spec with an unknown field 99: %v, want accepted", err)
	}
	if err := supported(base); err != nil {
		t.Errorf("plain spec: %v", err)
	}
}

func TestHasUnknownField(t *testing.T) {
	var raw []byte
	raw = protowire.AppendTag(raw, 3, protowire.VarintType)
	raw = protowire.AppendVarint(raw, 42)
	raw = protowire.AppendTag(raw, 7, protowire.BytesType)
	raw = protowire.AppendBytes(raw, []byte("x"))
	for _, tt := range []struct {
		raw    []byte
		number protowire.Number
		want   bool
	}{
		{raw, 7, true},
		{raw, 3, true},
		{raw, 8, false},
		{nil, 7, false},
		{[]byte{0xff}, 7, false}, // malformed: not found, no panic
	} {
		if got := hasUnknownField(tt.raw, tt.number); got != tt.want {
			t.Errorf("hasUnknownField(%x, %d) = %t, want %t", tt.raw, tt.number, got, tt.want)
		}
	}
}
