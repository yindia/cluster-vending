package tenant

import (
	"context"

	"google.golang.org/protobuf/proto"

	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/server/admin"
)

func (s *TenantService) ListReleases(ctx context.Context, req *v1.TenantListReleasesRequest) (*v1.ListReleasesResponse, error) {
	if err := dashboardAccess(req); err != nil {
		return nil, err
	}
	out, err := admin.NewService(s.deps).ListReleases(ctx, &v1.ListReleasesRequest{PageSize: req.GetPageSize(), PageToken: req.GetPageToken()})
	if err != nil {
		return nil, err
	}
	for _, release := range out.Releases {
		release.Holds = nil
	}
	return out, nil
}

func (s *TenantService) GetRelease(ctx context.Context, req *v1.TenantGetReleaseRequest) (*v1.GetReleaseResponse, error) {
	if err := dashboardAccess(req); err != nil {
		return nil, err
	}
	out, err := admin.NewService(s.deps).GetRelease(ctx, &v1.GetReleaseRequest{Name: req.GetName()})
	if err != nil {
		return nil, err
	}
	if out.Release != nil {
		out.Release.Holds = nil
	}
	return out, nil
}

// Published channel settings are shared configuration. Aggregate rollout
// progress, conditions, admission lists and gate reasons belong to the operator
// view, and cannot be interpreted as this tenant's observations. Leave them
// absent rather than misrepresenting global totals as tenant progress.
func dashboardChannel(in *v1.ReleaseChannelInfo) *v1.ReleaseChannelInfo {
	if in == nil {
		return nil
	}
	out := &v1.ReleaseChannelInfo{Channel: in.Channel, Promotion: in.Promotion, PinnedRelease: in.PinnedRelease,
		Release: in.Release, KubernetesMinor: in.KubernetesMinor, Paused: in.Paused,
		Policy: in.Policy, CreateTime: in.CreateTime, Pending: in.Pending}
	for _, history := range in.History {
		out.History = append(out.History, &v1.ChannelHistoryEntry{Release: history.Release, From: history.From, To: history.To})
	}
	return out
}

func (s *TenantService) ListReleaseChannels(ctx context.Context, req *v1.TenantListReleaseChannelsRequest) (*v1.ListReleaseChannelsResponse, error) {
	if err := dashboardAccess(req); err != nil {
		return nil, err
	}
	result, err := admin.NewService(s.deps).ListReleaseChannels(ctx, &v1.ListReleaseChannelsRequest{})
	if err != nil {
		return nil, err
	}
	out := &v1.ListReleaseChannelsResponse{}
	for _, channel := range result.Channels {
		out.Channels = append(out.Channels, dashboardChannel(channel))
	}
	return out, nil
}

func dashboardRollout(in *v1.Rollout, tenant string) *v1.Rollout {
	if in == nil {
		return nil
	}
	out := &v1.Rollout{Channel: in.Channel, Release: in.Release, Policy: in.Policy, Paused: in.Paused}
	for _, member := range in.Members {
		if member.GetTenantId() != tenant {
			continue
		}
		own := proto.Clone(member).(*v1.RolloutMember)
		// Even an own member's gate can quote a platform hold or another
		// tenant's failed rollout. The tenant's cluster status provides detail.
		own.Reason = ""
		out.Members = append(out.Members, own)
	}
	return out
}

func (s *TenantService) GetReleaseChannel(ctx context.Context, req *v1.TenantGetReleaseChannelRequest) (*v1.GetReleaseChannelResponse, error) {
	if err := dashboardAccess(req); err != nil {
		return nil, err
	}
	result, err := admin.NewService(s.deps).GetReleaseChannel(ctx, &v1.GetReleaseChannelRequest{Channel: req.GetChannel()})
	if err != nil {
		return nil, err
	}
	// Global freeze identity/reasons and global plans are intentionally absent.
	// TenantService.GetFreeze is the tenant-safe source of freeze observations.
	return &v1.GetReleaseChannelResponse{Channel: dashboardChannel(result.Channel), Rollout: dashboardRollout(result.Rollout, req.GetTenantId())}, nil
}
