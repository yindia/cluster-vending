package admin

import (
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

var releaseNow = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC) // a Saturday

const shipped = "shipped-1aed928c"

// releaseWorld is a management cluster with the shipped release (1.37
// alpha, 1.36 beta, 1.35 stable, 1.34 on none), the three channels
// offering it, and objects.
func releaseWorld(t *testing.T, objects ...client.Object) *Service {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	eos := metav1.NewTime(time.Date(2027, 9, 30, 0, 0, 0, 0, time.UTC))
	rel := &cloudv1alpha1.Release{
		ObjectMeta: metav1.ObjectMeta{Name: shipped, Labels: map[string]string{cloudv1alpha1.LabelShipped: "true"},
			CreationTimestamp: metav1.NewTime(releaseNow.Add(-48 * time.Hour))},
		Spec: cloudv1alpha1.ReleaseSpec{Content: cloudv1alpha1.ReleaseContent{RenderVersion: 4, Kubernetes: []cloudv1alpha1.KubernetesRelease{
			{Minor: "1.37", Channel: "alpha", Default: true, EndOfSupport: eos,
				Components: []cloudv1alpha1.ComponentPin{{Name: "cluster-autoscaler", Version: "9.60.0",
					Values: &runtime.RawExtension{Raw: []byte(`{"image":{"tag":"v1.37.0"}}`)}}}},
			{Minor: "1.36", Channel: "beta", UpgradeTo: "1.37", EndOfSupport: eos},
			{Minor: "1.35", Channel: "stable", UpgradeTo: "1.36", EndOfSupport: eos},
			{Minor: "1.34", UpgradeTo: "1.35", EndOfSupport: eos},
		}}},
		Status: cloudv1alpha1.ReleaseStatus{Channels: []string{"alpha", "beta", "stable"},
			Conditions: []metav1.Condition{{Type: cloudv1alpha1.ReleaseConditionValid, Status: metav1.ConditionTrue, Reason: "Valid"}}},
	}
	old := &cloudv1alpha1.Release{
		ObjectMeta: metav1.ObjectMeta{Name: "shipped-f874cd2c", CreationTimestamp: metav1.NewTime(releaseNow.Add(-96 * time.Hour))},
		Spec: cloudv1alpha1.ReleaseSpec{Content: cloudv1alpha1.ReleaseContent{RenderVersion: 4,
			Kubernetes: []cloudv1alpha1.KubernetesRelease{{Minor: "1.37", EndOfSupport: eos}}}},
		Status: cloudv1alpha1.ReleaseStatus{Conditions: []metav1.Condition{{Type: cloudv1alpha1.ReleaseConditionValid,
			Status: metav1.ConditionFalse, Reason: "Invalid", Message: "the minors are fewer than 3"}}},
	}
	objs := []client.Object{rel, old}
	for i, name := range []string{"stable", "alpha", "beta"} {
		minor := map[string]string{"alpha": "1.37", "beta": "1.36", "stable": "1.35"}[name]
		ch := &cloudv1alpha1.ReleaseChannel{ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(releaseNow.Add(-time.Duration(i) * time.Hour))},
			Spec: cloudv1alpha1.ReleaseChannelSpec{Promotion: cloudv1alpha1.PromotionAutomatic,
				Policy: cloudv1alpha1.RolloutPolicy{MaxInProgress: 2, MaxFailed: 1, Soak: metav1.Duration{Duration: 24 * time.Hour},
					SoakPerTier: map[cloudv1alpha1.ClusterTier]metav1.Duration{cloudv1alpha1.ClusterTier1: {Duration: 48 * time.Hour}}}},
			Status: cloudv1alpha1.ReleaseChannelStatus{Release: shipped, KubernetesMinor: minor, RolloutTier: cloudv1alpha1.ClusterTier2,
				Rollout: &cloudv1alpha1.ChannelRollout{TargetMinor: minor, ReportOnly: true, Waiting: "would start 1 cluster (automatic upgrades are off)",
					WouldStart: []string{"tenant-acme/next"},
					Members:    []cloudv1alpha1.RolloutTierCount{{Tier: cloudv1alpha1.ClusterTier2, Pending: 1, Succeeded: 1}}},
				History: []cloudv1alpha1.ChannelHistoryEntry{{Release: shipped, From: metav1.NewTime(releaseNow.Add(-time.Hour)), Reason: "promoted"}}}}
		if name == "alpha" {
			ch.Spec.Promotion, ch.Spec.Release = cloudv1alpha1.PromotionPinned, shipped
		}
		objs = append(objs, ch)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objs, objects...)...).Build()
	s := NewService(reconcile.Deps{Kube: c})
	s.now = func() time.Time { return releaseNow }
	return s
}

func stableCluster(name string, tier cloudv1alpha1.ClusterTier, version string) *cloudv1alpha1.Cluster {
	scanned := metav1.NewTime(releaseNow.Add(-time.Hour))
	ready := metav1.NewTime(releaseNow.Add(-48 * time.Hour))
	return &cloudv1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant-acme",
			Labels: map[string]string{cloudstore.LabelID: "id-" + name, cloudstore.LabelTenant: "acme"}},
		Spec: cloudv1alpha1.ClusterSpec{ReleaseChannel: cloudv1alpha1.ReleaseChannelStable, Tier: tier,
			UpgradeMode:  cloudv1alpha1.UpgradeModeAutomatic,
			ControlPlane: &cloudv1alpha1.ControlPlaneSpec{KubernetesVersion: version}},
		Status: cloudv1alpha1.ClusterStatus{Phase: cloudv1alpha1.ClusterPhaseRunning, CurrentKubernetesVersion: version,
			Conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready", LastTransitionTime: ready},
				{Type: cloudv1alpha1.ConditionPlatformComponentsReady, Status: metav1.ConditionTrue, Reason: "Ready", LastTransitionTime: ready},
			},
			Deprecations: &cloudv1alpha1.ClusterDeprecations{TargetVersion: "1.35", ScannedAt: &scanned}},
	}
}

func TestListReleasesNewestFirst(t *testing.T) {
	s := releaseWorld(t)

	res, err := s.ListReleases(t.Context(), &cloudv1.ListReleasesRequest{})

	if err != nil {
		t.Fatal(err)
	}
	if len(res.GetReleases()) != 2 || res.GetReleases()[0].GetName() != shipped {
		t.Fatalf("releases = %v", res.GetReleases())
	}
	r := res.GetReleases()[0]
	if !r.GetShipped() || !r.GetValid() || len(r.GetChannels()) != 3 || len(r.GetKubernetes()) != 0 {
		t.Errorf("listed release = %v (no minors in a list)", r)
	}
	if old := res.GetReleases()[1]; old.GetValid() || old.GetInvalidReason() != "the minors are fewer than 3" {
		t.Errorf("invalid release = %v", old)
	}
	page, err := s.ListReleases(t.Context(), &cloudv1.ListReleasesRequest{PageSize: 1})
	if err != nil || len(page.GetReleases()) != 1 || page.GetNextPageToken() == "" {
		t.Fatalf("first page = %v, %v", page, err)
	}
	rest, err := s.ListReleases(t.Context(), &cloudv1.ListReleasesRequest{PageSize: 1, PageToken: page.GetNextPageToken()})
	if err != nil || len(rest.GetReleases()) != 1 || rest.GetReleases()[0].GetName() != "shipped-f874cd2c" || rest.GetNextPageToken() != "" {
		t.Errorf("second page = %v, %v", rest, err)
	}
	if _, err := s.ListReleases(t.Context(), &cloudv1.ListReleasesRequest{PageToken: "x"}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("bad token: %v", err)
	}
}

func TestGetReleaseShowsEachMinor(t *testing.T) {
	s := releaseWorld(t)

	res, err := s.GetRelease(t.Context(), &cloudv1.GetReleaseRequest{Name: shipped})

	if err != nil {
		t.Fatal(err)
	}
	minors := res.GetRelease().GetKubernetes()
	if len(minors) != 4 {
		t.Fatalf("minors = %v", minors)
	}
	if m := minors[0]; m.GetChannel() != cloudv1.ReleaseChannel_RELEASE_CHANNEL_ALPHA || !m.GetDefault() || m.GetEnding() {
		t.Errorf("1.37 = %v", m)
	}
	if c := minors[0].GetComponents()[0]; c.GetVersion() != "9.60.0" ||
		c.GetValues().GetFields()["image"].GetStructValue().GetFields()["tag"].GetStringValue() != "v1.37.0" {
		t.Errorf("1.37's autoscaler = %v", c)
	}
	if m := minors[3]; m.GetChannel() != cloudv1.ReleaseChannel_RELEASE_CHANNEL_UNSPECIFIED || !m.GetEnding() || m.GetUpgradeTo() != "1.35" {
		t.Errorf("1.34 is on no channel, so ending: %v", m)
	}
	if _, err := s.GetRelease(t.Context(), &cloudv1.GetReleaseRequest{Name: "shipped-00000000"}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("missing release: %v", err)
	}
}

func TestChannelsAlphaFirst(t *testing.T) {
	s := releaseWorld(t)

	res, err := s.ListReleaseChannels(t.Context(), &cloudv1.ListReleaseChannelsRequest{})

	if err != nil {
		t.Fatal(err)
	}
	want := []cloudv1.ReleaseChannel{cloudv1.ReleaseChannel_RELEASE_CHANNEL_ALPHA, cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA,
		cloudv1.ReleaseChannel_RELEASE_CHANNEL_STABLE}
	for i, ch := range res.GetChannels() {
		if ch.GetChannel() != want[i] {
			t.Errorf("channel %d = %v, want %v", i, ch.GetChannel(), want[i])
		}
	}
	alpha := res.GetChannels()[0]
	if alpha.GetPromotion() != "Pinned" || alpha.GetPinnedRelease() != shipped || alpha.GetKubernetesMinor() != "1.37" ||
		alpha.GetPolicy().GetSoak() != "24h0m0s" || alpha.GetPolicy().GetSoakPerTier()["Tier1"] != "48h0m0s" || len(alpha.GetHistory()) != 0 {
		t.Errorf("alpha = %v", alpha)
	}
	one, err := s.GetReleaseChannel(t.Context(), &cloudv1.GetReleaseChannelRequest{Channel: cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA})
	if err != nil || len(one.GetChannel().GetHistory()) != 1 || one.GetChannel().GetHistory()[0].GetReason() != "promoted" {
		t.Errorf("beta = %v, %v", one, err)
	}
	if _, err := s.GetReleaseChannel(t.Context(), &cloudv1.GetReleaseChannelRequest{}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("no channel: %v", err)
	}
}

func TestListRolloutsIsWhatTheControllerReported(t *testing.T) {
	s := releaseWorld(t)

	res, err := s.ListRollouts(t.Context(), &cloudv1.ListRolloutsRequest{})

	if err != nil {
		t.Fatal(err)
	}
	if len(res.GetRollouts()) != 3 {
		t.Fatalf("rollouts = %v", res.GetRollouts())
	}
	r := res.GetRollouts()[2]
	if r.GetChannel() != cloudv1.ReleaseChannel_RELEASE_CHANNEL_STABLE || r.GetRolloutTier() != cloudv1.ClusterTier_CLUSTER_TIER_2 ||
		r.GetSummary().GetTargetMinor() != "1.35" || !r.GetSummary().GetReportOnly() || len(r.GetMembers()) != 0 ||
		r.GetSummary().GetTiers()[0].GetPending() != 1 {
		t.Errorf("stable = %v", r)
	}
}

func TestGetRolloutPlansClusterByCluster(t *testing.T) {
	manual := stableCluster("manual", cloudv1alpha1.ClusterTier0, "1.34")
	manual.Spec.UpgradeMode = cloudv1alpha1.UpgradeModeManual
	beta := stableCluster("beta-one", cloudv1alpha1.ClusterTier2, "1.34")
	beta.Spec.ReleaseChannel = cloudv1alpha1.ReleaseChannelBeta
	s := releaseWorld(t, stableCluster("done", cloudv1alpha1.ClusterTier2, "1.35"),
		stableCluster("next", cloudv1alpha1.ClusterTier2, "1.34"), stableCluster("later", cloudv1alpha1.ClusterTier1, "1.34"),
		manual, beta)
	stable := cloudv1.ReleaseChannel_RELEASE_CHANNEL_STABLE

	res, err := s.GetRollout(t.Context(), &cloudv1.GetRolloutRequest{Channel: stable})

	if err != nil {
		t.Fatal(err)
	}
	members := res.GetRollout().GetMembers()
	names := []string{}
	for _, m := range members {
		names = append(names, m.GetClusterName())
	}
	if want := []string{"done", "next", "later", "manual"}; len(names) != len(want) || names[0] != want[0] || names[3] != want[3] {
		t.Fatalf("members = %v, want %v (tier 2 first; the beta cluster is not stable's)", names, want)
	}
	next := members[1]
	if next.GetState() != cloudv1.RolloutState_ROLLOUT_STATE_PENDING || next.GetCurrentVersion() != "1.34" ||
		next.GetTargetVersion() != "1.35" || next.GetTenantId() != "acme" || next.GetClusterId() != "id-next" ||
		next.GetTier() != cloudv1.ClusterTier_CLUSTER_TIER_2 {
		t.Errorf("next = %v", next)
	}
	if m := members[3]; m.GetState() != cloudv1.RolloutState_ROLLOUT_STATE_BLOCKED || m.GetGate() != "MANUAL" || m.GetReason() == "" {
		t.Errorf("manual = %v", m)
	}
	if done := members[0]; done.GetState() != cloudv1.RolloutState_ROLLOUT_STATE_SUCCEEDED || done.GetSince() == nil {
		t.Errorf("done = %v", done)
	}
	sum := res.GetRollout().GetSummary()
	if len(sum.GetWouldStart()) != 1 || sum.GetWouldStart()[0] != "tenant-acme/next" || !sum.GetReportOnly() {
		t.Errorf("summary = %v", sum)
	}

	tier1, err := s.GetRollout(t.Context(), &cloudv1.GetRolloutRequest{Channel: stable, Tier: cloudv1.ClusterTier_CLUSTER_TIER_1})
	if err != nil || len(tier1.GetRollout().GetMembers()) != 1 || tier1.GetRollout().GetMembers()[0].GetClusterName() != "later" {
		t.Errorf("tier 1 = %v, %v", tier1, err)
	}
	paged, err := s.GetRollout(t.Context(), &cloudv1.GetRolloutRequest{Channel: stable, PageSize: 3})
	if err != nil || len(paged.GetRollout().GetMembers()) != 3 || paged.GetNextPageToken() != "3" {
		t.Errorf("page = %v, %v", paged, err)
	}
}

func TestListReleaseHolds(t *testing.T) {
	past := metav1.NewTime(releaseNow.Add(-time.Hour))
	s := releaseWorld(t,
		&cloudv1alpha1.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: "freeze-prod"},
			Spec:   cloudv1alpha1.ReleaseHoldSpec{ClusterID: "id-prod", Reason: "quarter end"},
			Status: cloudv1alpha1.ReleaseHoldStatus{Namespace: "tenant-acme", Name: "prod-eks"}},
		&cloudv1alpha1.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: "a-lock"},
			Spec: cloudv1alpha1.ReleaseHoldSpec{ClusterID: "id-old", Release: shipped, Reason: "audit", Until: &past}})

	res, err := s.ListReleaseHolds(t.Context(), &cloudv1.ListReleaseHoldsRequest{})

	if err != nil {
		t.Fatal(err)
	}
	holds := res.GetHolds()
	if len(holds) != 2 || holds[0].GetName() != "a-lock" || holds[0].GetActive() || holds[0].GetRelease() != shipped {
		t.Fatalf("holds = %v", holds)
	}
	if h := holds[1]; !h.GetActive() || h.GetClusterName() != "prod-eks" || h.GetTenantNamespace() != "tenant-acme" || h.GetUntil() != nil {
		t.Errorf("freeze = %v", h)
	}
}
