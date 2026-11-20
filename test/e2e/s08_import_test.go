//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// planLines logs the import plan and returns the lines that block.
func planLines(t *testing.T, imp *cloudv1.ImportStatus) (blocking []string) {
	t.Helper()
	for _, r := range imp.GetResources() {
		t.Logf("plan: %-14s %s/%s %s %s", r.GetDisposition(), r.GetKind(), r.GetName(), r.GetSpecPath(), r.GetDetail())
		if r.GetDisposition() == cloudv1.Disposition_DISPOSITION_BLOCKING {
			blocking = append(blocking, fmt.Sprintf("%s/%s: %s", r.GetKind(), r.GetName(), r.GetDetail()))
		}
	}
	return blocking
}

// waitNetworkImport waits as `network import --wait` does: until the plan is there.
func (e *env) waitNetworkImport(t *testing.T, id string) (*cloudv1.Network, error) {
	t.Helper()
	var last *cloudv1.Network
	err := e.waitFor(t, "network import "+id, e.cfg.NetworkTimeout, func() (bool, string, error) {
		n, err := e.getNetwork(id)
		if err != nil {
			return false, "", err
		}
		last = n
		st := n.GetStatus()
		msg := fmt.Sprintf("%s, import %s: %s", st.GetState(), st.GetImport().GetPhase(), st.GetMessage())
		if st.GetState() == cloudv1.NetworkState_NETWORK_STATE_FAILED {
			return false, msg, fmt.Errorf("network import FAILED: %s", st.GetMessage())
		}
		switch st.GetImport().GetPhase() {
		case cloudv1.ImportPhase_IMPORT_PHASE_BLOCKED:
			return false, msg, fmt.Errorf("network import BLOCKED: %v", planLines(t, st.GetImport()))
		case cloudv1.ImportPhase_IMPORT_PHASE_PLANNED:
			return len(st.GetManagedResources()) > 0, msg, nil
		default:
			return false, msg, nil
		}
	})
	return last, err
}

func (e *env) waitClusterImport(t *testing.T, id string) (*cloudv1.Cluster, error) {
	t.Helper()
	return e.waitCluster(t, "cluster import "+id, id, e.cfg.ClusterTimeout, nil, func(c *cloudv1.Cluster) bool {
		imp := c.GetStatus().GetImport()
		if imp.GetPhase() == cloudv1.ImportPhase_IMPORT_PHASE_BLOCKED {
			t.Errorf("the import is BLOCKED: %v", planLines(t, imp))
			return true
		}
		return imp.GetPhase() == cloudv1.ImportPhase_IMPORT_PHASE_PLANNED && len(c.GetStatus().GetManagedResources()) > 0
	})
}

// scenarioImport imports an existing VPC and EKS cluster read-only, checks
// the plan, and (opt-in) takes them over, including the refusal when the
// cluster changed in AWS after the import. It does nothing without
// CV_E2E_IMPORT_CLUSTER.
func (e *env) scenarioImport(t *testing.T) {
	need(t, e.cfg.ImportCluster != "", "CV_E2E_IMPORT_CLUSTER is not set")
	need(t, e.st.accountID != "", "no account (scenario 1)")
	if e.cfg.ImportVPC == "" || len(e.cfg.ImportSubnets) == 0 {
		t.Fatal("CV_E2E_IMPORT_VPC and CV_E2E_IMPORT_SUBNETS (comma separated) are required with CV_E2E_IMPORT_CLUSTER")
	}
	ctx, cancel := context.WithTimeout(context.Background(), e.cfg.ClusterTimeout+e.cfg.NetworkTimeout+e.cfg.UpdateTimeout)
	defer cancel()

	nreq, err := e.networkImportRequest()
	if err != nil {
		t.Fatal(err)
	}
	nres, err := rpc(e, e.api.Network.ImportNetwork, nreq)
	if err != nil {
		t.Fatalf("import network: %v", err)
	}
	e.st.importNetworkID = nres.GetNetwork().GetId()
	e.st.allNetworkIDs = append(e.st.allNetworkIDs, e.st.importNetworkID)
	e.cleanupNetwork(e.parent, func() string { return e.st.importNetworkID })
	n, err := e.waitNetworkImport(t, e.st.importNetworkID)
	if err != nil {
		e.dumpNetwork(t, e.st.importNetworkID)
		t.Fatal(err)
	}
	if bad := planLines(t, n.GetStatus().GetImport()); len(bad) > 0 {
		t.Fatalf("the network plan blocks: %v", bad)
	}
	if n.GetStatus().GetState() != cloudv1.NetworkState_NETWORK_STATE_IMPORTED {
		t.Errorf("an imported network is %s, want IMPORTED", n.GetStatus().GetState())
	}

	e.importCluster(t)
	c := mustCluster(e, e.st.importClusterID)
	if c.GetStatus().GetState() != cloudv1.ClusterState_CLUSTER_STATE_IMPORTED {
		t.Errorf("an imported cluster is %s, want IMPORTED", c.GetStatus().GetState())
	}
	if len(c.GetStatus().GetPlatformComponents()) != 0 {
		t.Errorf("an imported cluster has platform components before the takeover: %d", len(c.GetStatus().GetPlatformComponents()))
	}

	if e.cfg.Role == roleReader {
		// A reader may import and observe, nothing more.
		_, err := rpc(e, e.api.Network.TakeOverNetwork, &cloudv1.TakeOverNetworkRequest{TenantId: e.cfg.Tenant, NetworkId: e.st.importNetworkID, AcknowledgeNotImported: true})
		if !isCode(err, connect.CodeFailedPrecondition) {
			t.Errorf("reader: TakeOverNetwork: %v, want FAILED_PRECONDITION", err)
		}
		return
	}
	if e.cfg.ImportDrift {
		e.driftRefusal(ctx, t)
	}
	if !e.cfg.ImportTakeover {
		t.Log("takeover not requested (CV_E2E_IMPORT_TAKEOVER=1): the import is deleted without it, which touches nothing in AWS")
		return
	}
	e.takeOver(t)
}

// importCluster imports the EKS cluster and waits for its plan; the id is
// kept for the cleanup and the leak check.
func (e *env) importCluster(t *testing.T) {
	t.Helper()
	req, err := e.clusterImportRequest(e.st.importNetworkID)
	if err != nil {
		t.Fatal(err)
	}
	res, err := rpc(e, e.api.Cluster.ImportCluster, req)
	if err != nil {
		t.Fatalf("import cluster: %v", err)
	}
	if e.st.importClusterID == "" {
		e.cleanupCluster(e.parent, func() string { return e.st.importClusterID })
	}
	e.st.importClusterID = res.GetCluster().GetId()
	e.st.allClusterIDs = append(e.st.allClusterIDs, e.st.importClusterID)
	c, err := e.waitClusterImport(t, e.st.importClusterID)
	if err != nil {
		t.Fatal(err)
	}
	if bad := planLines(t, c.GetStatus().GetImport()); len(bad) > 0 {
		t.Fatalf("the cluster plan blocks: %v", bad)
	}
}

// takeOver makes the platform the owner of the imported network and cluster.
func (e *env) takeOver(t *testing.T) {
	t.Helper()
	if _, err := rpc(e, e.api.Network.TakeOverNetwork, &cloudv1.TakeOverNetworkRequest{
		TenantId: e.cfg.Tenant, NetworkId: e.st.importNetworkID, AcknowledgeNotImported: true,
	}); err != nil {
		t.Fatalf("take over the network: %v", err)
	}
	err := e.waitFor(t, "network takeover", e.cfg.NetworkTimeout, func() (bool, string, error) {
		n, err := e.getNetwork(e.st.importNetworkID)
		if err != nil {
			return false, "", err
		}
		st := n.GetStatus()
		if st.GetState() == cloudv1.NetworkState_NETWORK_STATE_FAILED {
			return false, "", fmt.Errorf("network FAILED: %s", st.GetMessage())
		}
		return st.GetState() == cloudv1.NetworkState_NETWORK_STATE_READY, fmt.Sprintf("%s: %s", st.GetState(), st.GetMessage()), nil
	})
	if err != nil {
		e.dumpNetwork(t, e.st.importNetworkID)
		t.Fatal(err)
	}
	if _, err := rpc(e, e.api.Cluster.TakeOverCluster, &cloudv1.TakeOverClusterRequest{
		TenantId: e.cfg.Tenant, ClusterId: e.st.importClusterID, AcknowledgeNotImported: true, SkipPlatformComponents: !e.cfg.ImportPlatform,
	}); err != nil {
		t.Fatalf("take over the cluster: %v", err)
	}
	c, err := e.waitCluster(t, "cluster takeover", e.st.importClusterID, e.cfg.ClusterTimeout, nil, isRunning)
	if err != nil {
		t.Fatal(err)
	}
	if bad := notReady(c.GetStatus().GetManagedResources()); len(bad) > 0 {
		t.Errorf("taken over with resources not ready: %v", bad)
	}
}

// endpointAccess reads the cluster's endpoint access from EKS.
func (e *env) endpointAccess(ctx context.Context) (private, public bool, err error) {
	out, err := e.awsJSON(ctx, e.cfg.Region, "eks", "describe-cluster", "--name", e.cfg.ImportCluster)
	if err != nil {
		return false, false, err
	}
	return at(out, "cluster", "resourcesVpcConfig", "endpointPrivateAccess") == true,
		at(out, "cluster", "resourcesVpcConfig", "endpointPublicAccess") == true, nil
}

func (e *env) setEndpointAccess(ctx context.Context, t *testing.T, private, public bool) error {
	t.Helper()
	cfg := fmt.Sprintf("endpointPrivateAccess=%t,endpointPublicAccess=%t", private, public)
	if _, err := run(ctx, "", "aws", "eks", "update-cluster-config", "--name", e.cfg.ImportCluster, "--region", e.cfg.Region, "--resources-vpc-config", cfg); err != nil {
		return err
	}
	// The update starts asynchronously: wait for it to finish.
	return e.waitFor(t, "EKS update", 30*time.Minute, func() (bool, string, error) {
		out, err := e.awsJSON(ctx, e.cfg.Region, "eks", "describe-cluster", "--name", e.cfg.ImportCluster)
		if err != nil {
			return false, "", err
		}
		status := asString(at(out, "cluster", "status"))
		p, pub := at(out, "cluster", "resourcesVpcConfig", "endpointPrivateAccess") == true, at(out, "cluster", "resourcesVpcConfig", "endpointPublicAccess") == true
		return status == "ACTIVE" && p == private && pub == public, fmt.Sprintf("%s private=%v public=%v", status, p, pub), nil
	})
}

// observedEndpointPrivate is what the platform's Observe-only Cluster
// resource says of the endpoint.
func (e *env) observedEndpointPrivate(ctx context.Context, c *cloudv1.Cluster) (bool, error) {
	mrs := managedOfKind(c.GetStatus().GetManagedResources(), "Cluster")
	if len(mrs) == 0 {
		return false, fmt.Errorf("the imported cluster has no Cluster resource")
	}
	obj, err := e.kubeJSON(ctx, "clusters.eks.aws.m.upbound.io", mrs[0].GetName(), "-n", e.cfg.namespace())
	if err != nil {
		return false, err
	}
	return at(obj, "status", "atProvider", "vpcConfig", "endpointPrivateAccess") == true, nil
}

// driftRefusal changes the imported cluster in AWS after the import and
// asserts that the takeover is refused (FAILED_PRECONDITION naming the
// difference), then puts it back, imports again and has a plan to review.
// It needs the aws CLI and the management cluster: the takeover is only
// attempted once the platform's own resource shows the change, because a
// takeover before that would succeed.
func (e *env) driftRefusal(ctx context.Context, t *testing.T) {
	t.Helper()
	if !e.haveAWS(t) || !e.haveKube() {
		t.Log("drift refusal skipped: it needs AWS credentials of the account and CV_E2E_KUBECONTEXT")
		return
	}
	priv, pub, err := e.endpointAccess(ctx)
	if err != nil {
		t.Fatalf("read the endpoint access: %v", err)
	}
	newPriv, newPub := priv, pub
	switch {
	case priv && pub:
		newPriv = false
	case !priv && pub:
		newPriv = true
	case priv && !pub:
		newPub = true
	default:
		t.Fatalf("the cluster has neither endpoint enabled (private=%v public=%v)", priv, pub)
	}
	reverted := false
	revert := func() {
		if reverted {
			return
		}
		reverted = true
		rctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
		defer cancel()
		if err := e.setEndpointAccess(rctx, t, priv, pub); err != nil {
			t.Errorf("restore the endpoint access of %s to private=%v public=%v: %v (do it by hand)", e.cfg.ImportCluster, priv, pub, err)
		}
	}
	t.Cleanup(revert)

	t.Logf("changing the endpoint access of %s in AWS: private %v->%v, public %v->%v", e.cfg.ImportCluster, priv, newPriv, pub, newPub)
	if err := e.setEndpointAccess(ctx, t, newPriv, newPub); err != nil {
		t.Fatalf("change the endpoint access: %v", err)
	}
	err = e.waitFor(t, "the platform to observe the change", e.cfg.UpdateTimeout, func() (bool, string, error) {
		got, err := e.observedEndpointPrivate(ctx, mustCluster(e, e.st.importClusterID))
		return err == nil && got == newPriv, fmt.Sprintf("observed private=%v (want %v) %v", got, newPriv, err), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = rpc(e, e.api.Cluster.TakeOverCluster, &cloudv1.TakeOverClusterRequest{
		TenantId: e.cfg.Tenant, ClusterId: e.st.importClusterID, AcknowledgeNotImported: true, SkipPlatformComponents: true,
	})
	switch {
	case err == nil:
		t.Fatal("the takeover was accepted although the cluster changed after the import: it must be refused")
	case !isCode(err, connect.CodeFailedPrecondition):
		t.Fatalf("the takeover of a drifted cluster: %v, want FAILED_PRECONDITION", err)
	case !strings.Contains(strings.ToLower(err.Error()), "endpoint"):
		t.Errorf("the refusal does not name the endpoint access: %v", err)
	}
	t.Logf("takeover refused as expected: %v", err)

	revert()
	// The refusal says to import again: delete the import (it owns nothing
	// in AWS) and import the cluster as it is now.
	if err := e.deleteCluster(t, e.st.importClusterID, nil); err != nil {
		t.Fatalf("delete the stale import: %v", err)
	}
	e.importCluster(t)
}
