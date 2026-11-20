//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func (e *env) getCluster(id string) (*cloudv1.Cluster, error) {
	res, err := rpc(e, e.api.Cluster.GetCluster, &cloudv1.GetClusterRequest{TenantId: e.cfg.Tenant, ClusterId: id, Live: true})
	if err != nil {
		return nil, err
	}
	return res.GetCluster(), nil
}

func (e *env) getNetwork(id string) (*cloudv1.Network, error) {
	res, err := rpc(e, e.api.Network.GetNetwork, &cloudv1.GetNetworkRequest{TenantId: e.cfg.Tenant, NetworkId: id, Live: true})
	if err != nil {
		return nil, err
	}
	return res.GetNetwork(), nil
}

// waitCluster polls the live cluster, calling observe on every read, until
// done holds. FAILED ends it as an error, as it does in the CLI's --wait.
func (e *env) waitCluster(t *testing.T, what, id string, timeout time.Duration, observe func(*cloudv1.Cluster), done func(*cloudv1.Cluster) bool) (*cloudv1.Cluster, error) {
	t.Helper()
	var last *cloudv1.Cluster
	err := e.waitFor(t, what, timeout, func() (bool, string, error) {
		c, err := e.getCluster(id)
		if err != nil {
			return false, "", err
		}
		last = c
		st := c.GetStatus()
		if observe != nil {
			observe(c)
		}
		msg := fmt.Sprintf("%s: %s", st.GetState(), st.GetMessage())
		if st.GetState() == cloudv1.ClusterState_CLUSTER_STATE_FAILED {
			return false, msg, fmt.Errorf("cluster is FAILED: %s", st.GetMessage())
		}
		return done(c), msg, nil
	})
	if err != nil {
		e.dumpCluster(t, id)
	}
	return last, err
}

func isRunning(c *cloudv1.Cluster) bool {
	return c.GetStatus().GetState() == cloudv1.ClusterState_CLUSTER_STATE_RUNNING
}

// settled is RUNNING with the status at (or past) the generation of the last update.
func settled(generation int64) func(*cloudv1.Cluster) bool {
	return func(c *cloudv1.Cluster) bool {
		return isRunning(c) && c.GetStatus().GetObservedGeneration() >= generation
	}
}

// notReady lists the managed resources that are not READY.
func notReady(mrs []*cloudv1.ManagedResource) []string {
	var out []string
	for _, m := range mrs {
		if m.GetState() != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY {
			out = append(out, fmt.Sprintf("%s/%s=%s", m.GetKind(), m.GetName(), m.GetState()))
		}
	}
	return out
}

func managedOfKind(mrs []*cloudv1.ManagedResource, kind string) []*cloudv1.ManagedResource {
	var out []*cloudv1.ManagedResource
	for _, m := range mrs {
		if m.GetKind() == kind {
			out = append(out, m)
		}
	}
	return out
}

func condition(conds []*cloudv1.Condition, typ string) *cloudv1.Condition {
	for _, c := range conds {
		if c.GetType() == typ {
			return c
		}
	}
	return nil
}

// dumpCluster logs what helps to see why a step failed: the status, the
// resources that are not ready, the conditions and the recent events.
func (e *env) dumpCluster(t *testing.T, id string) {
	t.Helper()
	c, err := e.getCluster(id)
	if err != nil {
		t.Logf("--- cluster %s: cannot be read: %v", id, err)
		return
	}
	st := c.GetStatus()
	t.Logf("--- cluster %s (%s): %s: %s", c.GetName(), id, st.GetState(), st.GetMessage())
	for _, cond := range st.GetConditions() {
		t.Logf("    condition %s=%s %s: %s", cond.GetType(), cond.GetStatus(), cond.GetReason(), cond.GetMessage())
	}
	for _, m := range st.GetManagedResources() {
		if m.GetState() != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY {
			t.Logf("    resource %s/%s %s: %s %s", m.GetKind(), m.GetName(), m.GetState(), m.GetReason(), m.GetMessage())
		}
	}
	for _, p := range st.GetPlatformComponents() {
		if p.GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_READY {
			t.Logf("    component %s %s: %s %s", p.GetName(), p.GetPhase(), p.GetMessage(), p.GetLastError())
		}
	}
	if u := st.GetUpgrade(); u != nil {
		t.Logf("    upgrade %s -> %s %s: %s", u.GetFromVersion(), u.GetToVersion(), u.GetPhase(), u.GetMessage())
	}
	res, err := rpc(e, e.api.Cluster.ListClusterEvents, &cloudv1.ListClusterEventsRequest{TenantId: e.cfg.Tenant, ClusterId: id, PageSize: 30})
	if err != nil {
		t.Logf("    events: %v", err)
		for _, ev := range st.GetRecentEvents() {
			t.Logf("    event %s %s: %s", ev.GetType(), ev.GetReason(), ev.GetMessage())
		}
		return
	}
	for _, ev := range res.GetEvents() {
		t.Logf("    event %s %s %s/%s: %s", ev.GetType(), ev.GetReason(), ev.GetObjectKind(), ev.GetObjectName(), ev.GetMessage())
	}
}

func (e *env) dumpNetwork(t *testing.T, id string) {
	t.Helper()
	n, err := e.getNetwork(id)
	if err != nil {
		t.Logf("--- network %s: cannot be read: %v", id, err)
		return
	}
	st := n.GetStatus()
	t.Logf("--- network %s (%s): %s: %s", n.GetName(), id, st.GetState(), st.GetMessage())
	for _, cond := range st.GetConditions() {
		t.Logf("    condition %s=%s %s: %s", cond.GetType(), cond.GetStatus(), cond.GetReason(), cond.GetMessage())
	}
	for _, m := range st.GetManagedResources() {
		if m.GetState() != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY {
			t.Logf("    resource %s/%s %s: %s %s", m.GetKind(), m.GetName(), m.GetState(), m.GetReason(), m.GetMessage())
		}
	}
	if res, err := rpc(e, e.api.Network.ListNetworkEvents, &cloudv1.ListNetworkEventsRequest{TenantId: e.cfg.Tenant, NetworkId: id, PageSize: 30}); err == nil {
		for _, ev := range res.GetEvents() {
			t.Logf("    event %s %s %s/%s: %s", ev.GetType(), ev.GetReason(), ev.GetObjectKind(), ev.GetObjectName(), ev.GetMessage())
		}
	}
}

// deleteCluster stores the deletion of a cluster (once) and waits until the
// API no longer has it. An error says what was left.
func (e *env) deleteCluster(t *testing.T, id string, observe func(*cloudv1.Cluster)) error {
	t.Helper()
	c, err := e.getCluster(id)
	switch {
	case isCode(err, connect.CodeNotFound):
		return nil
	case err != nil:
		return err
	}
	if c.GetStatus().GetState() != cloudv1.ClusterState_CLUSTER_STATE_DELETING {
		if _, err := rpc(e, e.api.Cluster.DeleteCluster, &cloudv1.DeleteClusterRequest{TenantId: e.cfg.Tenant, ClusterId: id}); err != nil && !isCode(err, connect.CodeNotFound) {
			return fmt.Errorf("delete cluster %s: %w", id, err)
		}
	}
	err = e.waitFor(t, "cluster "+id+" deletion", e.cfg.DeleteTimeout, func() (bool, string, error) {
		c, err := e.getCluster(id)
		if isCode(err, connect.CodeNotFound) {
			return true, "gone", nil
		}
		if err != nil {
			return false, "", err
		}
		if observe != nil {
			observe(c)
		}
		return false, fmt.Sprintf("%s: %s", c.GetStatus().GetState(), c.GetStatus().GetMessage()), nil
	})
	if err != nil {
		e.dumpCluster(t, id)
	}
	return err
}

// deleteNetwork is deleteCluster for a network.
func (e *env) deleteNetwork(t *testing.T, id string) error {
	t.Helper()
	n, err := e.getNetwork(id)
	switch {
	case isCode(err, connect.CodeNotFound):
		return nil
	case err != nil:
		return err
	}
	if n.GetStatus().GetState() != cloudv1.NetworkState_NETWORK_STATE_DELETING {
		if _, err := rpc(e, e.api.Network.DeleteNetwork, &cloudv1.DeleteNetworkRequest{TenantId: e.cfg.Tenant, NetworkId: id}); err != nil && !isCode(err, connect.CodeNotFound) {
			return fmt.Errorf("delete network %s: %w", id, err)
		}
	}
	err = e.waitFor(t, "network "+id+" deletion", e.cfg.DeleteTimeout, func() (bool, string, error) {
		n, err := e.getNetwork(id)
		if isCode(err, connect.CodeNotFound) {
			return true, "gone", nil
		}
		if err != nil {
			return false, "", err
		}
		return false, fmt.Sprintf("%s: %s", n.GetStatus().GetState(), n.GetStatus().GetMessage()), nil
	})
	if err != nil {
		e.dumpNetwork(t, id)
	}
	return err
}

// cleanupCluster registers, on the parent test, the deletion of the cluster
// whose id is read when the cleanup runs, and the check that it completed.
func (e *env) cleanupCluster(parent *testing.T, id func() string) {
	parent.Cleanup(func() {
		if cid := id(); cid != "" {
			if err := e.deleteCluster(parent, cid, nil); err != nil {
				parent.Errorf("cleanup: cluster %s was not deleted: %v", cid, err)
			}
		}
	})
}

func (e *env) cleanupNetwork(parent *testing.T, id func() string) {
	parent.Cleanup(func() {
		if nid := id(); nid != "" {
			if err := e.deleteNetwork(parent, nid); err != nil {
				parent.Errorf("cleanup: network %s was not deleted: %v", nid, err)
			}
		}
	})
}

// stageOf reads "stage N of 7" from a deleting cluster's message.
func stageOf(message string) int {
	const marker = "stage "
	i := strings.Index(message, marker)
	if i < 0 {
		return 0
	}
	var n, of int
	if _, err := fmt.Sscanf(message[i:], "stage %d of %d", &n, &of); err != nil {
		return 0
	}
	return n
}
