package tenant

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

func dashboardReleases(t *testing.T) *TenantService {
	t.Helper()
	s, _ := dashboardWorld(t,
		&cloud.Release{ObjectMeta: metav1.ObjectMeta{Name: "2026-11-a"},
			Spec:   cloud.ReleaseSpec{Content: cloud.ReleaseContent{Kubernetes: []cloud.KubernetesRelease{{Minor: "1.35", Channel: "stable", EndOfSupport: metav1.NewTime(time.Now().Add(365 * 24 * time.Hour))}}}},
			Status: cloud.ReleaseStatus{Channels: []string{"stable"}, Holds: []string{"foreign-private-hold"}, Conditions: []metav1.Condition{{Type: cloud.ReleaseConditionValid, Status: metav1.ConditionTrue, Reason: "Valid"}}}},
		&cloud.Release{ObjectMeta: metav1.ObjectMeta{Name: "2026-10-a"}, Status: cloud.ReleaseStatus{Holds: []string{"foreign-older-hold"}}},
		&cloud.ReleaseChannel{ObjectMeta: metav1.ObjectMeta{Name: "stable"},
			Spec: cloud.ReleaseChannelSpec{Promotion: cloud.PromotionPinned, Release: "2026-11-a", Policy: cloud.RolloutPolicy{MaxInProgress: 1, MaxFailed: 1}},
			Status: cloud.ReleaseChannelStatus{Release: "2026-11-a", KubernetesMinor: "1.35", RolloutTier: cloud.ClusterTier2,
				Rollout:      &cloud.ChannelRollout{TargetMinor: "1.35", Waiting: "foreign-private-global-reason", WouldStart: []string{"tenant-foreign-tenant/foreign-private-cluster"}},
				Conditions:   []metav1.Condition{{Type: "Offering", Status: metav1.ConditionFalse, Reason: "Blocked", Message: "foreign-private-condition"}},
				History:      []cloud.ChannelHistoryEntry{{Release: "2026-11-a", From: metav1.Now(), Reason: "foreign-private-history"}},
				ReleaseMoves: &cloud.ChannelReleaseMoves{Release: "2026-11-a", Admitted: []string{"tenant-foreign-tenant/foreign-private-cluster"}, Waiting: "foreign-private-move"},
				CodeChanges:  &cloud.ChannelReleaseMoves{Waiting: "foreign-private-code"},
			}},
		&cloud.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: reconcile.FreezeName}, Spec: cloud.ReleaseHoldSpec{Platform: true, Reason: "foreign-private-freeze", By: "foreign-private-operator"}},
	)
	return s
}

func TestDashboardViewerReadsReleaseCatalogAndTenantRollout(t *testing.T) {
	s := dashboardReleases(t)
	ctx := dashboardIdentity(t)
	first, err := s.ListReleases(ctx, &v1.TenantListReleasesRequest{TenantId: "acme", PageSize: 1})
	if err != nil || len(first.GetReleases()) != 1 || first.NextPageToken == "" {
		t.Fatalf("list: %v %v", first, err)
	}
	second, err := s.ListReleases(ctx, &v1.TenantListReleasesRequest{TenantId: "acme", PageSize: 1, PageToken: first.NextPageToken})
	if err != nil || len(second.GetReleases()) != 1 || second.NextPageToken != "" {
		t.Fatalf("next: %v %v", second, err)
	}
	release, err := s.GetRelease(ctx, &v1.TenantGetReleaseRequest{TenantId: "acme", Name: "2026-11-a"})
	if err != nil || len(release.GetRelease().GetKubernetes()) != 1 || len(release.GetRelease().GetHolds()) != 0 {
		t.Fatalf("detail: %v %v", release, err)
	}
	channels, err := s.ListReleaseChannels(ctx, &v1.TenantListReleaseChannelsRequest{TenantId: "acme"})
	if err != nil || len(channels.GetChannels()) != 1 {
		t.Fatalf("channels: %v %v", channels, err)
	}
	for _, channel := range channels.Channels {
		if channel.Rollout != nil || channel.ReleaseMoves != nil || channel.CodeChanges != nil || len(channel.Conditions) != 0 || channel.RolloutTier != 0 {
			t.Fatalf("global channel status survived: %v", channel)
		}
	}
	result, err := s.GetReleaseChannel(ctx, &v1.TenantGetReleaseChannelRequest{TenantId: "acme", Channel: v1.ReleaseChannel_RELEASE_CHANNEL_STABLE})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.GetRollout().GetMembers()) != 1 || result.Rollout.Members[0].TenantId != "acme" || result.Rollout.Members[0].ClusterId != dashboardCluster {
		t.Fatalf("tenant members: %v", result.Rollout)
	}
	if result.Rollout.Summary != nil || result.Rollout.Plan != nil || result.Freeze != nil || result.Rollout.RolloutTier != 0 {
		t.Fatalf("global progress survived: %v", result)
	}
	for _, value := range []proto.Message{first, second, release, channels, result} {
		encoded, err := protojson.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "foreign") || strings.Contains(string(encoded), dashboardForeignCluster) {
			t.Fatalf("foreign metadata leaked: %s", encoded)
		}
	}
}

func TestDashboardSanitizersDoNotCopyGlobalProgress(t *testing.T) {
	rollout := dashboardRollout(&v1.Rollout{
		Summary: &v1.RolloutSummary{WouldStart: []string{"foreign-private"}, Waiting: "foreign-private", Tiers: []*v1.RolloutTier{{Pending: 999}}},
		Plan:    &v1.RolloutPlan{Batches: 999},
		Members: []*v1.RolloutMember{{TenantId: "acme", ClusterId: dashboardCluster, Reason: "foreign-private hold"}, {TenantId: "foreign-tenant", ClusterId: dashboardForeignCluster}},
	}, "acme")
	if len(rollout.Members) != 1 || rollout.Members[0].Reason != "" || rollout.Summary != nil || rollout.Plan != nil {
		t.Fatalf("unsafe rollout: %v", rollout)
	}
	selected := dashboardSelection{tenant: "acme", target: &v1.FreezeTarget{Scope: &v1.FreezeTarget_TenantId{TenantId: "acme"}}}
	status := dashboardSafeStatus(&v1.FreezeStatus{Target: &v1.FreezeTarget{Scope: &v1.FreezeTarget_Platform{Platform: true}},
		Scopes: []*v1.FreezeScope{{TenantId: "foreign-tenant", Reason: "foreign-private"}}, Exceptions: []string{dashboardForeignCluster},
		Freeze: &v1.Freeze{Scope: "platform", By: "foreign-private", Reason: "foreign-private"}, PlatformFrozen: true,
	}, selected)
	encoded, _ := protojson.Marshal(status)
	if strings.Contains(string(encoded), "foreign") || len(status.Scopes) != 0 || len(status.Exceptions) != 0 || status.Target.GetTenantId() != "acme" {
		t.Fatalf("unsafe freeze: %s", encoded)
	}
}
