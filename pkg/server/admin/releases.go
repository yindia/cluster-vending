package admin

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"connectrpc.com/connect/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/rollout"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// Page sizes of the release lists.
const (
	defaultPage = 50
	maxPage     = 200
)

// reader reads the release objects. They are read at the API server, not
// from the server's cache, which does not hold them: these are operator
// reads, few and far between.
func (s *Service) reader() client.Reader {
	switch {
	case s.deps.KubeReader != nil:
		return s.deps.KubeReader
	case s.deps.Kube != nil:
		return s.deps.Kube
	}
	return noCluster{}
}

// noCluster is the reader of a server built without the management
// cluster (tests): every read is unavailable.
type noCluster struct{}

func (noCluster) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errNoCluster
}

func (noCluster) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errNoCluster
}

var errNoCluster = errors.New("the server has no management cluster client")

// readError answers a failed read of the management cluster: NOT_FOUND
// for a missing object (what), UNAVAILABLE when the server may not read
// the kind or cannot reach the cluster. The cause goes to the log only.
func (s *Service) readError(ctx context.Context, what string, err error) error {
	switch {
	case apierrors.IsNotFound(err):
		return connect.NewError(connect.CodeNotFound, what+" not found")
	case apierrors.IsForbidden(err):
		s.logger().ErrorContext(ctx, "read "+what, "error", err)
		return connect.NewError(connect.CodeUnavailable, "the API server may not read "+what+
			" in the management cluster (its RBAC); see the server's log")
	}
	s.logger().ErrorContext(ctx, "read "+what, "error", err)
	return connect.NewError(connect.CodeUnavailable, "cannot read "+what+" from the management cluster; see the server's log")
}

// page is the slice [start, end) of n items the request's page asks for,
// and the next page's token ("" at the end).
func page(n int, size int32, token string) (start, end int, next string, err error) {
	if token != "" {
		if start, err = strconv.Atoi(token); err != nil || start < 0 || start > n {
			return 0, 0, "", connect.NewError(connect.CodeInvalidArgument, "page_token is not a token of this list")
		}
	}
	limit := int(size)
	if limit <= 0 {
		limit = defaultPage
	}
	end = min(start+min(limit, maxPage), n)
	if end < n {
		next = strconv.Itoa(end)
	}
	return start, end, next, nil
}

// ListReleases lists the releases, newest first: by serial (a release
// without one is below those with one, and is ordered by its creation time).
func (s *Service) ListReleases(ctx context.Context, req *cloudv1.ListReleasesRequest) (*cloudv1.ListReleasesResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	list := &cloudv1alpha1.ReleaseList{}
	if err := s.reader().List(ctx, list); err != nil {
		return nil, s.readError(ctx, "releases", err)
	}
	cloudv1alpha1.SortReleasesNewestFirst(list.Items)
	start, end, next, err := page(len(list.Items), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	out := &cloudv1.ListReleasesResponse{NextPageToken: next}
	for i := start; i < end; i++ {
		out.Releases = append(out.Releases, ReleaseFromAPI(&list.Items[i], false))
	}
	return out, nil
}

// GetRelease returns one release with what it offers per minor.
func (s *Service) GetRelease(ctx context.Context, req *cloudv1.GetReleaseRequest) (*cloudv1.GetReleaseResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	rel := &cloudv1alpha1.Release{}
	if err := s.reader().Get(ctx, client.ObjectKey{Name: req.GetName()}, rel); err != nil {
		return nil, s.readError(ctx, fmt.Sprintf("release %s", req.GetName()), err)
	}
	return &cloudv1.GetReleaseResponse{Release: ReleaseFromAPI(rel, true)}, nil
}

// channels reads the channel objects, alpha first; a channel that does
// not exist (yet) is left out.
func (s *Service) channels(ctx context.Context) ([]cloudv1alpha1.ReleaseChannel, error) {
	list := &cloudv1alpha1.ReleaseChannelList{}
	if err := s.reader().List(ctx, list); err != nil {
		return nil, s.readError(ctx, "release channels", err)
	}
	order := ChannelNames()
	slices.SortFunc(list.Items, func(a, b cloudv1alpha1.ReleaseChannel) int {
		return cmp.Compare(slices.Index(order, a.Name), slices.Index(order, b.Name))
	})
	return list.Items, nil
}

// channel reads one channel object.
func (s *Service) channel(ctx context.Context, c cloudv1.ReleaseChannel) (*cloudv1alpha1.ReleaseChannel, error) {
	name := ChannelName(c)
	ch := &cloudv1alpha1.ReleaseChannel{}
	if err := s.reader().Get(ctx, client.ObjectKey{Name: name}, ch); err != nil {
		return nil, s.readError(ctx, fmt.Sprintf("release channel %s", name), err)
	}
	return ch, nil
}

// ListReleaseChannels lists the three channels.
func (s *Service) ListReleaseChannels(ctx context.Context, req *cloudv1.ListReleaseChannelsRequest) (*cloudv1.ListReleaseChannelsResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	chs, err := s.channels(ctx)
	if err != nil {
		return nil, err
	}
	out := &cloudv1.ListReleaseChannelsResponse{}
	for i := range chs {
		out.Channels = append(out.Channels, ChannelFromAPI(&chs[i], false))
	}
	return out, nil
}

// GetReleaseChannel returns one channel with its history and release
// moves, and its rollout planned now, the way the controller does
// (rollout.Compute), cluster by cluster. Whether automatic upgrades are
// on is the controller's setting: it is taken from what the controller
// last reported.
func (s *Service) GetReleaseChannel(ctx context.Context, req *cloudv1.GetReleaseChannelRequest) (*cloudv1.GetReleaseChannelResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	ch, err := s.channel(ctx, req.GetChannel())
	if err != nil {
		return nil, err
	}
	in, err := rollout.Read(ctx, s.reader(), ch)
	if err != nil {
		return nil, s.readError(ctx, "the rollout of channel "+ch.Name, err)
	}
	in.Now, in.Automatic = s.now(), ch.Status.Rollout != nil && !ch.Status.Rollout.ReportOnly
	res := rollout.Compute(in)
	planned := PlannedRolloutFromAPI(ch, res)
	for _, r := range res.Rows {
		planned.Members = append(planned.Members, RolloutMemberFromRow(r))
	}
	frozen, err := s.deps.Freeze(ctx)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	return &cloudv1.GetReleaseChannelResponse{Channel: ChannelFromAPI(ch, true), Rollout: planned, Freeze: reconcile.FreezeProto(frozen)}, nil
}
