package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
)

const nodesCluster = "01a1d0a6-69d5-7e15-965e-fc6a53fa3d33"

// nodeService answers the node group RPCs: one node a page per group.
type nodeService struct {
	cloudv1connect.ClusterServiceClient
	groups    []string
	nodes     map[string][]*cloudv1.NodeGroupNode
	requests  []*cloudv1.ListNodeGroupNodesRequest
	manifests []*cloudv1.NodeGroupManifest
}

func (s *nodeService) GetCluster(_ context.Context, _ *cloudv1.GetClusterRequest) (*cloudv1.GetClusterResponse, error) {
	st := &cloudv1.ClusterStatus{}
	for _, g := range s.groups {
		st.NodeGroups = append(st.NodeGroups, &cloudv1.NodeGroupStatus{Name: g})
	}
	return &cloudv1.GetClusterResponse{Cluster: &cloudv1.Cluster{Id: nodesCluster, Status: st}}, nil
}

func (s *nodeService) ListNodeGroupNodes(_ context.Context, req *cloudv1.ListNodeGroupNodesRequest) (*cloudv1.ListNodeGroupNodesResponse, error) {
	s.requests = append(s.requests, req)
	all := s.nodes[req.GetNodeGroup()]
	res := &cloudv1.ListNodeGroupNodesResponse{Total: int32(len(all)), Complete: true,
		LiveAt: timestamppb.New(time.Date(2026, 11, 22, 4, 0, 0, 0, time.UTC))}
	i := 0
	if req.GetPageToken() == "next" {
		i = 1
	}
	if i < len(all) {
		res.Nodes = all[i : i+1]
	}
	if i+1 < len(all) {
		res.NextPageToken = "next"
	}
	return res, nil
}

func (s *nodeService) ListClusterNodeGroupManifests(context.Context, *cloudv1.ListClusterNodeGroupManifestsRequest) (*cloudv1.ListClusterNodeGroupManifestsResponse, error) {
	return &cloudv1.ListClusterNodeGroupManifestsResponse{NodeGroups: s.manifests}, nil
}

func node(name string, ready bool, cpu, cpuAlloc int64) *cloudv1.NodeGroupNode {
	return &cloudv1.NodeGroupNode{Name: name, Ready: ready, InstanceType: "t3.large", Zone: "us-east-1a", CapacityType: "on-demand",
		KubeletVersion: "v1.33.1", AllocatableCpuMillis: cpuAlloc, RequestedCpuMillis: cpu,
		AllocatableMemoryBytes: 8 << 30, RequestedMemoryBytes: 2 << 30, AllocatablePods: 35, PodCount: 12,
		Conditions: []string{"KubeletNotReady"}}
}

func TestClusterNodesReadsEveryGroupAndPage(t *testing.T) {
	svc := &nodeService{groups: []string{"system", "batch"}, nodes: map[string][]*cloudv1.NodeGroupNode{
		"system": {node("ip-1", true, 500, 1930), node("ip-2", false, 0, 1930)},
		"batch":  {node("ip-3", true, 1930, 1930)},
	}}
	var out bytes.Buffer
	err := Run(t.Context(), ClusterNodes, Clients{Cluster: svc},
		Options{Tenant: "acme", ID: nodesCluster, SortBy: "cpu-requested-percent", SortOrder: "desc", NameContains: "ip"}, &out, OutputTable)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(svc.requests) != 3 || svc.requests[1].GetPageToken() != "next" || svc.requests[2].GetNodeGroup() != "batch" {
		t.Fatalf("requests = %v", svc.requests)
	}
	r := svc.requests[0]
	if r.GetSortBy() != cloudv1.NodeSortBy_NODE_SORT_BY_CPU_REQUESTED_PERCENT || r.GetSortOrder() != cloudv1.SortOrder_SORT_ORDER_DESC ||
		r.GetNameContains() != "ip" || r.GetFilter() != cloudv1.NodeStatusFilter_NODE_STATUS_FILTER_ALL {
		t.Errorf("request = %v", r)
	}
	got := out.String()
	for _, want := range []string{"3 nodes, live as of 2026-11-22 04:00:00 UTC", "CPU REQUESTED", "system  ", "batch  ",
		"500m/1930m (25%)", "2048Mi/8192Mi (25%)", "12/35", "NotReady"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

func TestClusterNodesOneGroupOnePage(t *testing.T) {
	svc := &nodeService{nodes: map[string][]*cloudv1.NodeGroupNode{"batch": {node("a", true, 1, 2), node("b", true, 1, 2)}}}
	var out bytes.Buffer
	err := Run(t.Context(), ClusterNodes, Clients{Cluster: svc},
		Options{Tenant: "acme", ID: nodesCluster, NodeGroup: "batch", PageSize: 1, NotReady: true}, &out, OutputWide)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(svc.requests) != 1 || svc.requests[0].GetFilter() != cloudv1.NodeStatusFilter_NODE_STATUS_FILTER_NOT_READY {
		t.Fatalf("requests = %v", svc.requests)
	}
	if !strings.Contains(out.String(), "Next page: --page-token next") || !strings.Contains(out.String(), "PROVIDER ID") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestClusterNodesRefusals(t *testing.T) {
	for name, tt := range map[string]struct {
		o    Options
		want string
	}{
		"no cluster":             {Options{Tenant: "acme"}, "--id is required"},
		"both filters":           {Options{Tenant: "acme", ID: nodesCluster, Ready: true, NotReady: true}, "exclude each other"},
		"unknown sort":           {Options{Tenant: "acme", ID: nodesCluster, SortBy: "size"}, "want one of name, age, readiness, cpu-requested-percent"},
		"unknown order":          {Options{Tenant: "acme", ID: nodesCluster, SortOrder: "up"}, "want one of asc, desc"},
		"token without group":    {Options{Tenant: "acme", ID: nodesCluster, PageToken: "x"}, "--page-token needs --group"},
		"no group in the status": {Options{Tenant: "acme", ID: nodesCluster}, "reports no node group"},
	} {
		err := Run(t.Context(), ClusterNodes, Clients{Cluster: &nodeService{}}, tt.o, &bytes.Buffer{}, OutputTable)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tt.want)
		}
	}
}

func TestClusterNodesManifests(t *testing.T) {
	svc := &nodeService{
		manifests: []*cloudv1.NodeGroupManifest{
			{Name: "system", PlatformManaged: true},
			{Name: "platform-pool", PlatformManaged: true, Ec2NodeClassYaml: "kind: EC2NodeClass\n", NodePoolYaml: "kind: NodePool"},
		},
	}
	var out bytes.Buffer
	if err := Run(t.Context(), ClusterNodes, Clients{Cluster: svc}, Options{Tenant: "acme", ID: nodesCluster, Manifests: true, NodeGroup: "platform-pool"}, &out, OutputTable); err != nil {
		t.Fatalf("manifests: %v", err)
	}
	if got := out.String(); got != "---\n# node group platform-pool (platform)\nkind: EC2NodeClass\n---\n# node group platform-pool (platform)\nkind: NodePool\n" {
		t.Errorf("manifests = %q", got)
	}
	if err := Run(t.Context(), ClusterNodes, Clients{Cluster: svc}, Options{Tenant: "acme", ID: nodesCluster, Manifests: true, NodeGroup: "web"}, &out, OutputTable); err == nil {
		t.Error("an unknown group was accepted")
	}
}
