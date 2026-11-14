package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/cloudformation"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// demoValues are what hack/demo/demo.tape fills the demo manifests'
// placeholders with: a run id like the tape's default (MMDDHHMM), AWS's
// documentation account, and ids of the shape the API returns.
var demoValues = map[string]string{
	"__RUN__":          "01151430",
	"__TENANT__":       "demo-01151430",
	"__ACCOUNT_ID__":   tenantAccount,
	"__BOOTSTRAP_ID__": "0190a1b2-0000-7000-8000-000000000001",
	"__NETWORK_ID__":   "0190a1b2-0000-7000-8000-000000000002",
	"__REGION__":       "us-west-1",
	"__ZONE_A__":       "usw1-az1",
	"__ZONE_B__":       "usw1-az3",
}

var placeholder = regexp.MustCompile(`__[A-Z_]+__`)

// demoManifest is hack/demo/<name> with its placeholders filled, loaded
// as kind the way the CLI loads it.
func demoManifest(t *testing.T, name, kind string) *Manifest {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "hack", "demo", name))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for k, v := range demoValues {
		text = strings.ReplaceAll(text, k, v)
	}
	if left := placeholder.FindAllString(text, -1); len(left) > 0 {
		t.Fatalf("%s: placeholders the test does not fill (add them to demoValues and the tape): %v", name, left)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(path, kind)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return m
}

// The demo's manifests, filled the way the tape fills them, are valid
// requests: a broken demo manifest fails here, not 20 minutes into a
// recording. The run's role picks the provisioner template and fits IAM.
func TestDemoManifestsAreValid(t *testing.T) {
	tenant := demoValues["__TENANT__"]

	account, err := demoManifest(t, "cloudaccount.yaml", KindCloudAccount).CloudAccount()
	valid(t, "cloudaccount.yaml", &cloudv1.BootstrapTenantRequest{TenantId: tenant, Account: account}, err)
	if role := account.GetAws().GetRole(); cloudformation.RoleOf(role) != "provisioner" {
		t.Errorf("the demo role %s is not a name of the provisioner template", role)
	}
	valid(t, "cloudaccount.yaml onboarding", &cloudv1.GetAccountOnboardingRequest{TenantId: tenant, Role: "provisioner",
		RoleName: account.GetAws().GetRole(), AccountId: tenantAccount}, nil)

	network, err := demoManifest(t, "network.yaml", KindNetwork).Network()
	valid(t, "network.yaml", &cloudv1.CreateNetworkRequest{TenantId: tenant, Network: network}, err)

	cluster, err := demoManifest(t, "cluster.yaml", KindCluster).Cluster()
	valid(t, "cluster.yaml", &cloudv1.CreateClusterRequest{TenantId: tenant, Cluster: cluster}, err)
	if cluster.GetSpec().GetRegion() != network.GetSpec().GetRegion() {
		t.Errorf("the cluster is in %s, its network in %s", cluster.GetSpec().GetRegion(), network.GetSpec().GetRegion())
	}
}
