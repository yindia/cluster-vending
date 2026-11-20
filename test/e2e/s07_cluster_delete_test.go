//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// deleteStageCount is how many stages a cluster's deletion has
// (pkg/reconcile/cluster/delete_stage.go).
const deleteStageCount = 7

// firstSlowStage: the stages from here on take minutes (node groups, Karpenter,
// add-ons, the rest) and are always seen by a poll; the first three can pass
// within one reconcile on a cluster that has nothing in them.
const firstSlowStage = 4

// reservationLabel marks the ConfigMaps that reserve an EKS name.
const reservationLabel = "cloud.cluster-vending.io/eks-name-reservation=true"

// stageWatch records the deletion stages a cluster passes through.
type stageWatch struct {
	stages   []int
	teardown *cloudv1.ClusterTeardown
}

func (w *stageWatch) observe(c *cloudv1.Cluster) {
	if c.GetStatus().GetState() != cloudv1.ClusterState_CLUSTER_STATE_DELETING {
		return
	}
	if s := stageOf(c.GetStatus().GetMessage()); s > 0 && (len(w.stages) == 0 || w.stages[len(w.stages)-1] != s) {
		w.stages = append(w.stages, s)
	}
	if td := c.GetStatus().GetTeardown(); td != nil {
		w.teardown = td
	}
}

func (w *stageWatch) assert(t *testing.T, strict bool) {
	t.Helper()
	t.Logf("deletion stages seen: %v of %d", w.stages, deleteStageCount)
	for i := 1; i < len(w.stages); i++ {
		if w.stages[i] <= w.stages[i-1] {
			t.Errorf("deletion stage went from %d to %d: stages only advance", w.stages[i-1], w.stages[i])
		}
	}
	first := firstSlowStage
	if strict {
		first = 1
	}
	for s := first; s <= deleteStageCount; s++ {
		if !slices.Contains(w.stages, s) {
			t.Errorf("deletion stage %d of %d was never seen (seen %v)", s, deleteStageCount, w.stages)
		}
	}
	if td := w.teardown; td != nil {
		t.Logf("teardown: %s %s", td.GetResult(), td.GetMessage())
		if td.GetResult() == cloudv1.ClusterTeardownResult_CLUSTER_TEARDOWN_RESULT_TIMED_OUT {
			t.Errorf("the teardown timed out, leaving %v: %s", td.GetWaiting(), td.GetMessage())
		}
	}
}

// scenarioClusterDelete deletes the cluster through all its stages, checks
// that the EKS name reservation was released, creates the same name again,
// and deletes that too.
func (e *env) scenarioClusterDelete(t *testing.T) {
	need(t, e.st.clusterID != "", "no cluster (scenario 3)")
	id, name := e.st.clusterID, e.st.clusterName
	ctx, cancel := context.WithTimeout(context.Background(), 2*e.cfg.DeleteTimeout)
	defer cancel()

	profiles := e.instanceProfilesOf(ctx, t, name)
	t.Logf("instance profiles of %s before the deletion: %v (Karpenter makes them when it launches nodes)", name, profiles)

	w := &stageWatch{}
	if err := e.deleteCluster(t, id, w.observe); err != nil {
		t.Fatalf("delete cluster: %v", err)
	}
	w.assert(t, e.cfg.StrictStages)
	e.assertClusterGone(ctx, t, id, name)
	// Stage 5 takes the node role out of Karpenter's instance profiles and
	// deletes them through the tenant role; IAM refuses to delete a role a
	// profile still holds, so a missing permission shows as a stuck deletion
	// above, and a profile left behind shows here.
	if left := e.instanceProfilesOf(ctx, t, name); len(left) > 0 {
		t.Errorf("instance profiles of %s left after its deletion: %v", name, left)
	}

	// The same name again: the reservation was released.
	cl, err := e.clusterManifest(name, e.st.networkID)
	if err != nil {
		t.Fatal(err)
	}
	newID, err := e.createCluster(cl)
	if err != nil {
		t.Fatalf("re-create %s: %v (the EKS name reservation was not released?)", name, err)
	}
	e.st.clusterID = newID // the cleanup deletes whatever is there at the end
	t.Logf("cluster %s created again: %s", name, newID)
	done := func(c *cloudv1.Cluster) bool {
		if e.cfg.RecreateRunning {
			return isRunning(c)
		}
		// Past the create-time checks (the name is reserved, EKS has none of it) and rendered.
		return len(managedOfKind(c.GetStatus().GetManagedResources(), "Cluster")) > 0
	}
	if _, err := e.waitCluster(t, "re-created cluster "+newID, newID, e.cfg.ClusterTimeout, nil, done); err != nil {
		t.Fatal(err)
	}

	w2 := &stageWatch{}
	if err := e.deleteCluster(t, newID, w2.observe); err != nil {
		t.Fatalf("delete the re-created cluster: %v", err)
	}
	t.Logf("second deletion stages seen: %v", w2.stages)
	for i := 1; i < len(w2.stages); i++ {
		if w2.stages[i] <= w2.stages[i-1] {
			t.Errorf("second deletion: stage went from %d to %d", w2.stages[i-1], w2.stages[i])
		}
	}
	e.assertClusterGone(ctx, t, newID, name)
}

// assertClusterGone: the API has no trace of the cluster, EKS has none of
// that name, and no reservation names it.
func (e *env) assertClusterGone(ctx context.Context, t *testing.T, id, name string) {
	t.Helper()
	if _, err := e.getCluster(id); !isCode(err, connect.CodeNotFound) {
		t.Errorf("cluster %s after deletion: %v, want NOT_FOUND", id, err)
	}
	list, err := rpc(e, e.api.Cluster.ListClusters, &cloudv1.ListClustersRequest{TenantId: e.cfg.Tenant, PageSize: 200})
	if err != nil {
		t.Errorf("list clusters: %v", err)
	}
	for _, c := range list.GetClusters() {
		if c.GetId() == id {
			t.Errorf("the deleted cluster %s is still listed", id)
		}
	}
	if e.haveAWS(t) {
		if _, err := e.awsJSON(ctx, e.cfg.Region, "eks", "describe-cluster", "--name", name); err == nil {
			t.Errorf("EKS still has a cluster %s", name)
		} else if !awsNotFound(err) {
			t.Errorf("describe-cluster %s: %v", name, err)
		}
	}
	if !e.haveKube() {
		return
	}
	// Release is two-phase: the reservation is removed with the Cluster object.
	err = e.waitFor(t, "EKS name reservation of "+name+" released", 5*time.Minute, func() (bool, string, error) {
		obj, err := e.kubeJSON(ctx, "configmaps", "-n", "kube-system", "-l", reservationLabel)
		if err != nil {
			return false, "", err
		}
		var held []string
		for _, it := range asList(at(obj, "items")) {
			if strings.Contains(fmt.Sprint(at(it, "data")), name) {
				held = append(held, asString(at(it, "metadata", "name")))
			}
		}
		return len(held) == 0, fmt.Sprintf("reservations naming it: %v", held), nil
	})
	if err != nil {
		t.Error(err)
	}
}

// instanceProfilesOf lists the IAM instance profiles that carry the cluster's
// name in a tag (Karpenter tags what it makes with the cluster). Without
// credentials of the account it lists nothing.
func (e *env) instanceProfilesOf(ctx context.Context, t *testing.T, cluster string) []string {
	t.Helper()
	if !e.haveAWS(t) {
		return nil
	}
	all, err := e.awsJSON(ctx, "us-east-1", "iam", "list-instance-profiles")
	if err != nil {
		t.Errorf("list instance profiles: %v", err)
		return nil
	}
	var out []string
	for _, p := range asList(all["InstanceProfiles"]) {
		name := asString(at(p, "InstanceProfileName"))
		tags, err := e.awsJSON(ctx, "us-east-1", "iam", "list-instance-profile-tags", "--instance-profile-name", name)
		if err != nil {
			continue // deleted meanwhile
		}
		for _, tg := range asList(tags["Tags"]) {
			if asString(at(tg, "Value")) == cluster {
				out = append(out, name)
				break
			}
		}
	}
	return out
}
