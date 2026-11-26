package live

import (
	"context"
	"errors"
	"net/http"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// service is the live reader's internal Connect endpoint: the live RPCs of
// cloud.v1.ClusterService, nothing else (every other RPC is UNIMPLEMENTED).
// Only the API server reaches it (its NetworkPolicy); the API has checked
// the caller's tenant and cluster before it calls.
type liveService struct {
	cloudv1connect.UnimplementedClusterServiceHandler
	r *Reader
}

func (s *liveService) ListNodeGroupNodes(ctx context.Context, req *cloudv1.ListNodeGroupNodesRequest) (*cloudv1.ListNodeGroupNodesResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	res, err := s.r.ListNodeGroupNodes(ctx, req)
	return res, ToConnect(err)
}

func (s *liveService) GetNodeResources(ctx context.Context, req *cloudv1.GetNodeResourcesRequest) (*cloudv1.GetNodeResourcesResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	res, err := s.r.GetNodeResources(ctx, req)
	return res, ToConnect(err)
}

// NewHandler serves r over Connect, with /healthz, /readyz and /metrics.
func NewHandler(r *Reader) http.Handler {
	server := connect.NewServer(reads.Interceptor)
	cloudv1connect.RegisterClusterServiceHandler(server, &liveService{r: r})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	mountOperational(mux)
	return mux
}

// NewMetricsHandler serves /metrics, /healthz and /readyz only: what a
// scraper reaches on the metrics listener (live-metricsAddr), without the
// internal endpoint.
func NewMetricsHandler() http.Handler {
	mux := http.NewServeMux()
	mountOperational(mux)
	return mux
}

func mountOperational(mux *http.ServeMux) {
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	mux.Handle("GET /metrics", promhttp.HandlerFor(metrics, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", ok)
	mux.HandleFunc("GET /readyz", ok)
}

// Client is what the API server calls the live reader with.
type Client interface {
	ListNodeGroupNodes(context.Context, *cloudv1.ListNodeGroupNodesRequest) (*cloudv1.ListNodeGroupNodesResponse, error)
	GetNodeResources(context.Context, *cloudv1.GetNodeResourcesRequest) (*cloudv1.GetNodeResourcesResponse, error)
}

// NewClient is the live reader at url (its internal Service), over httpClient.
func NewClient(url string, httpClient *http.Client) Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return cloudv1connect.NewClusterServiceClient(connect.NewClient(connecthttp.NewTransport(httpClient, url)))
}

// Forward is the live reader's answer as the API answers it: the reader's
// own verdict (its code, message and LiveReadError) as the API's, and a
// failure to reach the reader as UNAVAILABLE with LIVE_READER_UNAVAILABLE.
func Forward(err error) error {
	if err == nil {
		return nil
	}
	var cerr *connect.Error
	if errors.As(err, &cerr) && cerr.IsRemote() {
		out := connect.NewError(cerr.Code(), cerr.Message())
		for _, d := range cerr.Details() {
			out = out.WithDetail(d)
		}
		return out
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return connect.NewError(connect.CodeDeadlineExceeded, "the live read did not finish in time")
	}
	return ToConnect(&Error{Reason: ReasonLiveReaderUnavailable, Message: "the live reader cannot be reached right now"})
}
