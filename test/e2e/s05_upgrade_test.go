//go:build e2e

package e2e

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// upgradeRank orders the steps of an upgrade (docs/architecture.md, How a
// Kubernetes upgrade runs). NodesAndAddons is the old step that covered the
// node groups and the add-ons together.
func upgradeRank(p cloudv1.ClusterUpgradePhase) int {
	switch p {
	case cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_CONTROL_PLANE:
		return 1
	case cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_NODES_AND_ADDONS,
		cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_NODE_GROUPS:
		return 2
	case cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_ADDONS:
		return 3
	case cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_PLATFORM_COMPONENTS:
		return 4
	case cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_TENANT_COMPONENTS:
		return 5
	case cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_UNSPECIFIED:
		return 0
	default:
		return 0
	}
}

// upgradeWatch records the steps an upgrade goes through, one read at a time.
type upgradeWatch struct {
	phases     []cloudv1.ClusterUpgradePhase // in the order seen, each once in a row
	skipped    map[cloudv1.ClusterUpgradePhase]bool
	behindAt   map[cloudv1.ClusterUpgradePhase][]string // components not settled, per step
	halted     bool
	recoveries int32
	problems   []string
}

func newUpgradeWatch() *upgradeWatch {
	return &upgradeWatch{skipped: map[cloudv1.ClusterUpgradePhase]bool{}, behindAt: map[cloudv1.ClusterUpgradePhase][]string{}}
}

func (w *upgradeWatch) observe(c *cloudv1.Cluster) {
	u := c.GetStatus().GetUpgrade()
	if u == nil {
		return
	}
	if n := len(w.phases); n == 0 || w.phases[n-1] != u.GetPhase() {
		if n > 0 && upgradeRank(u.GetPhase()) < upgradeRank(w.phases[n-1]) {
			w.problems = append(w.problems, fmt.Sprintf("step went back from %s to %s", w.phases[n-1], u.GetPhase()))
		}
		w.phases = append(w.phases, u.GetPhase())
	}
	for _, s := range u.GetSkippedSteps() {
		w.skipped[s] = true
	}
	if len(u.GetComponentsBehind()) > 0 {
		w.behindAt[u.GetPhase()] = u.GetComponentsBehind()
	}
	w.halted = w.halted || u.GetHalted()
	w.recoveries = max(w.recoveries, u.GetRecoveries())
}

// scenarioClusterUpgrade moves the cluster one Kubernetes minor, manually,
// and asserts the steps and what they leave.
func (e *env) scenarioClusterUpgrade(t *testing.T) {
	need(t, e.st.clusterID != "", "no cluster (scenario 3)")
	id := e.st.clusterID
	c, err := e.getCluster(id)
	if err != nil {
		t.Fatal(err)
	}
	from, target := c.GetStatus().GetCurrentKubernetesVersion(), c.GetStatus().GetKubernetes().GetAvailable()
	if target == "" {
		t.Skipf("the cluster runs %s, and no later minor is on offer (status.kubernetes.available is empty): start it on an older minor with CV_E2E_K8S_FROM", from)
	}
	t.Logf("upgrading %s -> %s (channel %s offers %s)", from, target, c.GetStatus().GetKubernetes().GetChannel(), c.GetStatus().GetKubernetes().GetChannelMinor())

	res, err := e.updateCluster(t, id, []string{maskK8sVersion}, func(c *cloudv1.Cluster) {
		c.Spec.ControlPlane.KubernetesVersion = target
	})
	if err != nil {
		t.Fatalf("start the upgrade: %v", err)
	}
	e.st.upgradedTo = target
	gen := res.GetCluster().GetGeneration()
	w := newUpgradeWatch()
	final, err := e.waitCluster(t, "upgrade of "+id, id, e.cfg.UpgradeTimeout, w.observe, func(c *cloudv1.Cluster) bool {
		return settled(gen)(c) && c.GetStatus().GetUpgrade() == nil && c.GetStatus().GetCurrentKubernetesVersion() == target
	})
	if err != nil {
		t.Fatal(err)
	}
	e.assertUpgradeSteps(t, w)

	st := final.GetStatus()
	if st.GetCurrentKubernetesVersion() != target {
		t.Errorf("the control plane runs %s, want %s", st.GetCurrentKubernetesVersion(), target)
	}
	if bad := notReady(st.GetManagedResources()); len(bad) > 0 {
		t.Errorf("resources not ready after the upgrade: %v", bad)
	}
	err = e.waitFor(t, "platform components after the upgrade", e.cfg.ComponentsWait, func() (bool, string, error) {
		cur, err := e.getCluster(id)
		if err != nil {
			return false, "", err
		}
		var pending []string
		for _, p := range cur.GetStatus().GetPlatformComponents() {
			if p.GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_READY {
				pending = append(pending, p.GetName()+"="+p.GetPhase().String())
			}
		}
		return len(pending) == 0, fmt.Sprintf("not ready: %v", pending), nil
	})
	if err != nil {
		e.dumpCluster(t, id)
		t.Error(err)
	}
	// Every node runs the new minor (the node groups' step is settled when it does).
	res2, err := e.waitNodes(t, final, systemGroup, minSystemSize)
	if err != nil {
		t.Errorf("nodes after the upgrade: %v", err)
		return
	}
	for _, n := range res2.GetNodes() {
		if !strings.HasPrefix(n.GetKubeletVersion(), "v"+target+".") {
			t.Errorf("node %s runs %s, want v%s.x", n.GetName(), n.GetKubeletVersion(), target)
		}
	}
}

// assertUpgradeSteps: the steps came in order, the ones that do real work
// were seen, and a step with nothing to do was reported skipped.
func (e *env) assertUpgradeSteps(t *testing.T, w *upgradeWatch) {
	t.Helper()
	t.Logf("steps seen: %v; skipped: %v; behind: %v; recoveries: %d", w.phases, keys(w.skipped), w.behindAt, w.recoveries)
	for _, p := range w.problems {
		t.Errorf("upgrade: %s", p)
	}
	if w.halted {
		t.Error("the upgrade halted at some step")
	}
	for _, must := range []cloudv1.ClusterUpgradePhase{
		cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_CONTROL_PLANE,
		cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_ADDONS,
	} {
		if !slices.Contains(w.phases, must) {
			t.Errorf("the step %s was never seen (seen %v)", must, w.phases)
		}
	}
	if !slices.Contains(w.phases, cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_NODE_GROUPS) &&
		!slices.Contains(w.phases, cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_NODES_AND_ADDONS) {
		t.Errorf("no node group step was seen (seen %v)", w.phases)
	}
	// A component step either ran, with components behind at some read, or
	// was skipped because their versions did not change; both are reported.
	for _, step := range []cloudv1.ClusterUpgradePhase{
		cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_PLATFORM_COMPONENTS,
		cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_TENANT_COMPONENTS,
	} {
		switch {
		case w.skipped[step] && len(w.behindAt[step]) > 0:
			t.Errorf("%s is reported skipped (components unchanged) but %v were behind in it", step, w.behindAt[step])
		case w.skipped[step]:
			t.Logf("%s skipped: component versions unchanged", step)
		case slices.Contains(w.phases, step):
			t.Logf("%s ran", step)
		default:
			t.Errorf("%s was neither seen nor reported skipped", step)
		}
	}
}

func keys[K comparable](m map[K]bool) []K {
	out := make([]K, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
