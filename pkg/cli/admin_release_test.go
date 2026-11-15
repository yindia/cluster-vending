package cli

import (
	"bytes"
	"context"
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
)

// releaseReader is an AdminService that keeps the release reads it got.
type releaseReader struct {
	cloudv1connect.UnimplementedAdminServiceHandler
	release  *cloudv1.GetReleaseRequest
	channel  *cloudv1.GetReleaseChannelRequest
	rollout  *cloudv1.GetRolloutRequest
	releases *cloudv1.ListReleasesRequest
	promote  *cloudv1.PromoteReleaseChannelRequest
}

func (r *releaseReader) GetRelease(_ context.Context, req *cloudv1.GetReleaseRequest) (*cloudv1.GetReleaseResponse, error) {
	r.release = req
	return &cloudv1.GetReleaseResponse{Release: &cloudv1.Release{Name: req.GetName()}}, nil
}

func (r *releaseReader) ListReleases(_ context.Context, req *cloudv1.ListReleasesRequest) (*cloudv1.ListReleasesResponse, error) {
	r.releases = req
	return &cloudv1.ListReleasesResponse{}, nil
}

func (r *releaseReader) GetReleaseChannel(_ context.Context, req *cloudv1.GetReleaseChannelRequest) (*cloudv1.GetReleaseChannelResponse, error) {
	r.channel = req
	return &cloudv1.GetReleaseChannelResponse{Channel: &cloudv1.ReleaseChannelInfo{Channel: req.GetChannel()}}, nil
}

func (r *releaseReader) PromoteReleaseChannel(_ context.Context, req *cloudv1.PromoteReleaseChannelRequest) (*cloudv1.PromoteReleaseChannelResponse, error) {
	r.promote = req
	return &cloudv1.PromoteReleaseChannelResponse{Note: "beta takes " + req.GetRelease() + " on the controller's next pass",
		Channel: &cloudv1.ReleaseChannelInfo{Channel: req.GetChannel()}}, nil
}

// admin channel promote sends the channel and the release, and prints
// what happens next.
func TestAdminChannelPromote(t *testing.T) {
	svc := &releaseReader{}
	var out bytes.Buffer

	err := Run(t.Context(), AdminChannelPromote, Clients{Admin: svc}, Options{ID: "beta", PromoteRelease: "2026-11-a"}, &out, OutputTable)

	if err != nil {
		t.Fatal(err)
	}
	if p := svc.promote; p.GetChannel() != cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA || p.GetRelease() != "2026-11-a" {
		t.Errorf("request = %v", p)
	}
	if !bytes.Contains(out.Bytes(), []byte("beta takes 2026-11-a on the controller's next pass")) {
		t.Errorf("output:\n%s", out.String())
	}
}

func (r *releaseReader) GetRollout(_ context.Context, req *cloudv1.GetRolloutRequest) (*cloudv1.GetRolloutResponse, error) {
	r.rollout = req
	return &cloudv1.GetRolloutResponse{Rollout: &cloudv1.Rollout{Channel: req.GetChannel()}}, nil
}

// The admin release commands send what their flags name: the release's
// name, the channel, the rollout's channel, tier and page.
func TestAdminReleaseCommandsSendTheirFlags(t *testing.T) {
	svc := &releaseReader{}
	clients := Clients{Admin: svc}
	run := func(cmd Command, o Options) {
		t.Helper()
		if err := Run(t.Context(), cmd, clients, o, &bytes.Buffer{}, OutputTable); err != nil {
			t.Fatal(err)
		}
	}

	run(AdminReleaseGet, Options{ID: "shipped-1aed928c"})
	run(AdminReleaseList, Options{PageSize: 5, PageToken: "5"})
	run(AdminChannelGet, Options{ID: "beta"})
	run(AdminRolloutGet, Options{Channel: "stable", Tier: "1", PageSize: 10})

	if svc.release.GetName() != "shipped-1aed928c" {
		t.Errorf("release = %v", svc.release)
	}
	if svc.releases.GetPageSize() != 5 || svc.releases.GetPageToken() != "5" {
		t.Errorf("releases = %v", svc.releases)
	}
	if svc.channel.GetChannel() != cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA {
		t.Errorf("channel = %v", svc.channel)
	}
	if r := svc.rollout; r.GetChannel() != cloudv1.ReleaseChannel_RELEASE_CHANNEL_STABLE ||
		r.GetTier() != cloudv1.ClusterTier_CLUSTER_TIER_1 || r.GetPageSize() != 10 {
		t.Errorf("rollout = %v", r)
	}
	if err := Run(t.Context(), AdminRolloutGet, clients, Options{Tier: "1"}, &bytes.Buffer{}, OutputTable); err == nil {
		t.Error("rollout get without --channel is refused")
	}
}
