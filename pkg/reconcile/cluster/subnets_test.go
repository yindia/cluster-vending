package cluster

import (
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// The server-side apply itself (tags merged per key next to the network's,
// removed by applying no fields) is not covered here: the fake client
// round-trips applies through the provider's Go types, which send
// forProvider.region (no omitempty) as null and conflict. It was checked
// against the API server (kubectl apply --server-side --dry-run=server).

func TestManagedBy(t *testing.T) {
	fields := []metav1.ManagedFieldsEntry{{Manager: "cluster-vending"}, {Manager: subnetManager("c-1")}}
	if !managedBy(fields, subnetManager("c-1")) || managedBy(fields, subnetManager("c-2")) {
		t.Error("managedBy does not match the cluster's manager exactly")
	}
}

// The tags a cluster applies to a subnet never fight a tag the subnet
// already has with another value: an imported network's subnets often
// carry kubernetes.io/cluster/<name>=owned, and applying "shared" next to
// it is a field conflict that would stop every apply of the cluster.
func TestClusterSubnetTags(t *testing.T) {
	const cluster = "tf"
	clusterTag := "kubernetes.io/cluster/" + cluster
	tests := []struct {
		name     string
		existing map[string]*string
		want     map[string]any
	}{
		{"an untagged subnet", nil, map[string]any{clusterTag: "shared", "karpenter.sh/discovery": cluster}},
		{"the cluster's own tags, applied before", map[string]*string{clusterTag: ptr.To("shared"), "karpenter.sh/discovery": ptr.To(cluster)},
			map[string]any{clusterTag: "shared", "karpenter.sh/discovery": cluster}},
		{"the import's tag says owned", map[string]*string{clusterTag: ptr.To("owned")},
			map[string]any{"karpenter.sh/discovery": cluster}},
		{"another cluster's karpenter tag", map[string]*string{"karpenter.sh/discovery": ptr.To("other")},
			map[string]any{clusterTag: "shared"}},
		{"both are someone else's", map[string]*string{clusterTag: ptr.To("owned"), "karpenter.sh/discovery": ptr.To("other")},
			map[string]any{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := clusterSubnetTags(tt.existing, cluster)

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("tags = %v, want %v", got, tt.want)
			}
		})
	}
}
