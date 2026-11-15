package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/cloudformation"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// demoValues are what the demo's phase tapes (lib.sh fill) fill the demo manifests'
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

// fakeCLI is a cluster-vending stand-in for the demo's shell helpers:
// account demo-<run>, network demo-<run>-net (READY) and nothing else.
const fakeCLI = `#!/usr/bin/env bash
case "$1 $2" in
"account list") echo '{"accounts":[{"id":"acc-1","accountName":"demo-'"$RUN"'"}]}' ;;
"network list") echo '{"networks":[{"id":"net-1","name":"demo-'"$RUN"'-net"}]}' ;;
"network get") echo '{"network":{"status":{"state":"NETWORK_STATE_READY"}}}' ;;
"cluster list") echo '{"clusters":[]}' ;;
*) exit 1 ;;
esac
`

// demoShell runs script in bash at the repository root with lib.sh
// sourced, a fake cluster-vending first in PATH and the runs' state in
// stateDir, and returns its output.
func demoShell(t *testing.T, stateDir, script string, env ...string) string {
	t.Helper()
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "cluster-vending"), []byte(fakeCLI), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", "set -u; . hack/demo/lib.sh; "+script)
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"DEMO_STATE_DIR="+stateDir, "DEMO_POLL=0", "DEMO_RUN=", "DEMO_TENANT=", "T=", "ACCOUNT=", "NETWORK=", "CLUSTER=")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash: %v\n%s", err, out)
	}
	return string(out)
}

// A run's state is written by the phase that learns a value and read by
// the next; the run to act on is DEMO_RUN, else the newest unfinished one,
// else a new one.
func TestDemoRunState(t *testing.T) {
	dir := t.TempDir()

	got := demoShell(t, dir, `RUN=0101; state_set NETWORK "net 1"; state_set NETWORK net-2; unset NETWORK; state_load 0101; echo "net=$NETWORK"`)
	if !strings.Contains(got, "net=net-2") {
		t.Fatalf("state round trip: %q", got)
	}

	if got := strings.TrimSpace(demoShell(t, dir, `demo_run`)); got != "0101" {
		t.Errorf("demo_run = %q, want the unfinished run 0101", got)
	}
	if got := strings.TrimSpace(demoShell(t, dir, `demo_run`, "DEMO_RUN=0202")); got != "0202" {
		t.Errorf("demo_run with DEMO_RUN = %q, want 0202", got)
	}
	demoShell(t, dir, `RUN=0101; demo_finish`)
	if got := strings.TrimSpace(demoShell(t, dir, `demo_run`)); got == "0101" || !regexp.MustCompile(`^\d{8}$`).MatchString(got) {
		t.Errorf("demo_run after the run finished = %q, want a new MMDDHHMM id", got)
	}
}

// A phase run again resumes: a create refused because an earlier attempt
// made the object is not a failure, the object's id lands in the state,
// and the done phase is skipped (phase_done) and unblocks the next.
func TestDemoPhaseResumes(t *testing.T) {
	dir := t.TempDir()

	got := demoShell(t, dir, `demo_init; wait_network 2 1; phase_done network && echo network-done; phase_needs cluster`, "DEMO_RUN=0303")

	if !strings.Contains(got, "__step_2_done__") || !strings.Contains(got, "network-done") {
		t.Fatalf("resumed network phase:\n%s", got)
	}
	if strings.Contains(got, "demo-network") {
		t.Errorf("the cluster phase still asks for the network phase:\n%s", got)
	}
	state, err := os.ReadFile(filepath.Join(dir, "0303.env"))
	if err != nil || !strings.Contains(string(state), "NETWORK=net-1") || !strings.Contains(string(state), "T=demo-0303") {
		t.Fatalf("state = %q (%v), want NETWORK=net-1 and T=demo-0303", state, err)
	}
}

// A create that failed with nothing made fails the phase, with the reason
// in the phase's failed file; cleaning up a piece never made is done.
func TestDemoPhaseFailsAndCleanupTolerates(t *testing.T) {
	dir := t.TempDir()

	got := demoShell(t, dir, `demo_init; DEMO_PHASE=cluster; wait_cluster 3 1; wait_gone 6 1 cluster "" 60`, "DEMO_RUN=0404")

	if !strings.Contains(got, "__step_3_failed__") || !strings.Contains(got, "__step_6_done__") {
		t.Fatalf("output:\n%s", got)
	}
	failed, err := os.ReadFile(filepath.Join(dir, "0404-cluster-failed.txt"))
	if err != nil || !strings.Contains(string(failed), "cluster create exited 1") {
		t.Errorf("failed file = %q (%v)", failed, err)
	}
	if got := demoShell(t, dir, `demo_init; phase_needs upgrade`, "DEMO_RUN=0404"); !strings.Contains(got, "demo-cluster") {
		t.Errorf("the upgrade phase without a cluster says %q, want it to ask for demo-cluster", got)
	}
}
