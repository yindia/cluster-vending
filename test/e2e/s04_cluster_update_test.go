//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

const (
	maskLabels     = "labels"
	maskK8sVersion = "spec.control_plane.kubernetes_version"

	// platformPool is the Karpenter platform pool every cluster has next to
	// the system group.
	platformPool = "platform-pool"

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

// scenarioClusterUpdate changes the labels (tags, in place), checks the
// cluster has exactly the platform's node groups, and that an update naming
// a removed node group field is refused.
func (e *env) scenarioClusterUpdate(t *testing.T) {
	need(t, e.st.clusterID != "", "no cluster (scenario 3)")
	id := e.st.clusterID
	ctx, cancel := context.WithTimeout(context.Background(), e.cfg.UpdateTimeout+e.cfg.ScanTimeout)
	defer cancel()

	t.Run("labels_in_place", func(t *testing.T) { e.updateLabels(ctx, t, id) })
	t.Run("platform_node_groups", func(t *testing.T) { e.assertPlatformNodeGroups(t, id) })
	t.Run("node_group_fields_refused", func(t *testing.T) { e.assertNodeGroupFieldsRefused(t, id) })
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

// assertPlatformNodeGroups: status.nodeGroups is the system group and the
// platform pool, both platform managed, and the platform pool Ready.
func (e *env) assertPlatformNodeGroups(t *testing.T, id string) {
	err := e.waitFor(t, "platform pool READY", e.cfg.UpdateTimeout, func() (bool, string, error) {
		cur, err := e.getCluster(id)
		if err != nil {
			return false, "", err
		}
		g := nodeGroupStatus(cur, platformPool)
		if g == nil {
			return false, "no status yet", nil
		}
		return g.GetPhase() == cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY, fmt.Sprintf("%s: %s", g.GetPhase(), g.GetMessage()), nil
	})
	if err != nil {
		e.dumpCluster(t, id)
		t.Fatal(err)
	}
	c := mustCluster(e, id)
	var names []string
	for _, g := range c.GetStatus().GetNodeGroups() {
		names = append(names, g.GetName())
		if !g.GetPlatformManaged() {
			t.Errorf("node group %s is not platform managed", g.GetName())
		}
	}
	if !slices.Equal(names, []string{systemGroup, platformPool}) {
		t.Errorf("status.nodeGroups = %v, want [%s %s]", names, systemGroup, platformPool)
	}
}

// assertNodeGroupFieldsRefused: the node group fields are gone from the
// API; an update mask that names one is refused.
func (e *env) assertNodeGroupFieldsRefused(t *testing.T, id string) {
	for _, path := range []string{"spec.node_groups", "spec.default_node_pool.instance_types", "spec.aws.default_node_pool.ami_type"} {
		_, err := e.updateCluster(t, id, []string{path}, func(*cloudv1.Cluster) {})
		if !isCode(err, connect.CodeInvalidArgument) {
			t.Errorf("update of %s: %v, want INVALID_ARGUMENT", path, err)
		}
	}
}

func mustCluster(e *env, id string) *cloudv1.Cluster {
	c, err := e.getCluster(id)
	if err != nil {
		return nil
	}
	return c
}
