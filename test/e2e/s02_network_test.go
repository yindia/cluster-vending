//go:build e2e

package e2e

import (
	"fmt"
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// scenarioNetworkCreate creates the network and waits until it is READY.
func (e *env) scenarioNetworkCreate(t *testing.T) {
	need(t, e.st.accountID != "", "no account (scenario 1)")
	need(t, e.cfg.Role == roleProvisioner, "only a provisioner account creates networks")

	e.st.networkName = e.runID + "-n"
	n, err := e.networkManifest(e.st.networkName)
	if err != nil {
		t.Fatal(err)
	}
	res, err := rpc(e, e.api.Network.CreateNetwork, &cloudv1.CreateNetworkRequest{TenantId: e.cfg.Tenant, Network: n, RequestId: newRequestID()})
	if err != nil {
		t.Fatalf("create network: %v", err)
	}
	e.st.networkID = res.GetNetwork().GetId()
	e.st.allNetworkIDs = append(e.st.allNetworkIDs, e.st.networkID)
	e.cleanupNetwork(e.parent, func() string { return e.st.networkID })
	t.Logf("network %s created: %s", e.st.networkName, e.st.networkID)

	var last *cloudv1.Network
	err = e.waitFor(t, "network "+e.st.networkID, e.cfg.NetworkTimeout, func() (bool, string, error) {
		got, err := e.getNetwork(e.st.networkID)
		if err != nil {
			return false, "", err
		}
		last = got
		st := got.GetStatus()
		msg := fmt.Sprintf("%s: %s (%d resources)", st.GetState(), st.GetMessage(), len(st.GetManagedResources()))
		if st.GetState() == cloudv1.NetworkState_NETWORK_STATE_FAILED {
			return false, msg, fmt.Errorf("network is FAILED: %s", st.GetMessage())
		}
		return st.GetState() == cloudv1.NetworkState_NETWORK_STATE_READY, msg, nil
	})
	if err != nil {
		e.dumpNetwork(t, e.st.networkID)
		t.Fatal(err)
	}

	st := last.GetStatus()
	if bad := notReady(st.GetManagedResources()); len(bad) > 0 {
		t.Errorf("network READY with resources not ready: %v", bad)
	}
	if len(st.GetManagedResources()) == 0 {
		t.Error("network READY with no managed resources")
	}
	if len(managedOfKind(st.GetManagedResources(), "VPC")) != 1 {
		t.Errorf("want exactly one VPC resource, got %d", len(managedOfKind(st.GetManagedResources(), "VPC")))
	}
	if cond := condition(st.GetConditions(), "Ready"); cond == nil || cond.GetStatus() != cloudv1.ConditionStatus_CONDITION_STATUS_TRUE {
		t.Errorf("condition Ready = %v, want True", cond)
	}
	if len(st.GetClusterIds()) != 0 {
		t.Errorf("a new network lists clusters: %v", st.GetClusterIds())
	}
}
