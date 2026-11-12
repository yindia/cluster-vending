package cluster

import (
	"context"
	"strconv"

	"connectrpc.com/connect/v2"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// deprecations is what the controller's last scan of the cluster found,
// as it is stored; nil when it was never scanned.
func deprecations(row *model.Cluster) *cloudv1.ClusterDeprecations {
	if len(row.Deprecations) == 0 {
		return nil
	}
	d := &cloudv1.ClusterDeprecations{}
	if err := reconcilecluster.Lenient.Unmarshal(row.Deprecations, d); err != nil {
		return nil // written by the controller; unreadable: as if never scanned
	}
	return d
}

// deprecationSummary is the scan without its findings: what a cluster's
// status carries. The findings are ListClusterDeprecations'.
func deprecationSummary(row *model.Cluster) *cloudv1.ClusterDeprecations {
	d := deprecations(row)
	if d != nil {
		d.Findings = nil
	}
	return d
}

// ListClusterDeprecations returns a page of what the controller's last
// scan of the cluster found, removed first, from the store. The API
// server never reads the workload cluster.
func (s *ClusterService) ListClusterDeprecations(ctx context.Context, req *cloudv1.ListClusterDeprecationsRequest) (_ *cloudv1.ListClusterDeprecationsResponse, err error) {
	defer func() { err = rpcError(err) }()
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	row, err := s.get(ctx, req.GetTenantId(), req.GetClusterId())
	if err != nil {
		return nil, err
	}
	d := deprecations(row)
	if d == nil {
		return &cloudv1.ListClusterDeprecationsResponse{}, nil
	}
	findings := d.GetFindings()
	if req.GetOnlyRemoved() {
		findings = nil
		for _, f := range d.GetFindings() {
			if f.GetRemoved() {
				findings = append(findings, f)
			}
		}
	}
	start := 0
	if token := req.GetPageToken(); token != "" {
		if start, err = strconv.Atoi(token); err != nil || start > len(findings) {
			return nil, connect.NewError(connect.CodeInvalidArgument, "page_token is not a token of this list")
		}
	}
	size := int(req.GetPageSize())
	if size <= 0 {
		size = defaultPageSize
	}
	end := min(start+min(size, maxPageSize), len(findings))
	out := &cloudv1.ListClusterDeprecationsResponse{Findings: findings[start:end], TargetVersion: d.GetTargetVersion(),
		ScannedAt: d.GetScannedAt(), Total: d.GetTotal(), Removed: d.GetRemoved(), Truncated: d.GetTruncated(), Error: d.GetError()}
	if end < len(findings) {
		out.NextPageToken = strconv.Itoa(end)
	}
	return out, nil
}
