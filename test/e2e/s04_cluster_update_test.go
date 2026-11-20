//go:build e2e

package e2e

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

const (
	maskLabels     = "labels"
	maskNodeGroups = "spec.node_groups"
	maskK8sVersion = "spec.control_plane.kubernetes_version"

	labelPhase = "cv-e2e-phase"
)

// updateCluster is what `cluster update -f` does: read the cluster as it was
// requested, change it, ask for the plan (validate_only), and apply that plan
// with the etag the read returned.
func (e *env) updateCluster(t *testing.T, id string, mask []string, mutate func(*cloudv1.Cluster)) (*cloudv1.UpdateClusterResponse, error) {
	t.Helper()
	got, err := rpc(e, e.api.Cluster.GetCluster, &cloudv1.GetClusterRequest{TenantId: e.cfg.Tenant, ClusterId: id, AsRequested: true})
	if err != nil {
		return nil, err
	}
	cl := proto.Clone(got.GetCluster()).(*cloudv1.Cluster)
	cl.Status = nil
	mutate(cl)
	req := &cloudv1.UpdateClusterRequest{
		TenantId: e.cfg.Tenant, Cluster: cl, UpdateMask: &fieldmaskpb.FieldMask{Paths: mask}, ValidateOnly: true,
	}
	preview, err := rpc(e, e.api.Cluster.UpdateCluster, req)
	if err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}
	plan := preview.GetPlan()
	for _, ch := range plan.GetChanges() {
		t.Logf("plan: %s %s/%s %s replacement=%v %s", ch.GetAction(), ch.GetKind(), ch.GetName(), ch.GetSpecPath(), ch.GetRequiresReplacement(), ch.GetDetail())
	}
	for _, w := range plan.GetWarnings() {
		t.Logf("plan warning: %s", w)
	}
	if !plan.GetApplicable() {
		return nil, fmt.Errorf("the plan is not applicable: %v", plan.GetChanges())
	}
	req.ValidateOnly, req.PlanId = false, plan.GetPlanId()
	res, err := rpc(e, e.api.Cluster.UpdateCluster, req)
	if err != nil {
		return nil, fmt.Errorf("apply: %w", err)
	}
	return res, nil
}

// inPlace is whether a plan changes nothing destructively.
func inPlace(res *cloudv1.UpdateClusterResponse) []string {
	var bad []string
	for _, ch := range res.GetPlan().GetChanges() {
		if ch.GetRequiresReplacement() || ch.GetAction() == cloudv1.PlannedAction_PLANNED_ACTION_DISRUPTIVE || ch.GetAction() == cloudv1.PlannedAction_PLANNED_ACTION_DELETE {
			bad = append(bad, fmt.Sprintf("%s %s/%s", ch.GetAction(), ch.GetKind(), ch.GetName()))
		}
	}
	return bad
}

// scenarioClusterUpdate changes the labels (tags, in place), adds a user
// Karpenter node group with nodes, and removes it.
func (e *env) scenarioClusterUpdate(t *testing.T) {
	need(t, e.st.clusterID != "", "no cluster (scenario 3)")
	id := e.st.clusterID
	ctx, cancel := context.WithTimeout(context.Background(), e.cfg.UpdateTimeout+e.cfg.ScanTimeout)
	defer cancel()

	t.Run("labels_in_place", func(t *testing.T) { e.updateLabels(ctx, t, id) })
	t.Run("add_node_group", func(t *testing.T) { e.addNodeGroup(ctx, t, id) })
	t.Run("remove_node_group", func(t *testing.T) { e.removeNodeGroup(ctx, t, id) })
}

// groupIdentity is what changes when EKS replaces a node group: its creation time.
func (e *env) groupIdentity(ctx context.Context, t *testing.T, c *cloudv1.Cluster) string {
	t.Helper()
	mr := systemNodeGroup(c)
	if mr == nil || !e.haveAWS(t) {
		return ""
	}
	out, err := e.awsJSON(ctx, e.cfg.Region, "eks", "describe-nodegroup", "--cluster-name", c.GetName(), "--nodegroup-name", awsNodeGroupName(mr.GetExternalName()))
	if err != nil {
		t.Logf("describe node group: %v", err)
		return ""
	}
	return asString(at(out, "nodegroup", "createdAt"))
}

func (e *env) updateLabels(ctx context.Context, t *testing.T, id string) {
	before, err := e.getCluster(id)
	if err != nil {
		t.Fatal(err)
	}
	arn, created := before.GetStatus().GetAws().GetClusterArn(), e.groupIdentity(ctx, t, before)

	res, err := e.updateCluster(t, id, []string{maskLabels}, func(c *cloudv1.Cluster) {
		labels := map[string]string{}
		for k, v := range c.GetLabels() {
			labels[k] = v
		}
		labels[labelPhase] = "updated"
		c.Labels = labels
	})
	if err != nil {
		t.Fatalf("update labels: %v", err)
	}
	if bad := inPlace(res); len(bad) > 0 {
		t.Errorf("a label change is not in place: %v", bad)
	}
	after, err := e.waitCluster(t, "cluster after the label update", id, e.cfg.UpdateTimeout, nil, settled(res.GetCluster().GetGeneration()))
	if err != nil {
		t.Fatal(err)
	}
	if after.GetLabels()[labelPhase] != "updated" {
		t.Errorf("labels %v lack %s=updated", after.GetLabels(), labelPhase)
	}
	if got := after.GetStatus().GetAws().GetClusterArn(); got != arn {
		t.Errorf("the EKS cluster was replaced: ARN %s, was %s", got, arn)
	}
	if now := e.groupIdentity(ctx, t, after); now != created {
		t.Errorf("the system node group was replaced: created %q, was %q", now, created)
	}
	if e.haveAWS(t) {
		// The label has to be on the EKS cluster as a tag, applied in place.
		err := e.waitFor(t, "tag on the EKS cluster", 10*time.Minute, func() (bool, string, error) {
			out, err := e.awsJSON(ctx, e.cfg.Region, "eks", "describe-cluster", "--name", after.GetName())
			if err != nil {
				return false, "", err
			}
			tags := stringMap(at(out, "cluster", "tags"))
			return tags[labelPhase] == "updated", fmt.Sprintf("tags %v", tags), nil
		})
		if err != nil {
			t.Error(err)
		}
	}
}

// workloadKubeconfig writes the workload cluster's kubeconfig, read from the
// Secret on the management cluster, to a private temporary file.
func (e *env) workloadKubeconfig(ctx context.Context, t *testing.T, c *cloudv1.Cluster) (string, error) {
	t.Helper()
	if !e.haveKube() {
		return "", fmt.Errorf("no management cluster (CV_E2E_KUBECONTEXT)")
	}
	ns, name, ok := strings.Cut(c.GetStatus().GetKubeconfigSecret(), "/")
	if !ok {
		return "", fmt.Errorf("status.kubeconfigSecret %q is not <namespace>/<name>", c.GetStatus().GetKubeconfigSecret())
	}
	out, err := e.kubectl(ctx, "", "get", "secret", name, "-n", ns, "-o", "jsonpath={.data.kubeconfig}")
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(out)))
	if err != nil {
		return "", fmt.Errorf("decode the kubeconfig: %w", err)
	}
	path := filepath.Join(t.TempDir(), "kubeconfig")
	return path, os.WriteFile(path, raw, 0o600)
}

func workloadKubectl(ctx context.Context, kubeconfig, stdin string, args ...string) ([]byte, error) {
	return run(ctx, stdin, "kubectl", append([]string{"--kubeconfig", kubeconfig}, args...)...)
}

// pinnedDeployment is a pod that only the t3 group can run: Karpenter
// provisions a node for it.
const pinnedDeployment = `apiVersion: apps/v1
kind: Deployment
metadata: {name: cv-e2e-t3, namespace: default}
spec:
  replicas: 1
  selector: {matchLabels: {app: cv-e2e-t3}}
  template:
    metadata: {labels: {app: cv-e2e-t3}}
    spec:
      nodeSelector: {node.cluster-vending.io/pool: t3}
      containers:
        - name: pause
          image: registry.k8s.io/pause:3.9
          resources: {requests: {cpu: 100m, memory: 64Mi}}
`

func (e *env) addNodeGroup(ctx context.Context, t *testing.T, id string) {
	res, err := e.updateCluster(t, id, []string{maskNodeGroups}, func(c *cloudv1.Cluster) {
		c.Spec.NodeGroups = append(slices.Clone(c.GetSpec().GetNodeGroups()), userNodeGroupSpec())
	})
	if err != nil {
		t.Fatalf("add node group: %v", err)
	}
	c, err := e.waitCluster(t, "cluster after adding "+userNodeGroup, id, e.cfg.UpdateTimeout, nil, settled(res.GetCluster().GetGeneration()))
	if err != nil {
		t.Fatal(err)
	}
	err = e.waitFor(t, "node group "+userNodeGroup+" READY", e.cfg.UpdateTimeout, func() (bool, string, error) {
		cur, err := e.getCluster(id)
		if err != nil {
			return false, "", err
		}
		g := nodeGroupStatus(cur, userNodeGroup)
		if g == nil {
			return false, "no status yet", nil
		}
		if g.GetPhase() == cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_FAILED {
			return false, "", fmt.Errorf("node group %s FAILED: %s", userNodeGroup, g.GetMessage())
		}
		return g.GetPhase() == cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY, fmt.Sprintf("%s: %s", g.GetPhase(), g.GetMessage()), nil
	})
	if err != nil {
		e.dumpCluster(t, id)
		t.Fatal(err)
	}
	if g := nodeGroupStatus(c, userNodeGroup); g != nil && g.GetPlatformManaged() {
		t.Errorf("%s is marked platform managed", userNodeGroup)
	}
	kubeconfig, err := e.workloadKubeconfig(ctx, t, c)
	if err != nil {
		t.Logf("no nodes are requested (%v): only the NodePool is asserted", err)
		return
	}
	if _, err := workloadKubectl(ctx, kubeconfig, pinnedDeployment, "apply", "-f", "-"); err != nil {
		t.Fatalf("apply the pinned workload: %v", err)
	}
	res2, err := e.waitNodes(t, c, userNodeGroup, 1)
	if err != nil {
		e.dumpCluster(t, id)
		t.Fatalf("no node of %s: %v", userNodeGroup, err)
	}
	for _, n := range res2.GetNodes() {
		t.Logf("node %s %s %s %s claim %s %s", n.GetName(), n.GetInstanceType(), n.GetZone(), n.GetCapacityType(), n.GetNodeclaimName(), n.GetNodeclaimPhase())
		if !strings.HasPrefix(n.GetInstanceType(), "t3.") {
			t.Errorf("node %s is a %s, want t3", n.GetName(), n.GetInstanceType())
		}
		if n.GetNodeclaimName() == "" {
			t.Errorf("node %s has no NodeClaim", n.GetName())
		}
	}
	if g := nodeGroupStatus(mustCluster(e, id), userNodeGroup); g != nil && g.GetNodes() < 1 {
		t.Logf("status.nodeGroups counts %d nodes of %s (the count follows the node list)", g.GetNodes(), userNodeGroup)
	}
}

func mustCluster(e *env, id string) *cloudv1.Cluster {
	c, err := e.getCluster(id)
	if err != nil {
		return nil
	}
	return c
}

func (e *env) removeNodeGroup(ctx context.Context, t *testing.T, id string) {
	c, err := e.getCluster(id)
	if err != nil {
		t.Fatal(err)
	}
	if nodeGroupStatus(c, userNodeGroup) == nil {
		t.Skipf("no %s group to remove", userNodeGroup)
	}
	kubeconfig, kerr := e.workloadKubeconfig(ctx, t, c)
	if kerr == nil {
		if _, err := workloadKubectl(ctx, kubeconfig, "", "delete", "deployment", "cv-e2e-t3", "-n", "default", "--ignore-not-found"); err != nil {
			t.Logf("delete the pinned workload: %v", err)
		}
	}
	res, err := e.updateCluster(t, id, []string{maskNodeGroups}, func(c *cloudv1.Cluster) {
		c.Spec.NodeGroups = slices.DeleteFunc(slices.Clone(c.GetSpec().GetNodeGroups()), func(g *cloudv1.NodeGroupSpec) bool { return g.GetName() == userNodeGroup })
	})
	if err != nil {
		t.Fatalf("remove node group: %v", err)
	}
	if _, err := e.waitCluster(t, "cluster after removing "+userNodeGroup, id, e.cfg.UpdateTimeout, nil, settled(res.GetCluster().GetGeneration())); err != nil {
		t.Fatal(err)
	}
	err = e.waitFor(t, "node group "+userNodeGroup+" gone", e.cfg.UpdateTimeout, func() (bool, string, error) {
		cur, err := e.getCluster(id)
		if err != nil {
			return false, "", err
		}
		g := nodeGroupStatus(cur, userNodeGroup)
		if g != nil {
			return false, fmt.Sprintf("%s: %s", g.GetPhase(), g.GetMessage()), nil
		}
		return true, "gone", nil
	})
	if err != nil {
		e.dumpCluster(t, id)
		t.Fatal(err)
	}
	if kerr != nil {
		return
	}
	// Its NodePool and the nodes Karpenter made for it go with it.
	err = e.waitFor(t, "NodeClaims of "+userNodeGroup+" gone", e.cfg.UpdateTimeout, func() (bool, string, error) {
		out, err := workloadKubectl(ctx, kubeconfig, "", "get", "nodeclaims.karpenter.sh", "-l", "karpenter.sh/nodepool="+userNodeGroup, "-o", "name")
		if err != nil {
			return false, "", err
		}
		left := strings.TrimSpace(string(out))
		return left == "", fmt.Sprintf("left: %q", left), nil
	})
	if err != nil {
		t.Error(err)
	}
	if res, err := e.listNodes(id, userNodeGroup); err == nil && res.GetTotal() > 0 && !isCode(err, connect.CodeNotFound) {
		t.Logf("the node list still has %d nodes of %s (the next collection removes them)", res.GetTotal(), userNodeGroup)
	}
}
