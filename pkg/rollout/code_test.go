package rollout

import (
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/convert"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/upgrade"
)

func withCode(c cloudv1alpha1.Cluster, phase string) cloudv1alpha1.Cluster {
	c.Status.CodeChange = &cloudv1alpha1.CodeChange{Phase: phase, Summary: "2 objects change", Objects: 2}
	return c
}

// A code change rolls out like a release move: tier 2 first, a Manual
// cluster too, one frozen kept back; the rollout says the tier it is at.
func TestComputeCodeTierByTier(t *testing.T) {
	dev := withCode(cluster("dev", cloudv1alpha1.ClusterTier2, "1.36"), convert.CodeChangePending)
	manual := withCode(cluster("manual", cloudv1alpha1.ClusterTier2, "1.36"), convert.CodeChangePending)
	manual.Spec.UpgradeMode = cloudv1alpha1.UpgradeModeManual
	prod := withCode(cluster("prod", cloudv1alpha1.ClusterTier0, "1.36"), convert.CodeChangePending)
	done := cluster("done", cloudv1alpha1.ClusterTier1, "1.36")

	got := ComputeCode(input(dev, manual, prod, done))

	if !slices.Equal(got.Admitted, []string{"tenant-acme/dev", "tenant-acme/manual"}) || got.Tier != cloudv1alpha1.ClusterTier2 || got.Done {
		t.Errorf("tier 2 first, Manual included: %+v", got)
	}

	frozen := input(dev, prod)
	frozen.Holds = []cloudv1alpha1.ReleaseHold{{Spec: cloudv1alpha1.ReleaseHoldSpec{Platform: true, Reason: "deploy"}}}
	got = ComputeCode(frozen)
	if len(got.Admitted) != 0 || len(got.Blocked) != 2 || got.Blocked[0].Gate != string(upgrade.GateFrozen) ||
		!strings.Contains(got.Blocked[0].Message, "the platform is frozen: deploy") {
		t.Errorf("frozen: %+v", got)
	}

	applied := withCode(cluster("dev", cloudv1alpha1.ClusterTier2, "1.36"), convert.CodeChangeDone)
	finished := metav1.NewTime(now.Add(-2 * time.Hour))
	applied.Status.CodeChange.FinishTime = &finished
	got = ComputeCode(input(applied, prod))
	if !slices.Equal(got.Admitted, []string{"tenant-acme/prod"}) || got.Tier != cloudv1alpha1.ClusterTier0 {
		t.Errorf("tier 2 done and soaked, tier 0 next: %+v", got)
	}
	if got := ComputeCode(input(applied)); !got.Done || got.Tier != "" {
		t.Errorf("nothing pending: %+v", got)
	}
}

func channelAt(tier cloudv1alpha1.ClusterTier, done bool) *cloudv1alpha1.ReleaseChannel {
	return &cloudv1alpha1.ReleaseChannel{ObjectMeta: metav1.ObjectMeta{Name: "stable"},
		Status: cloudv1alpha1.ReleaseChannelStatus{CodeChanges: &cloudv1alpha1.ChannelReleaseMoves{Tier: tier, Done: done, Admitted: []string{"network:tenant-acme/net"}}}}
}

// A network's code change uses its explicit tier before its clusters.
// A cluster exception does not resume the shared network.
func TestNetworkCodeAdmitted(t *testing.T) {
	const networkID = "net-1"
	network := &cloudv1alpha1.Network{ObjectMeta: metav1.ObjectMeta{Name: "net", Namespace: "tenant-acme", Labels: map[string]string{cloudstore.LabelID: networkID}}, Spec: cloudv1alpha1.NetworkSpec{Tier: cloudv1alpha1.ClusterTier1}}
	clusters := []cloudv1alpha1.Cluster{cluster("dev", cloudv1alpha1.ClusterTier2, "1.36"), cluster("prod", cloudv1alpha1.ClusterTier1, "1.36")}
	platform := cloudv1alpha1.ReleaseHold{Spec: cloudv1alpha1.ReleaseHoldSpec{Platform: true, Reason: "deploy"}}
	tests := []struct {
		name    string
		channel *cloudv1alpha1.ReleaseChannel
		holds   []cloudv1alpha1.ReleaseHold
		want    bool
		why     string
	}{
		{"its tier", channelAt(cloudv1alpha1.ClusterTier1, false), nil, true, ""},
		{"a later tier", channelAt(cloudv1alpha1.ClusterTier0, false), nil, false, "reach tier 1"},
		{"done is not fresh admission", channelAt("", true), nil, false, "reach tier 1"},
		{"an earlier tier", channelAt(cloudv1alpha1.ClusterTier2, false), nil, false, "reach tier 1"},
		{"not planned", &cloudv1alpha1.ReleaseChannel{}, nil, false, "to be planned"},
		{"frozen", channelAt(cloudv1alpha1.ClusterTier1, false), []cloudv1alpha1.ReleaseHold{platform}, false, "the platform is frozen: deploy"},
		{"let through", channelAt(cloudv1alpha1.ClusterTier1, false), []cloudv1alpha1.ReleaseHold{{Spec: cloudv1alpha1.ReleaseHoldSpec{
			Platform: true, Reason: "deploy", Except: []string{networkID}}}}, true, ""},
		{"a cluster let through", channelAt(cloudv1alpha1.ClusterTier1, false), []cloudv1alpha1.ReleaseHold{{Spec: cloudv1alpha1.ReleaseHoldSpec{
			Platform: true, Reason: "deploy", Except: []string{"id-prod"}}}}, false, "the platform is frozen"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			channels := map[string]*cloudv1alpha1.ReleaseChannel{"stable": tt.channel}

			got, why := NetworkCodeAdmitted(network, clusters, channels, tt.holds, now)

			if got != tt.want || !strings.Contains(why, tt.why) {
				t.Errorf("admitted %v (%q), want %v (%q)", got, why, tt.want, tt.why)
			}
		})
	}
}

// A code change is the platform's own fix: a failed platform or tenant
// component does not hold it back (the change may be what repairs it), so
// a component broken by a render bug can always get the fixed render. The
// cluster's own readiness, freezes and the window still gate it.
func TestCodeChangeIsNotHeldByComponents(t *testing.T) {
	broken := withCode(cluster("dev", cloudv1alpha1.ClusterTier2, "1.36"), convert.CodeChangePending)
	broken.Status.PlatformComponents = []cloudv1alpha1.PlatformComponentStatus{{Name: "karpenter", Phase: "Ready"},
		{Name: "capsule", Phase: "Failed"}}

	got := ComputeCode(input(broken))

	if !slices.Equal(got.Admitted, []string{"tenant-acme/dev"}) {
		t.Fatalf("a failed component held the code change back: %+v", got)
	}

	notReady := broken
	notReady.Status.Conditions = slices.Clone(broken.Status.Conditions)
	notReady.Status.Conditions[0].Status = metav1.ConditionFalse
	notReady.Status.Phase = cloudv1alpha1.ClusterPhaseProvisioning
	if got := ComputeCode(input(notReady)); len(got.Admitted) != 0 || len(got.Blocked) != 1 ||
		got.Blocked[0].Gate != string(upgrade.GateNotReady) {
		t.Errorf("a cluster that is not Ready must still wait: %+v", got)
	}
}
