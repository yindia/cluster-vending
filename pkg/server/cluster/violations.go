package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/policy"
	"github.com/yindia/cluster-vending/pkg/results"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// maxPolicyNotices mirrors the cap policy.notice applies to stored notices.
const maxPolicyNotices = 32

// policySummary marks a snapshot stale when its collection time is older than
// the expected cadence, or unknown while it still holds rows. Staleness is
// separate from incomplete: the sources may have been fully read at that time.
func policySummary(p *cloudv1.ClusterPolicySnapshot, at time.Time) {
	t := p.Summary.CollectedAt
	if t != nil {
		p.Summary.Stale = at.Sub(t.AsTime()) > 2*policy.Interval
	} else {
		p.Summary.Stale = len(p.Violations) > 0 || len(p.Events) > 0
	}
	if p.Summary.Stale && len(p.Summary.Notices) < maxPolicyNotices {
		p.Summary.Notices = append(p.Summary.Notices, "Policy snapshot is stale; the controller has not collected recently.")
	}
}
func policyPage(raw []byte, scope []string, token string, size int32, n int) (int, int, string, error) {
	h := sha256.New()
	_, _ = h.Write(raw)
	for _, s := range scope {
		// Length-prefix each field: namespace/engine may contain NUL, so a
		// plain separator lets distinct scopes hash identically.
		_, _ = fmt.Fprintf(h, "%d:", len(s))
		_, _ = h.Write([]byte(s))
	}
	key := hex.EncodeToString(h.Sum(nil)[:16])
	start := 0
	invalid := func() (int, int, string, error) {
		return 0, 0, "", connect.NewError(connect.CodeInvalidArgument, "page_token does not match this snapshot and scope; restart the list")
	}
	if token != "" {
		prefix, offset, ok := strings.Cut(token, ":")
		if !ok || prefix != key {
			return invalid()
		}
		var err error
		start, err = strconv.Atoi(offset)
		if err != nil || start < 0 || start > n {
			return invalid()
		}
	}
	if size <= 0 {
		size = defaultPageSize
	}
	end := min(start+int(min(size, maxPageSize)), n)
	next := ""
	if end < n {
		next = fmt.Sprintf("%s:%d", key, end)
	}
	return start, end, next, nil
}
func (s *ClusterService) ListClusterViolations(ctx context.Context, req *cloudv1.ListClusterViolationsRequest) (_ *cloudv1.ListClusterViolationsResponse, err error) {
	defer func() { err = rpcError(err) }()
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	row, err := s.get(ctx, req.TenantId, req.ClusterId)
	if err != nil {
		return nil, err
	}
	if sortScope(int32(req.GetSortBy()), isDesc(req.GetSortOrder(), false), false) != "" {
		return nil, errSortNotLive("the violation list")
	}
	p, raw := s.policySnapshot(ctx, row, results.Violations)
	policySummary(p, now())
	var rows []*cloudv1.ClusterViolation
	for _, v := range p.Violations {
		if (req.Engine == "" || v.Engine == req.Engine) && (req.Namespace == "" || v.GetResource().GetNamespace() == req.Namespace) {
			rows = append(rows, v)
		}
	}
	start, end, next, err := policyPage(raw, []string{req.TenantId, req.ClusterId, "violations", req.Namespace, req.Engine}, req.PageToken, req.PageSize, len(rows))
	if err != nil {
		return nil, err
	}
	return &cloudv1.ListClusterViolationsResponse{Violations: rows[start:end], NextPageToken: next, Summary: p.Summary}, nil
}
func (s *ClusterService) ListClusterViolationEvents(ctx context.Context, req *cloudv1.ListClusterViolationEventsRequest) (_ *cloudv1.ListClusterViolationEventsResponse, err error) {
	defer func() { err = rpcError(err) }()
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	row, err := s.get(ctx, req.TenantId, req.ClusterId)
	if err != nil {
		return nil, err
	}
	if sortScope(int32(req.GetSortBy()), isDesc(req.GetSortOrder(), true), true) != "" {
		return nil, errSortNotLive("the event list")
	}
	p, raw := s.policySnapshot(ctx, row, results.ViolationEvents)
	policySummary(p, now())
	policy.Expire(p, now())
	var rows []*cloudv1.ClusterViolationEvent
	for _, e := range p.Events {
		if (req.Engine == "" || e.Engine == req.Engine) && (req.Namespace == "" || e.GetResource().GetNamespace() == req.Namespace) {
			rows = append(rows, e)
		}
	}
	// Include retained identities: a token must not shift when TTL removes rows.
	scope := []string{req.TenantId, req.ClusterId, "events", req.Namespace, req.Engine}
	for _, e := range rows {
		scope = append(scope, e.Id)
	}
	start, end, next, err := policyPage(raw, scope, req.PageToken, req.PageSize, len(rows))
	if err != nil {
		return nil, err
	}
	return &cloudv1.ListClusterViolationEventsResponse{Events: rows[start:end], NextPageToken: next, Summary: p.Summary, RetentionNotice: policy.RetentionNotice}, nil
}

// policySnapshot is the cluster's policy snapshot and the bytes its page
// tokens are bound to: the object of kind in the results bucket (its ETag)
// when the cluster's summary names one, else the Cluster object's snapshot.
// A bucket that cannot be read leaves the Cluster object's snapshot with a
// coverage notice in its summary.
func (s *ClusterService) policySnapshot(ctx context.Context, row *model.Cluster, kind results.Kind) (*cloudv1.ClusterPolicySnapshot, []byte) {
	snapshot := &cloudv1.ClusterPolicySnapshot{}
	bucket := s.readResult(ctx, row, kind, snapshot)
	switch {
	case bucket.Used:
		if snapshot.Summary == nil {
			snapshot.Summary = &cloudv1.ClusterPolicySnapshotSummary{}
		}
		if snapshot.Summary.CollectedAt == nil && !bucket.Collected.IsZero() {
			snapshot.Summary.CollectedAt = timestamppb.New(bucket.Collected)
		}
		return snapshot, []byte(bucket.ETag)
	case bucket.Notice != "":
		p := policy.Decode(row.PolicySnapshot)
		p.Summary.Incomplete = true
		if len(p.Summary.Notices) < maxPolicyNotices {
			p.Summary.Notices = append(p.Summary.Notices, bucket.Notice)
		}
		return p, row.PolicySnapshot
	}
	return policy.Decode(row.PolicySnapshot), row.PolicySnapshot
}
