//go:build e2e

package e2e

import (
	"testing"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"
)

// TestManifestsAreValid needs no AWS and no server: it checks that the
// manifests the scenarios send parse and pass the API's own validation, so
// a drift of the API shows here and not four hours into a run.
func TestManifestsAreValid(t *testing.T) {
	e := &env{
		runID: "cv-e2e-abc123",
		cfg: config{
			Tenant: "e2e", Region: "us-west-2", CIDR: "10.77.0.0/16", Channel: "RELEASE_CHANNEL_ALPHA", K8sFrom: "1.35",
			ImportCluster: "existing", ImportVPC: "vpc-0123456789abcdef0", ImportSubnets: []string{"subnet-0123456789abcdef0"},
			ImportNodeGroup: "default",
		},
	}
	e.st.accountID = "01a1d0a0-4a34-735f-be7d-19d1662f1138"
	const networkID = "01a1dfca-f5dd-7144-97b5-baeefe0e4a39"

	network, err := e.networkManifest(e.runID + "-n")
	if err != nil {
		t.Fatal(err)
	}
	cluster, err := e.clusterManifest(e.runID, networkID)
	if err != nil {
		t.Fatal(err)
	}
	netImport, err := e.networkImportRequest()
	if err != nil {
		t.Fatal(err)
	}
	clusterImport, err := e.clusterImportRequest(networkID)
	if err != nil {
		t.Fatal(err)
	}
	for name, msg := range map[string]proto.Message{
		"network": network, "cluster": cluster,
		"network import": netImport, "cluster import": clusterImport,
	} {
		if err := protovalidate.Validate(msg); err != nil {
			t.Errorf("%s manifest is invalid: %v", name, err)
		}
	}
}
