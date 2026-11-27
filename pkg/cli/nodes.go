package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// maxNodePages bounds how many pages of one group one command reads.
const maxNodePages = 50

// ClusterNodes is cluster nodes: the nodes of the cluster's node groups
// (ClusterService.ListNodeGroupNodes), one group with --group or every
// group the cluster's status lists, as the console's node group pages show
// them. --manifests prints the groups' NodePool and EC2NodeClass YAML
// instead (ListClusterNodeGroupManifests).
func ClusterNodes(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	switch {
	case o.ID == "":
		return nil, errors.New("--id is required: the cluster's ID")
	case o.Manifests:
		return nodeGroupManifests(ctx, c, o)
	}
	req, err := nodesRequest(o)
	if err != nil {
		return nil, err
	}
	groups := []string{o.NodeGroup}
	if o.NodeGroup == "" {
		if o.PageToken != "" {
			return nil, errors.New("--page-token needs --group: a token belongs to one group's list")
		}
		if groups, err = clusterNodeGroups(ctx, c, o); err != nil {
			return nil, err
		}
	}
	all := &cloudv1.ListNodeGroupNodesResponse{}
	var names []string // the group of each row of all.Nodes
	for _, g := range groups {
		res, err := groupNodes(ctx, c, proto.CloneOf(req), g, o)
		if err != nil {
			return nil, fmt.Errorf("node group %s: %w", g, err)
		}
		for range res.GetNodes() {
			names = append(names, g)
		}
		mergeNodes(all, res, len(groups) > 1, g)
	}
	if !isTable(o.Output) {
		return all, nil
	}
	if err := printNodes(o.Out, all, names, o.Output == OutputWide); err != nil {
		return nil, err
	}
	return nil, printNodeTotals(ctx, c, o, groups)
}

// printNodeTotals is the Totals section of cluster nodes: per group shown,
// CPU and memory allocatable vs requested, read live
// (ClusterService.GetNodeResources). A cluster that cannot be read live
// says so instead; the node rows above stand.
func printNodeTotals(ctx context.Context, c Clients, o Options, groups []string) error {
	res, err := call(ctx, &cloudv1.GetNodeResourcesRequest{TenantId: o.Tenant, ClusterId: o.ID}, c.Cluster.GetNodeResources)
	if err != nil {
		_, werr := fmt.Fprintf(o.Out, "\nTotals: not read live: %v\n", err)
		return werr
	}
	totals := res.(*cloudv1.GetNodeResourcesResponse)
	title := "Totals"
	if t := totals.GetLiveAt(); t != nil {
		title += ", live as of " + t.AsTime().UTC().Format("2006-01-02 15:04:05") + " UTC"
	}
	if !totals.GetComplete() {
		title += " (incomplete)"
	}
	var rows [][]string
	for _, g := range totals.GetGroups() {
		if slices.Contains(groups, g.GetNodeGroup()) {
			rows = append(rows, totalsRow(g.GetNodeGroup(), g))
		}
	}
	if len(groups) > 1 && totals.GetCluster() != nil {
		rows = append(rows, totalsRow("(cluster)", totals.GetCluster()))
	}
	return section(o.Out, title, []string{"GROUP", "NODES", "CPU REQUESTED", "MEMORY REQUESTED", "PODS"}, rows)
}

func totalsRow(name string, t *cloudv1.NodeResourceTotals) []string {
	return []string{name, fmt.Sprintf("%d/%d ready", t.GetReadyNodes(), t.GetNodes()),
		share(t.GetRequestedCpuMillis(), t.GetAllocatableCpuMillis(), cpuText),
		share(t.GetRequestedMemoryBytes(), t.GetAllocatableMemoryBytes(), memoryText),
		fmt.Sprintf("%d/%d", t.GetPodCount(), t.GetAllocatablePods())}
}

// nodesRequest is the request of --ready, --not-ready, --name, --sort and
// --order, for any group.
func nodesRequest(o Options) (*cloudv1.ListNodeGroupNodesRequest, error) {
	by, order, err := sortOf[cloudv1.NodeSortBy](o, "NODE_SORT_BY_")
	if err != nil {
		return nil, err
	}
	filter := cloudv1.NodeStatusFilter_NODE_STATUS_FILTER_ALL
	switch {
	case o.Ready && o.NotReady:
		return nil, errors.New("--ready and --not-ready exclude each other; give neither for every node")
	case o.Ready:
		filter = cloudv1.NodeStatusFilter_NODE_STATUS_FILTER_READY
	case o.NotReady:
		filter = cloudv1.NodeStatusFilter_NODE_STATUS_FILTER_NOT_READY
	}
	return &cloudv1.ListNodeGroupNodesRequest{TenantId: o.Tenant, ClusterId: o.ID, PageSize: o.PageSize,
		PageToken: o.PageToken, Filter: filter, NameContains: o.NameContains, SortBy: by, SortOrder: order}, nil
}

// clusterNodeGroups is the names of the groups the cluster's status lists:
// the system group and the Karpenter pools, platform and tenant.
func clusterNodeGroups(ctx context.Context, c Clients, o Options) ([]string, error) {
	res, err := call(ctx, &cloudv1.GetClusterRequest{TenantId: o.Tenant, ClusterId: o.ID}, c.Cluster.GetCluster)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, g := range res.(*cloudv1.GetClusterResponse).GetCluster().GetStatus().GetNodeGroups() {
		names = append(names, g.GetName())
	}
	if len(names) == 0 {
		return nil, errors.New("the cluster reports no node group yet; give --group to ask for one")
	}
	return names, nil
}

// groupNodes reads one group's pages: every page, or one when --page-size
// is given (the next token is then kept).
func groupNodes(ctx context.Context, c Clients, req *cloudv1.ListNodeGroupNodesRequest, group string, o Options) (*cloudv1.ListNodeGroupNodesResponse, error) {
	req.NodeGroup = group
	var all *cloudv1.ListNodeGroupNodesResponse
	for range maxNodePages {
		res, err := call(ctx, req, c.Cluster.ListNodeGroupNodes)
		if err != nil {
			return nil, err
		}
		page := res.(*cloudv1.ListNodeGroupNodesResponse)
		if all == nil {
			all = proto.CloneOf(page)
		} else {
			all.Nodes = append(all.Nodes, page.GetNodes()...)
			all.NextPageToken = page.GetNextPageToken()
		}
		if page.GetNextPageToken() == "" || o.PageSize > 0 {
			break
		}
		req.PageToken = page.GetNextPageToken()
	}
	if o.PageSize == 0 {
		all.NextPageToken = ""
	}
	return all, nil
}

// mergeNodes adds one group's answer to all. With several groups, totals
// add up, the oldest collection time is kept, and notices name their group.
func mergeNodes(all, res *cloudv1.ListNodeGroupNodesResponse, several bool, group string) {
	first := len(all.GetNodes()) == 0 && all.GetCollectedAt() == nil && all.GetLiveAt() == nil && all.GetTotal() == 0
	all.Nodes = append(all.Nodes, res.GetNodes()...)
	all.Total += res.GetTotal()
	all.NextPageToken = res.GetNextPageToken()
	if first {
		all.Complete, all.CollectedAt, all.LiveAt = res.GetComplete(), res.GetCollectedAt(), res.GetLiveAt()
	} else {
		all.Complete = all.Complete && res.GetComplete()
		if t := res.GetCollectedAt(); t != nil && (all.CollectedAt == nil || t.AsTime().Before(all.CollectedAt.AsTime())) {
			all.CollectedAt = t
		}
	}
	for _, n := range res.GetNotices() {
		if several {
			n = group + ": " + n
		}
		all.Notices = append(all.Notices, n)
	}
}

// printNodes writes when the rows are from, the notices, then a row per
// node. The resource columns show when the server reports them.
func printNodes(w io.Writer, res *cloudv1.ListNodeGroupNodesResponse, groups []string, wide bool) error {
	head := fmt.Sprintf("%d nodes", res.GetTotal())
	switch {
	case res.GetLiveAt() != nil:
		head += ", live as of " + res.GetLiveAt().AsTime().UTC().Format("2006-01-02 15:04:05") + " UTC"
	case res.GetCollectedAt() != nil:
		head += ", collected " + res.GetCollectedAt().AsTime().UTC().Format("2006-01-02 15:04:05") + " UTC"
	}
	if !res.GetComplete() && (res.GetLiveAt() != nil || res.GetCollectedAt() != nil) {
		head += " (incomplete)"
	}
	if _, err := fmt.Fprintln(w, head); err != nil {
		return err
	}
	for _, n := range res.GetNotices() {
		if _, err := fmt.Fprintln(w, "Notice: "+n); err != nil {
			return err
		}
	}
	if res.GetNextPageToken() != "" {
		if _, err := fmt.Fprintln(w, "Next page: --page-token "+res.GetNextPageToken()); err != nil {
			return err
		}
	}
	if len(res.GetNodes()) == 0 {
		return nil
	}
	resources := false
	for _, n := range res.GetNodes() {
		resources = resources || n.GetAllocatableCpuMillis() > 0
	}
	header := []string{"GROUP", "NAME", "STATUS", "INSTANCE TYPE", "ZONE", "CAPACITY", "KUBELET", "AGE", "NODECLAIM"}
	if resources {
		header = append(header, "CPU REQUESTED", "MEMORY REQUESTED", "PODS")
	}
	if wide {
		header = append(header, "DETAIL", "PROVIDER ID")
	}
	rows := make([][]string, 0, len(res.GetNodes()))
	for i, n := range res.GetNodes() {
		r := []string{groups[i], n.GetName(), nodeStatus(n), orNone(n.GetInstanceType()), orNone(n.GetZone()),
			orNone(n.GetCapacityType()), orNone(n.GetKubeletVersion()), age(n.GetCreatedAt()), nodeClaim(n)}
		if resources {
			r = append(r, share(n.GetRequestedCpuMillis(), n.GetAllocatableCpuMillis(), cpuText),
				share(n.GetRequestedMemoryBytes(), n.GetAllocatableMemoryBytes(), memoryText), nodePods(n))
		}
		if wide {
			r = append(r, orNone(nodeDetail(n)), orNone(n.GetProviderId()))
		}
		rows = append(rows, r)
	}
	return table(w, header, rows)
}

func nodeStatus(n *cloudv1.NodeGroupNode) string {
	switch {
	case !n.GetReady():
		return "NotReady"
	case n.GetUnschedulable():
		return "Ready,Cordoned"
	}
	return "Ready"
}

// nodeDetail is what the console shows under the status: the conditions of
// a node that is not Ready, else its cordon and taints.
func nodeDetail(n *cloudv1.NodeGroupNode) string {
	if !n.GetReady() {
		return strings.Join(n.GetConditions(), ", ")
	}
	if n.GetTaintsCount() > 0 {
		return fmt.Sprintf("%d taints", n.GetTaintsCount())
	}
	return ""
}

func nodeClaim(n *cloudv1.NodeGroupNode) string {
	if n.GetNodeclaimName() == "" {
		return none
	}
	if n.GetNodeclaimPhase() == "" {
		return n.GetNodeclaimName()
	}
	return n.GetNodeclaimName() + " (" + n.GetNodeclaimPhase() + ")"
}

// share is "requested/allocatable (percent)"; <none> without allocatable.
func share(requested, allocatable int64, text func(int64) string) string {
	if allocatable <= 0 {
		return none
	}
	return fmt.Sprintf("%s/%s (%d%%)", text(requested), text(allocatable), requested*100/allocatable)
}

func cpuText(m int64) string { return strconv.FormatInt(m, 10) + "m" }

func memoryText(b int64) string { return strconv.FormatInt(b>>20, 10) + "Mi" }

func nodePods(n *cloudv1.NodeGroupNode) string {
	s := strconv.Itoa(int(n.GetPodCount()))
	if n.GetAllocatablePods() > 0 {
		s += "/" + strconv.Itoa(int(n.GetAllocatablePods()))
	}
	if n.GetPodsWithoutRequests() > 0 {
		s += fmt.Sprintf(" (%d without requests)", n.GetPodsWithoutRequests())
	}
	return s
}

// nodeGroupManifests prints the YAML the platform applies for each group:
// its EC2NodeClass and NodePool (one group with --group).
func nodeGroupManifests(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	res, err := call(ctx, &cloudv1.ListClusterNodeGroupManifestsRequest{TenantId: o.Tenant, ClusterId: o.ID},
		c.Cluster.ListClusterNodeGroupManifests)
	if err != nil {
		return nil, err
	}
	out := res.(*cloudv1.ListClusterNodeGroupManifestsResponse)
	if o.NodeGroup != "" {
		var kept []*cloudv1.NodeGroupManifest
		for _, g := range out.GetNodeGroups() {
			if g.GetName() == o.NodeGroup {
				kept = append(kept, g)
			}
		}
		if len(kept) == 0 {
			return nil, fmt.Errorf("the cluster has no Karpenter node group %q", o.NodeGroup)
		}
		out = &cloudv1.ListClusterNodeGroupManifestsResponse{NodeGroups: kept}
	}
	if !isTable(o.Output) {
		return out, nil
	}
	return nil, printNodeGroupManifests(o.Out, out)
}

// printNodeGroupManifests writes the documents as one YAML stream, each
// group's EC2NodeClass then NodePool, ready for kubectl apply -f.
func printNodeGroupManifests(w io.Writer, res *cloudv1.ListClusterNodeGroupManifestsResponse) error {
	for _, g := range res.GetNodeGroups() {
		owner := "tenant"
		if g.GetPlatformManaged() {
			owner = "platform"
		}
		for _, doc := range []string{g.GetEc2NodeClassYaml(), g.GetNodePoolYaml()} {
			if doc == "" {
				continue
			}
			if _, err := fmt.Fprintf(w, "---\n# node group %s (%s)\n%s", g.GetName(), owner, strings.TrimLeft(doc, "-\n")); err != nil {
				return err
			}
			if !strings.HasSuffix(doc, "\n") {
				if _, err := fmt.Fprintln(w); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
