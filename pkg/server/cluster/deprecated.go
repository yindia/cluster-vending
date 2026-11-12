package cluster

import (
	"context"
	"fmt"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	"github.com/yindia/cluster-vending/pkg/deprecation"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// ListDeprecatedResources returns what the controller last found in the
// cluster of objects written with deprecated API versions, narrowed to a
// target version. Like GetCluster it reads the database only: the status
// last recorded for the cluster, which holds the scan. The handler does
// not reach into the cluster (it has neither its kubeconfig nor an AWS
// role); the controller scans (reconcilecluster: scanDeprecations).
func (s *ClusterService) ListDeprecatedResources(ctx context.Context, req *cloudv1.ListDeprecatedResourcesRequest) (_ *cloudv1.ListDeprecatedResourcesResponse, err error) {
	defer func() { err = rpcError(err) }()
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	row, err := s.get(ctx, req.GetTenantId(), req.GetClusterId())
	if err != nil {
		return nil, err
	}
	c, err := stored(row)
	if err != nil {
		return nil, err
	}
	scan := c.GetStatus().GetDeprecatedResources()
	if scan == nil {
		return &cloudv1.ListDeprecatedResourcesResponse{}, nil
	}
	target := req.GetTargetVersion()
	if target == "" {
		target = scan.GetKubernetesVersion()
	}
	if target == "" {
		// Scanned, but never with success: there is the reason, and
		// nothing to narrow.
		return &cloudv1.ListDeprecatedResourcesResponse{Scan: scan}, nil
	}
	narrowed, err := deprecatedBy(scan, target)
	if err != nil {
		return nil, err
	}
	return &cloudv1.ListDeprecatedResourcesResponse{Scan: narrowed, TargetVersion: target}, nil
}

// deprecatedBy is scan with only the resources whose API version was
// deprecated in the Kubernetes version target ("1.x") or before. scan is
// not changed; its total stays what the scan found.
func deprecatedBy(scan *cloudv1.DeprecatedResourceScan, target string) (*cloudv1.DeprecatedResourceScan, error) {
	limit, err := deprecation.ParseVersion(target)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf("target_version: %v", err))
	}
	out := proto.CloneOf(scan)
	out.Resources = nil
	for _, r := range scan.GetResources() {
		since, err := deprecation.ParseVersion(r.GetDeprecatedIn())
		if err != nil {
			// Written by the controller from the rules: a version it
			// cannot read is a stored scan this server does not understand.
			return nil, connect.NewError(connect.CodeInternal, fmt.Sprintf("stored scan: %v", err))
		}
		if since <= limit {
			out.Resources = append(out.Resources, r)
		}
	}
	return out, nil
}
