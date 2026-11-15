package rollout

import (
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/convert"
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
		Status: cloudv1alpha1.ReleaseChannelStatus{CodeChanges: &cloudv1alpha1.ChannelReleaseMoves{Tier: tier, Done: done}}}
}

// A network's code change goes in the tier of its most careful cluster,
// before its clusters of that tier: not while the channel's code rollout
// is at an earlier tier; a freeze holds it unless it, or one of its
// clusters, is let through.
func TestNetworkCodeAdmitted(t *testing.T) {
	const network = "net-1"
	clusters := []cloudv1alpha1.Cluster{cluster("dev", cloudv1alpha1.ClusterTier2, "1.36"), cluster("prod", cloudv1alpha1.ClusterTier1, "1.36")}
	if got := NetworkTier(clusters); got != upgrade.Tier1 {
		t.Fatalf("NetworkTier = %v, want the most careful (1)", got)
	}
	if got := NetworkTier(nil); got != upgrade.Tier2 {
		t.Fatalf("NetworkTier without clusters = %v, want 2", got)
	}
	platform := cloudv1alpha1.ReleaseHold{Spec: cloudv1alpha1.ReleaseHoldSpec{Platform: true, Reason: "deploy"}}
	tests := []struct {
		name    string
		channel *cloudv1alpha1.ReleaseChannel
		holds   []cloudv1alpha1.ReleaseHold
		want    bool
		why     string
	}{
		{"its tier", channelAt(cloudv1alpha1.ClusterTier1, false), nil, true, ""},
		{"a later tier", channelAt(cloudv1alpha1.ClusterTier0, false), nil, true, ""},
		{"done", channelAt("", true), nil, true, ""},
		{"an earlier tier", channelAt(cloudv1alpha1.ClusterTier2, false), nil, false, "reach tier 1"},
		{"not planned", &cloudv1alpha1.ReleaseChannel{}, nil, false, "to be planned"},
		{"frozen", channelAt(cloudv1alpha1.ClusterTier1, false), []cloudv1alpha1.ReleaseHold{platform}, false, "the platform is frozen: deploy"},
		{"let through", channelAt(cloudv1alpha1.ClusterTier1, false), []cloudv1alpha1.ReleaseHold{{Spec: cloudv1alpha1.ReleaseHoldSpec{
			Platform: true, Reason: "deploy", Except: []string{network}}}}, true, ""},
		{"a cluster let through", channelAt(cloudv1alpha1.ClusterTier1, false), []cloudv1alpha1.ReleaseHold{{Spec: cloudv1alpha1.ReleaseHoldSpec{
			Platform: true, Reason: "deploy", Except: []string{"id-prod"}}}}, true, ""},
		{"its tenant frozen", channelAt(cloudv1alpha1.ClusterTier1, false), []cloudv1alpha1.ReleaseHold{{Spec: cloudv1alpha1.ReleaseHoldSpec{
			TenantID: "acme", Reason: "audit"}}}, false, "tenant acme is frozen"},
		{"a cluster's channel frozen", channelAt(cloudv1alpha1.ClusterTier1, false), []cloudv1alpha1.ReleaseHold{{Spec: cloudv1alpha1.ReleaseHoldSpec{
			Channel: "stable", Tier: cloudv1alpha1.ClusterTier1, Reason: "quarter end"}}}, false, "channel stable tier 1 is frozen"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			channels := map[string]*cloudv1alpha1.ReleaseChannel{"stable": tt.channel}

			got, why := NetworkCodeAdmitted(network, "acme", clusters, channels, tt.holds, now)

			if got != tt.want || !strings.Contains(why, tt.why) {
				t.Errorf("admitted %v (%q), want %v (%q)", got, why, tt.want, tt.why)
			}
		})
	}
}
