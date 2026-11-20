package cluster

import (
	"context"
	"errors"
	"reflect"
	"testing"

	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
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

// Two clusters of one network tag karpenter.sh/discovery at once: the loser's
// apply is a field conflict. It is not an error: the cluster skips the tag
// (its Karpenter selects by kubernetes.io/cluster/<name>) and applies the rest.
func TestTagSubnetsSkipsTheDiscoveryTagTakenByAnotherCluster(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	subnet := &awsec2.Subnet{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-acme", Name: "net-private-a"}}
	var applied []map[string]any
	k := fake.NewClientBuilder().WithScheme(scheme).WithObjects(subnet).WithInterceptorFuncs(interceptor.Funcs{
		Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
			u := obj.(runtime.Unstructured)
			tags, _, _ := unstructured.NestedMap(u.UnstructuredContent(), "spec", "forProvider", "tags")
			applied = append(applied, tags)
			if _, ok := tags[crossplaneaws.TagKarpenterDiscovery]; ok {
				return apierrors.NewConflict(schema.GroupResource{Resource: "subnets"}, obj.(client.Object).GetName(), errors.New("field manager conflict"))
			}
			return nil
		}}).Build()
	s := New(reconcile.Deps{Kube: k})
	row := &model.Cluster{ID: "c-1", TenantID: "acme", Name: "tf", NetworkID: "n-1"}
	target := Target{Account: &model.CloudAccount{Namespace: "tenant-acme"}, SubnetObjects: []string{"net-private-a"}}
	if err := s.TagSubnets(t.Context(), row, target); err != nil {
		t.Fatalf("conflict on the discovery tag failed the cluster: %v", err)
	}
	if len(applied) != 2 {
		t.Fatalf("applies = %v, want the full set then the set without the discovery tag", applied)
	}
	if _, ok := applied[1][crossplaneaws.TagKarpenterDiscovery]; ok || applied[1]["kubernetes.io/cluster/tf"] != "shared" {
		t.Errorf("retry tags = %v", applied[1])
	}
}
