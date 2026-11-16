package management

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/kube"
)

func TestStaleAdmissionsAfterNetworkTierChange(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	network := &cloud.Network{ObjectMeta: metav1.ObjectMeta{Name: "net", Namespace: "tenant-acme", Labels: map[string]string{cloudstore.LabelID: "net-id"}}, Spec: cloud.NetworkSpec{Tier: cloud.ClusterTier0}}
	cluster := &cloud.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "tenant-acme"}, Spec: cloud.ClusterSpec{ReleaseChannel: cloud.ReleaseChannelStable, Network: cloud.ClusterNetwork{NetworkRef: cloud.LocalReference{Name: "net"}}}}
	plan := &cloud.ChannelReleaseMoves{Tier: cloud.ClusterTier2, Release: "r2", Admitted: []string{"tenant-acme/prod"}}
	channel := &cloud.ReleaseChannel{ObjectMeta: metav1.ObjectMeta{Name: "stable"}, Status: cloud.ReleaseChannelStatus{CodeChanges: plan, ReleaseMoves: plan}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(network, cluster, channel).Build()
	p := platformSource{c: c}
	if allowed, _, err := p.ClusterCodeAdmitted(t.Context(), "stable", "acme", "prod"); err != nil || allowed {
		t.Errorf("code admission after tier change: allowed=%v err=%v; want denied", allowed, err)
	}
	if allowed, _, err := p.MoveAdmitted(t.Context(), "stable", "r2", "acme", "prod"); err != nil || allowed {
		t.Errorf("release admission after tier change: allowed=%v err=%v; want denied", allowed, err)
	}
	network.Spec.Tier = cloud.ClusterTier2
	if err := c.Update(t.Context(), network); err != nil {
		t.Fatal(err)
	}
	if allowed, _, err := p.ClusterCodeAdmitted(t.Context(), "stable", "acme", "prod"); err != nil || !allowed {
		t.Fatalf("current code admission: %v %v", allowed, err)
	}
	if allowed, _, err := p.MoveAdmitted(t.Context(), "stable", "r2", "acme", "prod"); err != nil || !allowed {
		t.Fatalf("current release admission: %v %v", allowed, err)
	}
}

func TestEmptyNetworkRespectsOwnTier(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	network := &cloud.Network{ObjectMeta: metav1.ObjectMeta{Name: "net", Namespace: "tenant-acme", Labels: map[string]string{cloudstore.LabelID: "net-id"}}, Spec: cloud.NetworkSpec{Tier: cloud.ClusterTier0}}
	channel := &cloud.ReleaseChannel{ObjectMeta: metav1.ObjectMeta{Name: "stable"}, Status: cloud.ReleaseChannelStatus{CodeChanges: &cloud.ChannelReleaseMoves{Tier: cloud.ClusterTier2, Halted: true, Waiting: "failed canary"}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(network, channel).Build()
	p := platformSource{c: c}
	if allowed, _, err := p.NetworkCodeAdmitted(t.Context(), "acme", "net-id"); err != nil || allowed {
		t.Errorf("Tier0 empty network during halted Tier2 rollout: allowed=%v err=%v; want denied", allowed, err)
	}
	channel.Status.CodeChanges.Halted = false
	if err := c.Update(t.Context(), channel); err != nil {
		t.Fatal(err)
	}
	if allowed, _, err := p.NetworkCodeAdmitted(t.Context(), "acme", "net-id"); err != nil || allowed {
		t.Fatalf("Tier0 admitted during Tier2 rollout: %v %v", allowed, err)
	}
	channel.Status.CodeChanges.Tier = cloud.ClusterTier0
	if err := c.Update(t.Context(), channel); err != nil {
		t.Fatal(err)
	}
	if allowed, _, err := p.NetworkCodeAdmitted(t.Context(), "acme", "net-id"); err != nil || !allowed {
		t.Fatalf("Tier0 not admitted when reached: %v %v", allowed, err)
	}
}
