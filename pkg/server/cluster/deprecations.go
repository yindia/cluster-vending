package cluster

import (
	"context"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
	"github.com/yindia/cluster-vending/pkg/results"
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
	// The full scan is in the cluster's results bucket once a scanner wrote
	// it there; the Cluster object then has its counts only.
	fromBucket := &cloudv1.ClusterDeprecations{}
	bucket := s.readResult(ctx, row, results.Deprecations, fromBucket)
	switch {
	case bucket.Used:
		d = fromBucket
	case bucket.Notice != "":
		d = withDeprecationNotice(d, row.ResultRef(string(results.Deprecations)), bucket.Notice)
	}
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
	desc := isDesc(req.GetSortOrder(), false)
	scope := sortScope(int32(req.GetSortBy()), desc, false)
	if scope != "" {
		findings = sortDeprecations(findings, req.GetSortBy(), desc)
	}
	binding := scope
	if bucket.Used {
		binding = bucket.ETag + scope
	}
	start, err := pageStart(binding, scope, req.GetPageToken(), len(findings), bucket.Used)
	if err != nil {
		return nil, err
	}
	size := int(req.GetPageSize())
	if size <= 0 {
		size = defaultPageSize
	}
	end := min(start+min(size, maxPageSize), len(findings))
	out := &cloudv1.ListClusterDeprecationsResponse{Findings: findings[start:end], TargetVersion: d.GetTargetVersion(),
		ScannedAt: d.GetScannedAt(), Total: d.GetTotal(), Removed: d.GetRemoved(), Truncated: d.GetTruncated(), Error: d.GetError()}
	if end < len(findings) {
		out.NextPageToken = nextToken(binding, scope, end, bucket.Used)
	}
	return out, nil
}

// withDeprecationNotice is the scan of the Cluster object (nil: none) with a
// coverage notice in its error. The counts are the Cluster's, else those of
// the summary ref names, so the answer says how much there is even when it
// cannot list it.
func withDeprecationNotice(d *cloudv1.ClusterDeprecations, ref *model.ResultRef, notice string) *cloudv1.ClusterDeprecations {
	out := &cloudv1.ClusterDeprecations{}
	if d != nil {
		out = proto.CloneOf(d)
	}
	if ref != nil && out.Total == 0 && out.Removed == 0 && out.ScannedAt == nil {
		out.Total, out.Removed, out.Truncated = ref.Total, ref.Removed, ref.Truncated
		if !ref.CollectedAt.IsZero() {
			out.ScannedAt = timestamppb.New(ref.CollectedAt)
		}
	}
	if out.Error != "" {
		notice = out.Error + "; " + notice
	}
	out.Error = notice
	return out
}
