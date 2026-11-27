package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	cloud "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// versionsTenant offers alpha on release "new" (1.37 alpha, 1.36 beta)
// and beta and stable on "old" (1.36 beta, 1.35 stable, 1.34 on no
// channel: extended support).
type versionsTenant struct {
	tenantRecorder
	gets []string
}

var versionChannelInfos = []*cloud.ReleaseChannelInfo{
	{Channel: cloud.ReleaseChannel_RELEASE_CHANNEL_ALPHA, Release: "new"},
	{Channel: cloud.ReleaseChannel_RELEASE_CHANNEL_BETA, Release: "old"},
	{Channel: cloud.ReleaseChannel_RELEASE_CHANNEL_STABLE, Release: "old"},
}

func (v *versionsTenant) ListReleaseChannels(context.Context, *cloud.TenantListReleaseChannelsRequest) (*cloud.ListReleaseChannelsResponse, error) {
	return &cloud.ListReleaseChannelsResponse{Channels: versionChannelInfos}, nil
}

func (v *versionsTenant) GetReleaseChannel(_ context.Context, req *cloud.TenantGetReleaseChannelRequest) (*cloud.GetReleaseChannelResponse, error) {
	for _, c := range versionChannelInfos {
		if c.GetChannel() == req.GetChannel() {
			return &cloud.GetReleaseChannelResponse{Channel: c}, nil
		}
	}
	return &cloud.GetReleaseChannelResponse{}, nil
}

func (v *versionsTenant) GetRelease(_ context.Context, req *cloud.TenantGetReleaseRequest) (*cloud.GetReleaseResponse, error) {
	v.gets = append(v.gets, req.GetName())
	minor := func(m string, ch cloud.ReleaseChannel, def bool) *cloud.ReleaseMinor {
		return &cloud.ReleaseMinor{Minor: m, Channel: ch, Default: def}
	}
	if req.GetName() == "new" {
		return &cloud.GetReleaseResponse{Release: &cloud.Release{Name: "new", Kubernetes: []*cloud.ReleaseMinor{
			minor("1.37", cloud.ReleaseChannel_RELEASE_CHANNEL_ALPHA, false), minor("1.36", cloud.ReleaseChannel_RELEASE_CHANNEL_BETA, false)}}}, nil
	}
	return &cloud.GetReleaseResponse{Release: &cloud.Release{Name: "old", Kubernetes: []*cloud.ReleaseMinor{
		minor("1.36", cloud.ReleaseChannel_RELEASE_CHANNEL_BETA, false), minor("1.35", cloud.ReleaseChannel_RELEASE_CHANNEL_STABLE, true),
		minor("1.34", cloud.ReleaseChannel_RELEASE_CHANNEL_UNSPECIFIED, false)}}}, nil
}

func TestClusterVersionsPerChannel(t *testing.T) {
	var out bytes.Buffer
	if err := Run(t.Context(), ClusterVersions, Clients{Tenant: &versionsTenant{}}, Options{Tenant: "acme"}, &out, OutputTable); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 5 || !strings.HasPrefix(lines[0], "MINOR") {
		t.Fatalf("want a header and 4 minors:\n%s", out.String())
	}
	for i, want := range []string{"1.37 ", "1.36 ", "1.35 ", "1.34 "} {
		if !strings.HasPrefix(lines[i+1], want) {
			t.Errorf("line %d = %q, want %q first", i+1, lines[i+1], want)
		}
	}
	if !strings.Contains(lines[2], "beta") || !strings.Contains(lines[2], "old") {
		t.Errorf("1.36 must come from beta's release: %q", lines[2])
	}
	if !strings.Contains(lines[3], "yes") || !strings.Contains(lines[4], "extended") {
		t.Errorf("default or extended support not marked:\n%s", out.String())
	}
}

func TestClusterVersionsOneChannel(t *testing.T) {
	var out bytes.Buffer
	api := &versionsTenant{}
	if err := Run(t.Context(), ClusterVersions, Clients{Tenant: api}, Options{Tenant: "acme", Channel: "alpha"}, &out, OutputTable); err != nil {
		t.Fatal(err)
	}
	if len(api.gets) != 1 || api.gets[0] != "new" || !strings.Contains(out.String(), "1.37") || strings.Contains(out.String(), "1.35") {
		t.Fatalf("gets %v:\n%s", api.gets, out.String())
	}
	if err := Run(t.Context(), ClusterVersions, Clients{Tenant: api}, Options{Tenant: "acme", Channel: "nightly"}, &out, OutputTable); err == nil {
		t.Fatal("an unknown channel must fail")
	}
}

func TestCompareMinors(t *testing.T) {
	if compareMinors("1.9", "1.10") >= 0 || compareMinors("2.0", "1.99") <= 0 || compareMinors("x", "y") >= 0 {
		t.Fatal("minor order")
	}
}
