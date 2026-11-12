package cli

import (
	"path/filepath"
	"testing"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// exampleTenant is the tenant every example belongs to.
const exampleTenant = "acme"

// example loads examples/<name> as kind, the way the CLI does, and checks
// it is tenant acme's.
func example(t *testing.T, name, kind string) *Manifest {
	t.Helper()
	m, err := LoadManifest(filepath.Join("..", "..", "examples", name), kind)
	if err != nil {
		t.Fatal(err)
	}
	if tenant, err := m.tenant(""); err != nil || tenant != exampleTenant {
		t.Fatalf("%s: tenant %q (%v), want %s: the examples are one tenant's", name, tenant, err, exampleTenant)
	}
	return m
}

// valid checks a request the CLI would send against the contract's rules.
func valid(t *testing.T, name string, req proto.Message, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if err := protovalidate.Validate(req); err != nil {
		t.Errorf("%s: %v", name, err)
	}
}

// The examples are one chain, followed in the README's walkthrough: an
// account, a network in it, a cluster in the network; and an imported
// network with an imported cluster. Each is a valid request, and each
// refers to the previous step by the same placeholder id.
func TestExamplesAreOneChain(t *testing.T) {
	m := example(t, "cloudaccount.yaml", KindCloudAccount)
	account, err := m.CloudAccount()
	valid(t, "cloudaccount.yaml", &cloudv1.BootstrapTenantRequest{TenantId: exampleTenant, Account: account}, err)

	m = example(t, "network.yaml", KindNetwork)
	network, err := m.Network()
	valid(t, "network.yaml", &cloudv1.CreateNetworkRequest{TenantId: exampleTenant, Network: network}, err)

	m = example(t, "cluster.yaml", KindCluster)
	cluster, err := m.Cluster()
	valid(t, "cluster.yaml", &cloudv1.CreateClusterRequest{TenantId: exampleTenant, Cluster: cluster}, err)

	m = example(t, "network-import.yaml", KindNetworkImport)
	networkImport, err := m.NetworkImport()
	if err == nil {
		networkImport.TenantId = exampleTenant
	}
	valid(t, "network-import.yaml", networkImport, err)

	m = example(t, "cluster-import.yaml", KindClusterImport)
	clusterImport, err := m.ClusterImport()
	if err == nil {
		clusterImport.TenantId = exampleTenant
	}
	valid(t, "cluster-import.yaml", clusterImport, err)

	const bootstrapID, networkID, importedNetworkID = "00000000-0000-0000-0000-000000000001",
		"00000000-0000-0000-0000-000000000002", "00000000-0000-0000-0000-000000000003"
	for name, got := range map[string]string{
		"network.yaml spec.bootstrapId":        network.GetSpec().GetBootstrapId(),
		"network-import.yaml spec.bootstrapId": networkImport.GetBootstrapId(),
	} {
		if got != bootstrapID {
			t.Errorf("%s = %s, want the BOOTSTRAP_ID placeholder %s", name, got, bootstrapID)
		}
	}
	if got := cluster.GetSpec().GetNetwork().GetNetworkId(); got != networkID {
		t.Errorf("cluster.yaml spec.network.networkId = %s, want the NETWORK_ID placeholder %s", got, networkID)
	}
	if got := clusterImport.GetNetworkId(); got != importedNetworkID {
		t.Errorf("cluster-import.yaml spec.networkId = %s, want the IMPORTED_NETWORK_ID placeholder %s", got, importedNetworkID)
	}
	if cluster.GetSpec().GetRegion() != network.GetSpec().GetRegion() {
		t.Errorf("the cluster is in %s, its network in %s", cluster.GetSpec().GetRegion(), network.GetSpec().GetRegion())
	}
}
