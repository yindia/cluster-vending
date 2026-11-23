package cluster

import (
	"context"
	"slices"
	"strings"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/results"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// Page sizes of ListNodeGroupNodes: a node group may have a thousand nodes.
const (
	defaultNodesPageSize = 100
	maxNodesPageSize     = 500
)

// The default managed node group, the platform's only one, exists on every
// cluster; its status name is not in the spec.
const systemNodeGroup = "system"

const (
	noticeNodesNotCollected = "the node list has not been collected yet; the controller collects it while it reconciles the cluster's node groups"
	noticeNodesIncomplete   = "the node list is incomplete: the controller stopped collecting before it had read every node"
)

// ListNodeGroupNodes returns a page of the nodes of one node group: read live
// by the live reader when one is deployed (WithLive, PROP-02 phase 3), else
// read from the group's shard in the cluster's results bucket: the index
// first, then only that shard (docs/architecture.md#results-in-s3, Scale).
// Problems with the bucket are notices in the answer, never errors.
func (s *ClusterService) ListNodeGroupNodes(ctx context.Context, req *cloudv1.ListNodeGroupNodesRequest) (_ *cloudv1.ListNodeGroupNodesResponse, err error) {
	defer func() { err = rpcError(err) }()
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	row, err := s.get(ctx, req.GetTenantId(), req.GetClusterId())
	if err != nil {
		return nil, err
	}
	if s.live != nil {
		return s.liveNodes(ctx, row, req)
	}
	if sortScope(int32(req.GetSortBy()), isDesc(req.GetSortOrder(), false), false) != "" {
		return nil, errSortNotLive("the node list")
	}
	known, notice := nodeGroupKnown(row, req.GetNodeGroup())
	if !known {
		return nil, connect.NewError(connect.CodeNotFound, "cluster "+req.GetClusterId()+" has no node group "+req.GetNodeGroup())
	}
	out := &cloudv1.ListNodeGroupNodesResponse{}
	ref := row.ResultRef(string(results.Nodes))
	if ref == nil {
		out.Notices = append(out.Notices, notice)
		if notice == "" {
			out.Notices = []string{noticeNodesNotCollected}
		}
		return out, nil
	}
	shard, bad := s.readNodeShard(ctx, row, ref, req.GetNodeGroup(), out)
	if bad != "" {
		out.Notices = append(out.Notices, bad)
		return out, nil
	}
	if !out.GetComplete() {
		out.Notices = append(out.Notices, noticeNodesIncomplete)
	}
	rows := filterNodes(shard.nodes, req.GetFilter(), req.GetNameContains())
	binding := shard.etag + "\x00" + req.GetFilter().String() + "\x00" + strings.ToLower(req.GetNameContains())
	start, err := etagOffset(binding, req.GetPageToken(), len(rows))
	if err != nil {
		return nil, err
	}
	size := int(req.GetPageSize())
	if size <= 0 {
		size = defaultNodesPageSize
	}
	end := min(start+min(size, maxNodesPageSize), len(rows))
	out.Nodes = rows[start:end]
	out.Total = int32(len(rows)) //nolint:gosec // bounded by the object's size
	if end < len(rows) {
		out.NextPageToken = etagToken(binding, end)
	}
	return out, nil
}

// nodeGroupKnown reports whether the cluster has the node group: the default
// managed group and the platform's own always, else the spec's and the
// status's. The notice is the group's status notice (the controller could not
// write its node list), if any.
func nodeGroupKnown(row *model.Cluster, name string) (bool, string) {
	cl, err := stored(row)
	if err != nil {
		return false, ""
	}
	notice := ""
	for _, g := range cl.GetStatus().GetNodeGroups() {
		if g.GetName() == name {
			notice = g.GetNodesNotice()
		}
	}
	known := name == systemNodeGroup ||
		slices.ContainsFunc(cl.GetSpec().GetNodeGroups(), func(g *cloudv1.NodeGroupSpec) bool { return g.GetName() == name }) ||
		slices.ContainsFunc(cl.GetStatus().GetNodeGroups(), func(g *cloudv1.NodeGroupStatus) bool { return g.GetName() == name })
	return known, notice
}

type nodeShard struct {
	nodes []*cloudv1.NodeGroupNode
	etag  string
}

// readNodeShard reads the index, then the group's shard, and sets what the
// response says of the collection (time, completeness). A group the index
// lacks has no nodes. The second result is the notice when the bucket cannot
// be read.
func (s *ClusterService) readNodeShard(ctx context.Context, row *model.Cluster, ref *model.ResultRef, group string, out *cloudv1.ListNodeGroupNodesResponse) (nodeShard, string) {
	account, notice := s.resultAccount(ctx, row, ref, results.Nodes)
	if notice != "" {
		return nodeShard{}, notice
	}
	idx, _, collected, err := s.results.Index(ctx, account, row.Region, ref.Bucket, results.Nodes)
	if err != nil {
		s.logger().Warn("cluster results: node index not read", "cluster", row.ID, "bucket", ref.Bucket, "error", err)
		return nodeShard{}, results.Notice(err)
	}
	if t := idx.GetCollectedAt(); t != nil {
		collected = t.AsTime()
	}
	// An unchanged collection moves only the summary's time.
	if ref.CollectedAt.After(collected) {
		collected = ref.CollectedAt
	}
	if !collected.IsZero() {
		out.CollectedAt = timestamppb.New(collected.UTC())
	}
	out.Complete = idx.GetComplete() && !ref.Incomplete
	if !slices.ContainsFunc(idx.GetShards(), func(e *cloudv1.ResultIndexShard) bool { return e.GetShard() == group }) {
		return nodeShard{}, ""
	}
	into := &cloudv1.NodeGroupNodes{}
	etag, _, err := s.results.LatestShard(ctx, account, row.Region, ref.Bucket, results.Nodes, group, into)
	if err != nil {
		s.logger().Warn("cluster results: node shard not read", "cluster", row.ID, "group", group, "bucket", ref.Bucket, "error", err)
		return nodeShard{}, results.Notice(err)
	}
	return nodeShard{nodes: into.GetNodes(), etag: etag}, ""
}

// filterNodes keeps the nodes with the Ready condition the filter asks for
// and the text in their name (case-insensitive), in the shard's name order.
func filterNodes(nodes []*cloudv1.NodeGroupNode, filter cloudv1.NodeStatusFilter, contains string) []*cloudv1.NodeGroupNode {
	contains = strings.ToLower(strings.TrimSpace(contains))
	out := make([]*cloudv1.NodeGroupNode, 0, len(nodes))
	for _, n := range nodes {
		switch {
		case filter == cloudv1.NodeStatusFilter_NODE_STATUS_FILTER_READY && !n.GetReady(),
			filter == cloudv1.NodeStatusFilter_NODE_STATUS_FILTER_NOT_READY && n.GetReady(),
			contains != "" && !strings.Contains(strings.ToLower(n.GetName()), contains):
			continue
		}
		out = append(out, n)
	}
	return out
}

// GetNodeResources is the live resource totals of the cluster's node groups,
// read by the live reader. Without one deployed (WithLive) the answer is
// UNIMPLEMENTED once the caller's tenant and cluster check out.
func (s *ClusterService) GetNodeResources(ctx context.Context, req *cloudv1.GetNodeResourcesRequest) (_ *cloudv1.GetNodeResourcesResponse, err error) {
	defer func() { err = rpcError(err) }()
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	if _, err := s.get(ctx, req.GetTenantId(), req.GetClusterId()); err != nil {
		return nil, err
	}
	if s.live != nil {
		return s.liveNodeResources(ctx, req)
	}
	return nil, connect.NewError(connect.CodeUnimplemented, "node resources are read live from the workload cluster, which this deployment does not do yet")
}
