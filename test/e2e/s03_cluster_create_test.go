//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

const conditionPlatformReady = "PlatformComponentsReady"

// scenarioClusterCreate creates the cluster and asserts what a new cluster
// is: the install order, the platform components, the system node group,
// the cluster-autoscaler tags, the results bucket and the node lists.
func (e *env) scenarioClusterCreate(t *testing.T) {
	need(t, e.st.networkID != "", "no network (scenario 2)")

	e.st.clusterName = e.runID
	cl, err := e.clusterManifest(e.st.clusterName, e.st.networkID)
	if err != nil {
		t.Fatal(err)
	}
	id, err := e.createCluster(cl)
	if err != nil {
		t.Fatalf("create cluster: %v", err)
	}
	e.st.clusterID = id
	e.cleanupCluster(e.parent, func() string { return e.st.clusterID })
	t.Logf("cluster %s created: %s", e.st.clusterName, id)

	order := newInstallOrder()
	if _, err := e.waitCluster(t, "cluster "+id+" RUNNING", id, e.cfg.ClusterTimeout, order.observe, isRunning); err != nil {
		t.Fatal(err)
	}
	c, err := e.waitCluster(t, "platform components of "+id, id, e.cfg.ComponentsWait, order.observe, func(c *cloudv1.Cluster) bool {
		cond := condition(c.GetStatus().GetConditions(), conditionPlatformReady)
		return cond.GetStatus() == cloudv1.ConditionStatus_CONDITION_STATUS_TRUE && len(c.GetStatus().GetPlatformComponents()) > 0
	})
	if err != nil {
		t.Fatal(err)
	}

	e.assertInstallOrder(t, c, order)
	e.assertPlatformComponents(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if !e.checkSystemGroup(ctx, t, c) {
		t.Log("the system node group was checked by neither the management cluster nor AWS (set CV_E2E_KUBECONTEXT or AWS credentials)")
	}
	e.assertResultsBucket(ctx, t, c)
	e.assertEventBridge(t, c)
	e.assertNodeGroupStatus(t, c)
}

// assertEventBridge: Karpenter's interruption rules (EventBridge) exist and
// the provider has applied their current spec. The rules carry an explicit
// eventBusName, which the provider reads back on rules that already exist; a
// rule that never settles shows here as not synced to its generation.
func (e *env) assertEventBridge(t *testing.T, c *cloudv1.Cluster) {
	t.Helper()
	rules := managedOfKind(c.GetStatus().GetManagedResources(), "Rule")
	if len(rules) == 0 {
		t.Error("the cluster has no EventBridge Rule (Karpenter's interruption rules)")
	}
	for _, r := range rules {
		t.Logf("rule %s: %s generation %d observed %d", r.GetName(), r.GetState(), r.GetGeneration(), r.GetObservedGeneration())
		if r.GetState() != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY || r.GetObservedGeneration() < r.GetGeneration() {
			t.Errorf("rule %s is %s at generation %d of %d: %s", r.GetName(), r.GetState(), r.GetObservedGeneration(), r.GetGeneration(), r.GetMessage())
		}
	}
}

// createCluster stores a cluster and records its id for the leak check.
func (e *env) createCluster(cl *cloudv1.Cluster) (string, error) {
	res, err := rpc(e, e.api.Cluster.CreateCluster, &cloudv1.CreateClusterRequest{TenantId: e.cfg.Tenant, Cluster: cl, RequestId: newRequestID()})
	if err != nil {
		return "", err
	}
	id := res.GetCluster().GetId()
	e.st.allClusterIDs = append(e.st.allClusterIDs, id)
	return id, nil
}

func (e *env) assertInstallOrder(t *testing.T, c *cloudv1.Cluster, order *installOrder) {
	t.Helper()
	var names []string
	for _, p := range c.GetStatus().GetPlatformComponents() {
		names = append(names, fmt.Sprintf("%s@%s", p.GetName(), p.GetSince().AsTime().Format("15:04:05")))
	}
	t.Logf("platform components, with the time each entered its phase: %s", strings.Join(names, " "))
	for _, v := range order.verify(c) {
		t.Errorf("install order: %s", v)
	}
}

func (e *env) assertPlatformComponents(t *testing.T, c *cloudv1.Cluster) {
	t.Helper()
	st := c.GetStatus()
	have := map[string]bool{}
	for _, p := range st.GetPlatformComponents() {
		have[p.GetName()] = true
		if p.GetFailures() > 0 || p.GetHelmState() != "" && p.GetHelmState() != "deployed" {
			// A release that backed off and was resumed: the provider-helm unpause worked.
			t.Logf("platform component %s is READY after %d failed attempts (chart %s, helm %q, last error %q)", p.GetName(), p.GetFailures(), p.GetChart(), p.GetHelmState(), p.GetLastError())
		}
		if p.GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_READY {
			t.Errorf("platform component %s is %s: %s %s", p.GetName(), p.GetPhase(), p.GetMessage(), p.GetLastError())
		}
	}
	for _, want := range []string{karpenter, "cluster-autoscaler"} {
		if !have[want] {
			t.Errorf("platform component %s is missing (have %v)", want, have)
		}
	}
	if st.GetPlatformRelease() == "" {
		t.Error("status.platformRelease is empty")
	}
	if bad := notReady(st.GetManagedResources()); len(bad) > 0 {
		t.Errorf("managed resources not ready: %v", bad)
	}
	if st.GetEndpoint().GetUrl() == "" || st.GetKubeconfigSecret() == "" || st.GetOidcIssuerUrl() == "" {
		t.Errorf("status lacks the endpoint (%q), kubeconfig secret (%q) or OIDC issuer (%q)", st.GetEndpoint().GetUrl(), st.GetKubeconfigSecret(), st.GetOidcIssuerUrl())
	}
}

// assertResultsBucket: the cluster's S3 bucket for scan results exists.
func (e *env) assertResultsBucket(ctx context.Context, t *testing.T, c *cloudv1.Cluster) {
	t.Helper()
	buckets := managedOfKind(c.GetStatus().GetManagedResources(), "Bucket")
	if len(buckets) == 0 {
		t.Error("the cluster has no results Bucket resource")
		return
	}
	b := buckets[0]
	if b.GetState() != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY || b.GetExternalName() == "" {
		t.Errorf("results bucket %s: %s, external name %q", b.GetName(), b.GetState(), b.GetExternalName())
		return
	}
	if !strings.HasPrefix(b.GetExternalName(), "cv-") {
		t.Errorf("results bucket %q is not named cv-*: the server reads only those", b.GetExternalName())
	}
	if !e.haveAWS(t) {
		return
	}
	if _, err := run(ctx, "", "aws", "s3api", "head-bucket", "--bucket", b.GetExternalName(), "--region", e.cfg.Region); err != nil {
		t.Errorf("results bucket does not exist in S3: %v", err)
		return
	}
	pab, err := e.awsJSON(ctx, e.cfg.Region, "s3api", "get-public-access-block", "--bucket", b.GetExternalName())
	if err != nil {
		t.Errorf("public access block of the results bucket: %v", err)
		return
	}
	for _, k := range []string{"BlockPublicAcls", "IgnorePublicAcls", "BlockPublicPolicy", "RestrictPublicBuckets"} {
		if at(pab, "PublicAccessBlockConfiguration", k) != true {
			t.Errorf("results bucket: %s is not true", k)
		}
	}
}

// assertNodeGroupStatus: the group's status, and its nodes listed from S3.
func (e *env) assertNodeGroupStatus(t *testing.T, c *cloudv1.Cluster) {
	t.Helper()
	var g *cloudv1.NodeGroupStatus
	err := e.waitFor(t, "system node group status", e.cfg.ScanTimeout, func() (bool, string, error) {
		cur, err := e.getCluster(c.GetId())
		if err != nil {
			return false, "", err
		}
		g = nodeGroupStatus(cur, systemGroup)
		if g == nil {
			return false, "no status for the system group yet", nil
		}
		return g.GetPhase() == cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY && g.GetNodes() >= minSystemSize,
			fmt.Sprintf("%s nodes=%d %s", g.GetPhase(), g.GetNodes(), g.GetMessage()), nil
	})
	if err != nil {
		t.Errorf("%v", err)
		return
	}
	if !g.GetPlatformManaged() {
		t.Error("the system group is not platform managed")
	}
	if g.GetNodesNotice() != "" {
		t.Errorf("the node list is unavailable: %s", g.GetNodesNotice())
	}
	res, err := e.waitNodes(t, c, systemGroup, minSystemSize)
	if err != nil {
		t.Errorf("ListNodeGroupNodes: %v", err)
		return
	}
	for _, n := range res.GetNodes() {
		t.Logf("node %s %s %s %s kubelet %s taints=%d", n.GetName(), n.GetInstanceType(), n.GetZone(), n.GetCapacityType(), n.GetKubeletVersion(), n.GetTaintsCount())
		if n.GetKubeletVersion() == "" || n.GetInstanceType() == "" || n.GetProviderId() == "" {
			t.Errorf("node %s is listed without its kubelet version, instance type or provider id", n.GetName())
		}
		if n.GetTaintsCount() < 1 {
			t.Errorf("system node %s has no taint", n.GetName())
		}
		if n.GetNodeclaimName() != "" {
			t.Errorf("system node %s has a NodeClaim %q: it is not Karpenter's", n.GetName(), n.GetNodeclaimName())
		}
	}
	if int32(len(res.GetNodes())) != res.GetTotal() {
		t.Errorf("listed %d nodes of %d (a page of 500)", len(res.GetNodes()), res.GetTotal())
	}
	if !res.GetComplete() {
		t.Errorf("the node list is incomplete: %v", res.GetNotices())
	}
}
