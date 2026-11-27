package cli

import (
	"context"
	"fmt"
	"io"
	"slices"

	"google.golang.org/protobuf/proto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// offeredMinor is one Kubernetes minor a channel's release offers.
type offeredMinor struct {
	release string
	minor   *cloudv1.ReleaseMinor
}

// ClusterVersions is cluster versions: the Kubernetes minors the tenant's
// clusters can run, from the releases the channels offer now
// (TenantService.ListReleaseChannels and GetRelease). Each channel gives
// the minor its release puts on it; a minor on no channel is extended
// support (Manual clusters only). --channel shows every minor of that
// channel's release.
func ClusterVersions(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	channels, err := versionChannels(ctx, c, o)
	if err != nil {
		return nil, err
	}
	var minors []offeredMinor
	seen := map[string]bool{}
	for _, ch := range channels {
		if ch.GetRelease() == "" {
			continue
		}
		res, err := call(ctx, &cloudv1.TenantGetReleaseRequest{TenantId: o.Tenant, Name: ch.GetRelease()}, c.Tenant.GetRelease)
		if err != nil {
			return nil, fmt.Errorf("release %s of channel %s: %w", ch.GetRelease(), channelWord(ch.GetChannel()), err)
		}
		for _, m := range res.(*cloudv1.GetReleaseResponse).GetRelease().GetKubernetes() {
			mine := m.GetChannel() == ch.GetChannel() || m.GetChannel() == cloudv1.ReleaseChannel_RELEASE_CHANNEL_UNSPECIFIED
			if (o.Channel == "" && !mine) || seen[m.GetMinor()] {
				continue
			}
			seen[m.GetMinor()] = true
			minors = append(minors, offeredMinor{release: ch.GetRelease(), minor: m})
		}
	}
	slices.SortStableFunc(minors, func(a, b offeredMinor) int { return compareMinors(b.minor.GetMinor(), a.minor.GetMinor()) })
	if !isTable(o.Output) {
		out := &cloudv1.Release{}
		for _, m := range minors {
			out.Kubernetes = append(out.Kubernetes, m.minor)
		}
		return out, nil
	}
	return nil, printVersions(o.Out, minors)
}

// versionChannels is every channel, or --channel's.
func versionChannels(ctx context.Context, c Clients, o Options) ([]*cloudv1.ReleaseChannelInfo, error) {
	if o.Channel == "" {
		res, err := call(ctx, &cloudv1.TenantListReleaseChannelsRequest{TenantId: o.Tenant}, c.Tenant.ListReleaseChannels)
		if err != nil {
			return nil, err
		}
		return res.(*cloudv1.ListReleaseChannelsResponse).GetChannels(), nil
	}
	ch, err := parseChannel("--channel", o.Channel)
	if err != nil {
		return nil, err
	}
	res, err := call(ctx, &cloudv1.TenantGetReleaseChannelRequest{TenantId: o.Tenant, Channel: ch}, c.Tenant.GetReleaseChannel)
	if err != nil {
		return nil, err
	}
	return []*cloudv1.ReleaseChannelInfo{res.(*cloudv1.GetReleaseChannelResponse).GetChannel()}, nil
}

func printVersions(w io.Writer, minors []offeredMinor) error {
	if len(minors) == 0 {
		_, err := fmt.Fprintln(w, "No Kubernetes version is offered: no channel offers a release yet.")
		return err
	}
	rows := make([][]string, 0, len(minors))
	for _, m := range minors {
		rows = append(rows, append(minorRow(m.minor), supportWord(m.minor), m.release))
	}
	return table(w, append(slices.Clone(minorHeader), "SUPPORT", "RELEASE"), rows)
}

// supportWord is a minor's support: standard on a channel; extended when
// on none (Manual clusters only), ending when its end of support nears.
func supportWord(m *cloudv1.ReleaseMinor) string {
	if m.GetChannel() != cloudv1.ReleaseChannel_RELEASE_CHANNEL_UNSPECIFIED {
		return "standard"
	}
	return "extended (on no channel: Manual clusters only)"
}

// compareMinors orders "1.9" before "1.10"; unparsable minors compare as text.
func compareMinors(a, b string) int {
	var am, an, bm, bn int
	_, errA := fmt.Sscanf(a, "%d.%d", &am, &an)
	_, errB := fmt.Sscanf(b, "%d.%d", &bm, &bn)
	switch {
	case errA != nil || errB != nil:
		return compareText(a, b)
	case am != bm:
		return am - bm
	default:
		return an - bn
	}
}

func compareText(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
