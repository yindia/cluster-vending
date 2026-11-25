//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The labels and taints the platform puts on the system node group
// (docs/architecture.md, Node layout). Nothing autoscales it: it has no
// cluster-autoscaler tags.
const (
	poolLabel           = "node.cluster-vending.io/pool"
	poolValue           = "system"
	tagAutoscalerPrefix = "k8s.io/cluster-autoscaler/"
	systemGroup         = "system"
	minSystemSize       = 2 // Karpenter runs two replicas
)

type taint struct{ key, value, effect string }

// ngFacts is a node group as one source (the management cluster's managed
// resource, or EKS itself) reports it.
type ngFacts struct {
	source            string
	labels, tags      map[string]string
	taints            []taint
	min, max, desired int
	found             map[string]bool // which fields the source had at all
}

func stringMap(v any) map[string]string {
	out := map[string]string{}
	if m, ok := v.(map[string]any); ok {
		for k, val := range m {
			out[k] = asString(val)
		}
	}
	return out
}

func intOf(v any) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

// factsFromMR reads status.atProvider of the NodeGroup managed resource:
// the controller's assumption is that the provider reports labels, taints,
// tags and the scaling there.
func factsFromMR(mr map[string]any) ngFacts {
	ap := at(mr, "status", "atProvider")
	f := ngFacts{source: "NodeGroup status.atProvider", labels: stringMap(at(ap, "labels")), tags: stringMap(at(ap, "tags")), found: map[string]bool{}}
	for _, key := range []string{"taint", "taints"} {
		for _, tv := range asList(at(ap, key)) {
			f.taints = append(f.taints, taint{asString(at(tv, "key")), asString(at(tv, "value")), asString(at(tv, "effect"))})
		}
	}
	sc := at(ap, "scalingConfig")
	f.min, f.max, f.desired = intOf(at(sc, "minSize")), intOf(at(sc, "maxSize")), intOf(at(sc, "desiredSize"))
	f.found["labels"], f.found["taints"] = len(f.labels) > 0, len(f.taints) > 0
	f.found["scaling"], f.found["tags"] = sc != nil, len(f.tags) > 0
	return f
}

// factsFromAWS reads `aws eks describe-nodegroup`.
func factsFromAWS(out map[string]any) ngFacts {
	ng := at(out, "nodegroup")
	f := ngFacts{source: "EKS DescribeNodegroup", labels: stringMap(at(ng, "labels")), tags: stringMap(at(ng, "tags")), found: map[string]bool{}}
	for _, tv := range asList(at(ng, "taints")) {
		f.taints = append(f.taints, taint{asString(at(tv, "key")), asString(at(tv, "value")), asString(at(tv, "effect"))})
	}
	sc := at(ng, "scalingConfig")
	f.min, f.max, f.desired = intOf(at(sc, "minSize")), intOf(at(sc, "maxSize")), intOf(at(sc, "desiredSize"))
	f.found["labels"], f.found["taints"], f.found["scaling"], f.found["tags"] = true, true, sc != nil, true
	return f
}

// assertSystemGroup asserts the system node group's static size (min =
// desired = max), labels and taints, and that it has no cluster-autoscaler
// tags.
func assertSystemGroup(t *testing.T, cluster string, f ngFacts) {
	t.Helper()
	t.Logf("system node group per %s: min=%d desired=%d max=%d labels=%v taints=%v tags=%v", f.source, f.min, f.desired, f.max, f.labels, f.taints, f.tags)
	if !f.found["scaling"] {
		t.Errorf("%s: no scalingConfig", f.source)
	} else if f.min < minSystemSize || f.max != f.min || f.desired != f.min {
		t.Errorf("%s: scaling min=%d desired=%d max=%d, want one static size of at least %d", f.source, f.min, f.desired, f.max, minSystemSize)
	}
	if f.labels[poolLabel] != poolValue {
		t.Errorf("%s: label %s=%q, want %q (labels %v)", f.source, poolLabel, f.labels[poolLabel], poolValue, f.labels)
	}
	hasTaint := false
	for _, tn := range f.taints {
		if tn.key == poolLabel && tn.value == poolValue && strings.Contains(strings.ToUpper(strings.ReplaceAll(tn.effect, "_", "")), "NOSCHEDULE") &&
			!strings.Contains(strings.ToUpper(tn.effect), "PREFER") {
			hasTaint = true
		}
	}
	if !hasTaint {
		t.Errorf("%s: no taint %s=%s:NoSchedule (taints %v)", f.source, poolLabel, poolValue, f.taints)
	}
	for key := range f.tags {
		if strings.HasPrefix(key, tagAutoscalerPrefix) {
			t.Errorf("%s: tag %s on cluster %s: nothing autoscales the system group", f.source, key, cluster)
		}
	}
}

// systemNodeGroup is the managed resource of the default (system) node group.
func systemNodeGroup(c *cloudv1.Cluster) *cloudv1.ManagedResource {
	var first *cloudv1.ManagedResource
	for _, m := range managedOfKind(c.GetStatus().GetManagedResources(), "NodeGroup") {
		if first == nil {
			first = m
		}
		if strings.HasSuffix(m.GetName(), "-default") {
			return m
		}
	}
	return first
}

// awsNodeGroupName is the node group's name in EKS: an external name is
// <cluster>:<group>.
func awsNodeGroupName(external string) string {
	if _, after, ok := strings.Cut(external, ":"); ok {
		return after
	}
	return external
}

// checkSystemGroup reads the system group from the management cluster and
// from EKS, whichever of them is reachable, and asserts it. It reports
// whether it could read at least one.
func (e *env) checkSystemGroup(ctx context.Context, t *testing.T, c *cloudv1.Cluster) bool {
	t.Helper()
	mr := systemNodeGroup(c)
	if mr == nil {
		t.Error("the cluster has no NodeGroup resource")
		return false
	}
	read := false
	if e.haveKube() {
		obj, err := e.kubeJSON(ctx, "nodegroups.eks.aws.m.upbound.io", mr.GetName(), "-n", e.cfg.namespace())
		if err != nil {
			t.Errorf("read the NodeGroup resource: %v", err)
		} else {
			f := factsFromMR(obj)
			for field, ok := range f.found {
				if !ok {
					t.Errorf("assumption violated: NodeGroup status.atProvider has no %s", field)
				}
			}
			assertSystemGroup(t, c.GetName(), f)
			read = true
		}
	}
	if e.haveAWS(t) {
		out, err := e.awsJSON(ctx, e.cfg.Region, "eks", "describe-nodegroup", "--cluster-name", c.GetName(), "--nodegroup-name", awsNodeGroupName(mr.GetExternalName()))
		if err != nil {
			t.Errorf("describe node group: %v", err)
		} else {
			assertSystemGroup(t, c.GetName(), factsFromAWS(out))
			read = true
		}
		e.assertNoOtherGroupAutoscaled(ctx, t, c.GetName(), awsNodeGroupName(mr.GetExternalName()))
	}
	return read
}

// assertNoOtherGroupAutoscaled: no other node group carries
// cluster-autoscaler tags either (the system group is checked by
// assertSystemGroup).
func (e *env) assertNoOtherGroupAutoscaled(ctx context.Context, t *testing.T, cluster, system string) {
	t.Helper()
	list, err := e.awsJSON(ctx, e.cfg.Region, "eks", "list-nodegroups", "--cluster-name", cluster)
	if err != nil {
		t.Errorf("list node groups: %v", err)
		return
	}
	for _, name := range asList(at(list, "nodegroups")) {
		n := asString(name)
		if n == system {
			continue
		}
		out, err := e.awsJSON(ctx, e.cfg.Region, "eks", "describe-nodegroup", "--cluster-name", cluster, "--nodegroup-name", n)
		if err != nil {
			t.Errorf("describe node group %s: %v", n, err)
			continue
		}
		for key := range stringMap(at(out, "nodegroup", "tags")) {
			if strings.HasPrefix(key, tagAutoscalerPrefix) {
				t.Errorf("node group %s carries %s: nothing autoscales managed node groups", n, key)
			}
		}
	}
}

// nodeGroupStatus finds a group in status.nodeGroups.
func nodeGroupStatus(c *cloudv1.Cluster, name string) *cloudv1.NodeGroupStatus {
	for _, g := range c.GetStatus().GetNodeGroups() {
		if g.GetName() == name {
			return g
		}
	}
	return nil
}

// listNodes is the whole node list of a group, from the results bucket.
func (e *env) listNodes(clusterID, group string) (*cloudv1.ListNodeGroupNodesResponse, error) {
	return rpc(e, e.api.Cluster.ListNodeGroupNodes, &cloudv1.ListNodeGroupNodesRequest{
		TenantId: e.cfg.Tenant, ClusterId: clusterID, NodeGroup: group, PageSize: 500,
	})
}

// waitNodes waits for the group's nodes to be listed from S3: at least
// want, all of them ready.
func (e *env) waitNodes(t *testing.T, c *cloudv1.Cluster, group string, want int) (*cloudv1.ListNodeGroupNodesResponse, error) {
	t.Helper()
	var last *cloudv1.ListNodeGroupNodesResponse
	err := e.waitFor(t, fmt.Sprintf("nodes of %s from the results bucket", group), e.cfg.ScanTimeout, func() (bool, string, error) {
		res, err := e.listNodes(c.GetId(), group)
		if err != nil {
			return false, "", err
		}
		last = res
		ready := 0
		for _, n := range res.GetNodes() {
			if n.GetReady() {
				ready++
			}
		}
		return int(res.GetTotal()) >= want && ready >= want && res.GetCollectedAt().IsValid(),
			fmt.Sprintf("total=%d ready=%d complete=%v notices=%v", res.GetTotal(), ready, res.GetComplete(), res.GetNotices()), nil
	})
	return last, err
}
