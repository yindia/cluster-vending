package rollout

import (
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/kube"
)

func TestReadNetworkCodeMembershipAcrossChannels(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	shared := networkWithCode("shared", cloud.ClusterTier0, "Pending", "new/shared")
	alphaOnly := networkWithCode("alpha-only", cloud.ClusterTier2, "Pending", "new/alpha")
	empty := networkWithCode("empty", cloud.ClusterTier1, "Pending", "new/empty")
	a, b, c := cluster("alpha", cloud.ClusterTier0, "1.36"), cluster("stable", cloud.ClusterTier0, "1.36"), cluster("dedicated", cloud.ClusterTier2, "1.36")
	a.Spec.ReleaseChannel = cloud.ReleaseChannelAlpha
	a.Spec.Network.NetworkRef.Name = "shared"
	b.Spec.Network.NetworkRef.Name = "shared"
	c.Spec.ReleaseChannel = cloud.ReleaseChannelAlpha
	c.Spec.Network.NetworkRef.Name = "alpha-only"
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&shared, &alphaOnly, &empty, &a, &b, &c).Build()
	for _, test := range []struct {
		channel string
		want    []string
	}{{"alpha", []string{"alpha-only", "empty", "shared"}}, {"stable", []string{"empty", "shared"}}, {"beta", []string{"empty"}}} {
		t.Run(test.channel, func(t *testing.T) {
			in, err := Read(t.Context(), reader, &cloud.ReleaseChannel{ObjectMeta: metav1.ObjectMeta{Name: test.channel}})
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, network := range in.Networks {
				names = append(names, network.Name)
			}
			slices.Sort(names)
			if !slices.Equal(names, test.want) {
				t.Fatalf("network memberships=%v want %v", names, test.want)
			}
		})
	}
}
