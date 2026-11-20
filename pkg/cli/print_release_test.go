package cli

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

const (
	alpha  = cloudv1.ReleaseChannel_RELEASE_CHANNEL_ALPHA
	beta   = cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA
	stable = cloudv1.ReleaseChannel_RELEASE_CHANNEL_STABLE
	tier0  = cloudv1.ClusterTier_CLUSTER_TIER_0
	tier1  = cloudv1.ClusterTier_CLUSTER_TIER_1
	tier2  = cloudv1.ClusterTier_CLUSTER_TIER_2
)

func policy() *cloudv1.RolloutPolicy {
	return &cloudv1.RolloutPolicy{MaxInProgress: 2, MaxFailed: 1, Soak: "24h0m0s", SoakPerTier: map[string]string{"Tier1": "48h0m0s"}}
}

func TestPrintReleases(t *testing.T) {
	withClock(t)
	eos := timestamppb.New(time.Date(2027, 9, 30, 0, 0, 0, 0, time.UTC))
	tag, err := structpb.NewStruct(map[string]any{"image": map[string]any{"tag": "v1.37.0"}})
	if err != nil {
		t.Fatal(err)
	}
	shipped := &cloudv1.Release{Name: "shipped-1aed928c", Serial: 7, Shipped: true, Valid: true, Channels: []cloudv1.ReleaseChannel{alpha, beta, stable},
		CreateTime: ago(48 * time.Hour), Kubernetes: []*cloudv1.ReleaseMinor{
			{Minor: "1.37", Channel: alpha, Default: true, EndOfSupport: eos,
				Components: []*cloudv1.ReleaseComponent{{Name: "cluster-autoscaler", Version: "9.60.0", Values: tag}}},
			{Minor: "1.34", UpgradeTo: "1.35", EndOfSupport: eos, Ending: true},
		}}
	old := &cloudv1.Release{Name: "shipped-f874cd2c", InvalidReason: "the minors are fewer than 3", CreateTime: ago(96 * time.Hour)}

	if got, want := printed(t, &cloudv1.ListReleasesResponse{Releases: []*cloudv1.Release{shipped, old}}, OutputTable),
		"NAME               SERIAL   SHIPPED   VALID   CHANNELS            PINNED   AGE\n"+
			"shipped-1aed928c   7        yes       True    alpha,beta,stable   0        2d\n"+
			"shipped-f874cd2c   <none>   no        False   <none>              0        4d\n"; got != want {
		t.Errorf("releases: got\n%s\nwant\n%s", got, want)
	}
	if got := printed(t, &cloudv1.ListReleasesResponse{Releases: []*cloudv1.Release{old}}, OutputWide); !strings.Contains(got,
		"the minors are fewer than 3") {
		t.Errorf("wide releases lack the reason:\n%s", got)
	}

	got := printed(t, &cloudv1.GetReleaseResponse{Release: shipped}, OutputWide)
	for _, want := range []string{
		"Name:       shipped-1aed928c\n",
		"  MINOR   CHANNEL   DEFAULT   UPGRADE-TO   END-OF-SUPPORT   ENDING\n",
		"  1.37    alpha     yes       <none>       2027-09-30       no\n",
		"  1.34    <none>    no        1.35         2027-09-30       yes: on no channel\n",
		"Components on 1.37:\n",
		`  cluster-autoscaler   9.60.0    {"image":{"tag":"v1.37.0"}}`,
		"Components on 1.34:\n  <none>\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("release get lacks %q:\n%s", want, got)
		}
	}
}

func TestPrintChannels(t *testing.T) {
	withClock(t)
	summary := &cloudv1.RolloutSummary{TargetMinor: "1.35", ReportOnly: true, Waiting: "would start 1 cluster (automatic upgrades are off)",
		WouldStart: []string{"tenant-acme/next"}, Tiers: []*cloudv1.RolloutTier{{Tier: tier2, Pending: 1, Succeeded: 1}}}
	channels := []*cloudv1.ReleaseChannelInfo{
		{Channel: alpha, Promotion: "Pinned", PinnedRelease: "shipped-1aed928c", Release: "shipped-1aed928c", KubernetesMinor: "1.37",
			Policy: policy(), CreateTime: ago(72 * time.Hour), Rollout: &cloudv1.RolloutSummary{TargetMinor: "1.37", Done: true}},
		{Channel: stable, Promotion: "Automatic", Release: "shipped-1aed928c", KubernetesMinor: "1.35", RolloutTier: tier2,
			Policy: policy(), CreateTime: ago(72 * time.Hour), Rollout: summary,
			History: []*cloudv1.ChannelHistoryEntry{{Release: "shipped-1aed928c", From: timestamppb.New(printNow.Add(-time.Hour)), Reason: "promoted"}}},
	}

	if got, want := printed(t, &cloudv1.ListReleaseChannelsResponse{Channels: channels}, OutputTable),
		"NAME     PROMOTION   RELEASE            KUBERNETES   ROLLING TIER                STATE     WAITING                                              PAUSED   AGE\n"+
			"alpha    Pinned      shipped-1aed928c   1.37         none (nothing to upgrade)   Done      <none>                                               no       3d\n"+
			"stable   Automatic   shipped-1aed928c   1.35         2                           Rolling   would start 1 cluster (automatic upgrades are off)   no       3d\n"; got != want {
		t.Errorf("channels: got\n%s\nwant\n%s", got, want)
	}
	wide := printed(t, &cloudv1.ListReleaseChannelsResponse{Channels: channels}, OutputWide)
	for _, want := range []string{"TIER2      TIER1   TIER0   POLICY", "1/2 done   -       -       maxInProgress 2, maxFailed 1, soak 24h (Tier1: 48h)"} {
		if !strings.Contains(wide, want) {
			t.Errorf("wide channels lack %q:\n%s", want, wide)
		}
	}
	got := printed(t, &cloudv1.GetReleaseChannelResponse{Channel: channels[1]}, OutputTable)
	for _, want := range []string{"Policy:           maxInProgress 2, maxFailed 1, soak 24h (Tier1: 48h)\n",
		"Rollout:          Rolling\n", "Would Start:      tenant-acme/next\n",
		"  RELEASE            FROM         TO    REASON\n", "  shipped-1aed928c   2026-11-"} {
		if !strings.Contains(got, want) {
			t.Errorf("channel get lacks %q:\n%s", want, got)
		}
	}
}

// A channel waiting out its release soak says what it takes next, when,
// and how to take it now.
func TestPrintChannelWaitingForARelease(t *testing.T) {
	withClock(t)
	beta := &cloudv1.ReleaseChannelInfo{Channel: cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA, Promotion: "Automatic",
		Release: "shipped-1aed928c", KubernetesMinor: "1.36", Policy: &cloudv1.RolloutPolicy{Soak: "24h0m0s", ReleaseSoak: "168h0m0s"},
		CreateTime: ago(72 * time.Hour), Rollout: &cloudv1.RolloutSummary{TargetMinor: "1.36", Done: true},
		Pending: &cloudv1.PendingRelease{Release: "2026-11-a", Since: timestamppb.New(printNow.Add(-24 * time.Hour)),
			Until: timestamppb.New(printNow.Add(6 * 24 * time.Hour))}}

	list := printed(t, &cloudv1.ListReleaseChannelsResponse{Channels: []*cloudv1.ReleaseChannelInfo{beta}}, OutputTable)
	get := printed(t, &cloudv1.GetReleaseChannelResponse{Channel: beta}, OutputTable)

	if !strings.Contains(list, "takes 2026-11-a in 6d") {
		t.Errorf("channel list lacks the pending release:\n%s", list)
	}
	for _, want := range []string{"Release Soak:", "168h", "Next Release:", "2026-11-a in 6d"} {
		if !strings.Contains(get, want) {
			t.Errorf("channel get lacks %q:\n%s", want, get)
		}
	}
}

func TestPrintRollouts(t *testing.T) {
	withClock(t)
	stableRollout := &cloudv1.Rollout{Channel: stable, Release: "shipped-1aed928c", RolloutTier: tier1, Policy: policy(),
		Summary: &cloudv1.RolloutSummary{TargetMinor: "1.36", ReportOnly: true, Waiting: "soak ends in 6h (Tier2)",
			Tiers: []*cloudv1.RolloutTier{{Tier: tier2, Succeeded: 4}, {Tier: tier1, InProgress: 1, Pending: 2}, {Tier: tier0, Pending: 2}}}}
	stableRollout.Members = []*cloudv1.RolloutMember{
		{Tier: tier2, TenantId: "acme", ClusterId: "id-dev", ClusterName: "dev-eks", CurrentVersion: "1.36", TargetVersion: "1.36",
			State: cloudv1.RolloutState_ROLLOUT_STATE_SUCCEEDED, Since: ago(48 * time.Hour)},
		{Tier: tier1, TenantId: "acme", ClusterId: "id-pay", ClusterName: "payments-eks", CurrentVersion: "1.35", TargetVersion: "1.36",
			State: cloudv1.RolloutState_ROLLOUT_STATE_IN_PROGRESS, Since: ago(12 * time.Minute)},
		{Tier: tier1, TenantId: "acme", ClusterId: "id-search", ClusterName: "search-eks", CurrentVersion: "1.35", TargetVersion: "1.36",
			State: cloudv1.RolloutState_ROLLOUT_STATE_BLOCKED, Gate: "MAINTENANCE_WINDOW", Reason: "the maintenance window opens Sat 02:00 UTC"},
	}
	info := &cloudv1.ReleaseChannelInfo{Channel: stable, Release: "shipped-1aed928c", KubernetesMinor: "1.36", Policy: policy(),
		ReleaseMoves: &cloudv1.ChangeRollout{Target: "2026-11-a", Waiting: "tier 1 soaks",
			Tiers:   []*cloudv1.RolloutTier{{Tier: tier2, Succeeded: 3}, {Tier: tier1, Pending: 4}},
			Blocked: []*cloudv1.BlockedMember{{Cluster: "tenant-acme/prod", Gate: "FROZEN", Reason: "the platform is frozen: deploy"}}}}
	got := printed(t, &cloudv1.GetReleaseChannelResponse{Channel: info, Rollout: stableRollout}, OutputTable)
	for _, want := range []string{
		"Name:             stable\n", "Kubernetes:       1.36\n", "Policy:           maxInProgress 2, maxFailed 1, soak 24h (Tier1: 48h)\n",
		"Rolling Tier:     1\n", "Rollout:          Rolling\n",
		"Release Moves:    2026-11-a: Rolling, 3/7 done; waiting: tier 1 soaks; 1 kept back\n",
		"  TIER   TENANT   CLUSTER        RUNS   TARGET   STATE        SINCE    REASON\n",
		"  2      acme     dev-eks        1.36   1.36     Succeeded    2d       <none>\n",
		"  1      acme     payments-eks   1.35   1.36     InProgress   12m      <none>\n",
		"  1      acme     search-eks     1.35   1.36     Blocked      <none>   the maintenance window opens Sat 02:00 UTC\n",
		"Release Moves Kept Back:\n", "  tenant-acme/prod   FROZEN   the platform is frozen: deploy\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("channel get lacks %q:\n%s", want, got)
		}
	}
	wide := printed(t, &cloudv1.GetReleaseChannelResponse{Channel: info, Rollout: stableRollout}, OutputWide)
	if !strings.Contains(wide, "id-search    MAINTENANCE_WINDOW") {
		t.Errorf("wide channel get lacks the ID and gate:\n%s", wide)
	}
}

func TestParseChannelAndTier(t *testing.T) {
	if c, err := parseChannel("--channel", " Stable "); err != nil || c != stable {
		t.Errorf("stable = %v, %v", c, err)
	}
	if _, err := parseChannel("--channel", "edge"); err == nil || !strings.Contains(err.Error(), "alpha, beta or stable") {
		t.Errorf("edge: %v", err)
	}
	for in, want := range map[string]cloudv1.ClusterTier{"": cloudv1.ClusterTier_CLUSTER_TIER_UNSPECIFIED, "2": tier2, "Tier0": tier0} {
		if got, err := parseTier(in); err != nil || got != want {
			t.Errorf("tier %q = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseTier("3"); err == nil {
		t.Error("tier 3 is refused")
	}
}
