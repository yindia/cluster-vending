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
	releases *cloudv1.ListReleasesRequest
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

// The admin release commands send what their flags name: the release's
// name and page, the channel.
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

	if svc.release.GetName() != "shipped-1aed928c" {
		t.Errorf("release = %v", svc.release)
	}
	if svc.releases.GetPageSize() != 5 || svc.releases.GetPageToken() != "5" {
		t.Errorf("releases = %v", svc.releases)
	}
	if svc.channel.GetChannel() != cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA {
		t.Errorf("channel = %v", svc.channel)
	}
	if err := Run(t.Context(), AdminChannelGet, clients, Options{ID: "edge"}, &bytes.Buffer{}, OutputTable); err == nil {
		t.Error("channel get of no channel is refused")
	}
}
