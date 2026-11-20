package cluster

import (
	"context"
	"strconv"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/recommend"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
	"github.com/yindia/cluster-vending/pkg/results"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// now is the clock of the recommendations; tests replace it.
var now = time.Now

// recommendationSummary is the summary of the cluster's recorded usage,
// as it is stored; nil when it was never sampled.
func recommendationSummary(row *model.Cluster) *cloudv1.ClusterRecommendations {
	if len(row.Recommendations) == 0 {
		return nil
	}
	r := &cloudv1.ClusterRecommendations{}
	if err := reconcilecluster.Lenient.Unmarshal(row.Recommendations, r); err != nil {
		return nil // written by the controller; unreadable: as if never sampled
	}
	return r
}

// ListClusterRecommendations returns a page of the recommendations the
// cluster's recorded usage gives now, computed from the store: the API
// server never reads the workload cluster.
func (s *ClusterService) ListClusterRecommendations(ctx context.Context, req *cloudv1.ListClusterRecommendationsRequest) (_ *cloudv1.ListClusterRecommendationsResponse, err error) {
	defer func() { err = rpcError(err) }()
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	row, err := s.get(ctx, req.GetTenantId(), req.GetClusterId())
	if err != nil {
		return nil, err
	}
	out := &cloudv1.ListClusterRecommendationsResponse{Summary: recommendationSummary(row)}
	// The recommendation set is in the cluster's results bucket once a
	// scanner wrote it there.
	set := &cloudv1.ListClusterRecommendationsResponse{}
	bucket := s.readResult(ctx, row, results.Recommendations, set)
	if bucket.Used {
		return s.pageStoredRecommendations(req, set, bucket)
	}
	if bucket.Notice != "" {
		out.Summary = withRecommendationNotice(out.Summary, bucket.Notice)
	}
	raw, err := s.deps.Store.Clusters().UsageHistory(ctx, req.GetTenantId(), req.GetClusterId())
	if err != nil {
		return nil, service.ToConnect(err)
	}
	history, err := recommend.Decode(raw)
	if err != nil {
		// Written by the controller; one this build cannot read is not the
		// caller's fault: there are no recommendations until it rewrites it.
		return out, nil
	}
	var rows []recommend.Row
	for _, r := range recommend.Rows(history, now()) {
		if req.GetNamespace() != "" && r.Namespace != req.GetNamespace() {
			continue
		}
		if req.GetOnlyChanges() && !r.Changes() {
			continue
		}
		rows = append(rows, r)
	}
	start := 0
	if token := req.GetPageToken(); token != "" {
		if start, err = strconv.Atoi(token); err != nil || start > len(rows) {
			return nil, connect.NewError(connect.CodeInvalidArgument, "page_token is not a token of this list")
		}
	}
	size := int(req.GetPageSize())
	if size <= 0 {
		size = defaultPageSize
	}
	end := min(start+min(size, maxPageSize), len(rows))
	for _, r := range rows[start:end] {
		out.Recommendations = append(out.Recommendations, r.Proto())
	}
	out.Total = int32(len(rows)) //nolint:gosec // bounded by recommend.MaxContainers
	if end < len(rows) {
		out.NextPageToken = strconv.Itoa(end)
	}
	return out, nil
}

// pageStoredRecommendations is the page of a recommendation set read from
// the results bucket: the same filters as the computed list, and tokens bound
// to the object's ETag.
func (s *ClusterService) pageStoredRecommendations(req *cloudv1.ListClusterRecommendationsRequest, set *cloudv1.ListClusterRecommendationsResponse, bucket bucketRead) (*cloudv1.ListClusterRecommendationsResponse, error) {
	var rows []*cloudv1.ResourceRecommendation
	for _, r := range set.GetRecommendations() {
		if req.GetNamespace() != "" && r.GetNamespace() != req.GetNamespace() {
			continue
		}
		if req.GetOnlyChanges() && !recommend.ProtoChanges(r) {
			continue
		}
		rows = append(rows, r)
	}
	start, err := etagOffset(bucket.ETag, req.GetPageToken(), len(rows))
	if err != nil {
		return nil, err
	}
	size := int(req.GetPageSize())
	if size <= 0 {
		size = defaultPageSize
	}
	end := min(start+min(size, maxPageSize), len(rows))
	out := &cloudv1.ListClusterRecommendationsResponse{Summary: set.GetSummary(), Recommendations: rows[start:end],
		Total: int32(len(rows))} //nolint:gosec // bounded by the object's size
	if end < len(rows) {
		out.NextPageToken = etagToken(bucket.ETag, end)
	}
	return out, nil
}

// withRecommendationNotice is the summary (nil: none) with a coverage notice
// in its message.
func withRecommendationNotice(sum *cloudv1.ClusterRecommendations, notice string) *cloudv1.ClusterRecommendations {
	out := &cloudv1.ClusterRecommendations{}
	if sum != nil {
		out = proto.CloneOf(sum)
	}
	if out.Message != "" {
		notice = out.Message + " " + notice
	}
	out.Message = notice
	return out
}
