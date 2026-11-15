package cli

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// AdminReleaseList calls AdminService.ListReleases: the platform's
// releases, newest first.
func AdminReleaseList(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	return call(ctx, &cloudv1.ListReleasesRequest{PageSize: o.PageSize, PageToken: o.PageToken}, c.Admin.ListReleases)
}

// AdminReleaseGet calls AdminService.GetRelease for the release --id.
func AdminReleaseGet(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	return call(ctx, &cloudv1.GetReleaseRequest{Name: o.ID}, c.Admin.GetRelease)
}

// AdminChannelList calls AdminService.ListReleaseChannels.
func AdminChannelList(ctx context.Context, c Clients, _ Options) (proto.Message, error) {
	return call(ctx, &cloudv1.ListReleaseChannelsRequest{}, c.Admin.ListReleaseChannels)
}

// AdminChannelGet calls AdminService.GetReleaseChannel for the channel
// --id (alpha, beta or stable): the channel and its rollout planned now.
func AdminChannelGet(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	ch, err := parseChannel("--id", o.ID)
	if err != nil {
		return nil, err
	}
	return call(ctx, &cloudv1.GetReleaseChannelRequest{Channel: ch}, c.Admin.GetReleaseChannel)
}

// parseChannel is a channel's name (alpha, beta, stable) as the contract's
// enum; flag names the flag it came from, for the error.
func parseChannel(flag, name string) (cloudv1.ReleaseChannel, error) {
	v, ok := cloudv1.ReleaseChannel_value["RELEASE_CHANNEL_"+strings.ToUpper(strings.TrimSpace(name))]
	if !ok || v == 0 {
		return 0, fmt.Errorf("%s is required: alpha, beta or stable", flag)
	}
	return cloudv1.ReleaseChannel(v), nil
}

// parseTier is --tier (0, 1, 2, or tier0..tier2) as the contract's enum;
// empty is every tier.
func parseTier(s string) (cloudv1.ClusterTier, error) {
	s = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "tier")
	if s == "" {
		return cloudv1.ClusterTier_CLUSTER_TIER_UNSPECIFIED, nil
	}
	v, ok := cloudv1.ClusterTier_value["CLUSTER_TIER_"+s]
	if !ok || v == 0 {
		return 0, fmt.Errorf("--tier must be 0, 1 or 2, got %q", s)
	}
	return cloudv1.ClusterTier(v), nil
}
