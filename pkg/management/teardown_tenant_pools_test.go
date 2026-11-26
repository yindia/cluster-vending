package management_test

import (
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/yindia/cluster-vending/pkg/management"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// Tenants make their own NodePools and EC2NodeClasses, and the platform
// hands over the ones an earlier build made (it drops its label). A
// cluster's deletion still deletes every one of them, labelled or not:
// nothing Karpenter launched may outlive the cluster.
func TestTeardownListsEveryPoolLabelledOrNot(t *testing.T) {
	mapper := meta.NewDefaultRESTMapper(nil)
	for _, gvk := range []schema.GroupVersionKind{
		{Group: "karpenter.sh", Version: "v1", Kind: "NodePool"},
		{Group: "karpenter.k8s.aws", Version: "v1", Kind: "EC2NodeClass"},
	} {
		mapper.Add(gvk, meta.RESTScopeRoot)
	}
	platform := karpenter("karpenter.sh", "NodePool", "platform-pool")
	platform.SetLabels(map[string]string{"cloud.cluster-vending.io/managed-by": "platform"})
	objs := []client.Object{platform, karpenter("karpenter.sh", "NodePool", "gpu"), karpenter("karpenter.k8s.aws", "EC2NodeClass", "gpu")}
	c := fake.NewClientBuilder().WithScheme(teardownScheme(t)).WithRESTMapper(mapper).WithObjects(objs...).Build()

	got, err := management.TeardownOf(c).List(t.Context(), reconcile.KindNodePool, reconcile.KindEC2NodeClass)

	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"EC2NodeClass gpu", "NodePool gpu", "NodePool platform-pool"}; !slices.Equal(refsOf(got), want) {
		t.Errorf("listed %v, want %v", refsOf(got), want)
	}
}
