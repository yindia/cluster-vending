package rollout

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/upgrade"
)

func TestClustersInheritTheirNetworksTier(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	first, second := cluster("first", cloudv1alpha1.ClusterTier2, "1.35"), cluster("second", cloudv1alpha1.ClusterTier0, "1.35")
	first.Spec.Network.NetworkRef.Name, second.Spec.Network.NetworkRef.Name = "shared", "shared"
	network := &cloudv1alpha1.Network{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "tenant-acme"}, Spec: cloudv1alpha1.NetworkSpec{Tier: cloudv1alpha1.ClusterTier1}}
	fixture := input(first, second)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(network, &first, &second, fixture.Release).Build()
	read := func(want upgrade.Tier) {
		t.Helper()
		snapshot, err := Read(t.Context(), kubeClient, fixture.Channel)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.Now = now
		result := Compute(snapshot)
		if len(result.Rows) != 2 {
			t.Fatalf("rows: %+v", result)
		}
		for _, row := range result.Rows {
			if row.Tier != want {
				t.Fatalf("%s inherited tier %d; want %d", row.Name, row.Tier, want)
			}
		}
	}
	read(upgrade.Tier1)
	if err := kubeClient.Get(t.Context(), client.ObjectKeyFromObject(network), network); err != nil {
		t.Fatal(err)
	}
	network.Spec.Tier = cloudv1alpha1.ClusterTier0
	if err := kubeClient.Update(t.Context(), network); err != nil {
		t.Fatal(err)
	}
	read(upgrade.Tier0)
}

func TestMissingNetworkDoesNotBlockAnotherChannel(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	healthy := cluster("healthy", cloudv1alpha1.ClusterTier0, "1.35")
	healthy.Spec.Network.NetworkRef.Name = "shared"
	broken := cluster("broken", cloudv1alpha1.ClusterTier0, "1.35")
	broken.Spec.ReleaseChannel = cloudv1alpha1.ReleaseChannelAlpha
	broken.Spec.Network.NetworkRef.Name = "missing"
	network := &cloudv1alpha1.Network{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: healthy.Namespace}}
	fixture := input(healthy, broken)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(network, &healthy, &broken, fixture.Release).Build()
	snapshot, err := Read(t.Context(), c, fixture.Channel)
	if err != nil || len(snapshot.Clusters) != 1 {
		t.Fatalf("unrelated broken network blocked stable: %+v, %v", snapshot.Clusters, err)
	}
	fixture.Channel.Name = "alpha"
	if _, err := Read(t.Context(), c, fixture.Channel); err == nil {
		t.Fatal("the affected channel must fail closed for a missing network")
	}
}
