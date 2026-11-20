//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
)

// scenarioNetworkDelete deletes the network and checks that nothing of it
// is left in the VPC services of the account.
func (e *env) scenarioNetworkDelete(t *testing.T) {
	need(t, e.st.networkID != "", "no network (scenario 2)")
	if e.st.clusterID != "" {
		if _, err := e.getCluster(e.st.clusterID); !isCode(err, connect.CodeNotFound) {
			t.Skipf("cluster %s is still there (scenario 7 did not finish): the network cannot be deleted", e.st.clusterID)
		}
	}
	if err := e.deleteNetwork(t, e.st.networkID); err != nil {
		t.Fatalf("delete network: %v", err)
	}
	if _, err := e.getNetwork(e.st.networkID); !isCode(err, connect.CodeNotFound) {
		t.Errorf("network %s after deletion: %v, want NOT_FOUND", e.st.networkID, err)
	}
	if !e.haveAWS(t) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	tag := "Name=tag:" + labelRun + ",Values=" + e.runID
	// Labels are tags on every resource: nothing carrying the run's tag may remain.
	for _, q := range []struct{ service, call, list string }{
		{"ec2", "describe-vpcs", "Vpcs"},
		{"ec2", "describe-subnets", "Subnets"},
		{"ec2", "describe-internet-gateways", "InternetGateways"},
		{"ec2", "describe-route-tables", "RouteTables"},
		{"ec2", "describe-security-groups", "SecurityGroups"},
	} {
		out, err := e.awsJSON(ctx, e.cfg.Region, q.service, q.call, "--filters", tag)
		if err != nil {
			t.Errorf("%s: %v", q.call, err)
			continue
		}
		if left := asList(out[q.list]); len(left) > 0 {
			t.Errorf("%s still returns %d resources tagged %s=%s", q.call, len(left), labelRun, e.runID)
		}
	}
	nat, err := e.awsJSON(ctx, e.cfg.Region, "ec2", "describe-nat-gateways", "--filter", tag)
	if err != nil {
		t.Errorf("describe-nat-gateways: %v", err)
		return
	}
	for _, g := range asList(nat["NatGateways"]) {
		if state := asString(at(g, "State")); state != "deleted" {
			t.Errorf("NAT gateway %s is %s", asString(at(g, "NatGatewayId")), state)
		}
	}
}
