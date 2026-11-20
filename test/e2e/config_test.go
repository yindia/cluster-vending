//go:build e2e

// Package e2e is the opt-in end-to-end harness: it drives a running API
// server and controller against a sandbox AWS account. It is skipped unless
// CV_E2E=1 (docs/architecture.md, "End-to-end on a sandbox account").
package e2e

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// enableVar switches the harness on; without it every scenario skips.
const enableVar = "CV_E2E"

// Roles the sandbox account's tenant role can be (cloudformation templates).
const (
	roleProvisioner = "provisioner"
	roleReader      = "reader"
	roleUpdater     = "updater"
)

// config is everything the harness reads from the environment.
type config struct {
	Server      string
	Token       string
	Tenant      string
	AccountID   string
	Region      string
	Role        string // provisioner, reader or updater
	RoleName    string // the IAM role's name: cluster-vending-<role> by default
	KubeContext string // management cluster; empty: no kubectl checks

	CIDR    string // the network's /16
	Channel string // release channel of the cluster, a name of the enum
	// K8sFrom is the Kubernetes minor the cluster is created on, one behind
	// what the upgrade scenario moves it to.
	K8sFrom string

	// Import scenario: an existing EKS cluster in an existing VPC.
	ImportCluster   string
	ImportVPC       string
	ImportSubnets   []string
	ImportNodeGroup string
	ImportTakeover  bool // take the imported cluster over: it is deleted with the test
	ImportDrift     bool // change the cluster in AWS and expect the takeover refusal
	ImportPlatform  bool // install platform components on takeover

	StrictStages bool // every one of the 7 deletion stages must be seen

	Poll            time.Duration
	NetworkTimeout  time.Duration
	ClusterTimeout  time.Duration
	ComponentsWait  time.Duration
	UpdateTimeout   time.Duration
	UpgradeTimeout  time.Duration
	DeleteTimeout   time.Duration
	ScanTimeout     time.Duration
	RequestTimeout  time.Duration
	RecreateRunning bool // the re-created cluster is waited on until RUNNING
}

// loadConfig reads the environment; the test is skipped unless CV_E2E=1
// and fails when a required variable is missing.
func loadConfig(t *testing.T) config {
	t.Helper()
	if os.Getenv(enableVar) != "1" {
		t.Skipf("end-to-end tests are off: set %s=1 (see docs/architecture.md, End-to-end on a sandbox account)", enableVar)
	}
	c := config{
		Server:          envOr("CV_E2E_SERVER", "http://127.0.0.1:8080"),
		Token:           os.Getenv("CV_E2E_TOKEN"),
		Tenant:          os.Getenv("CV_E2E_TENANT"),
		AccountID:       os.Getenv("CV_E2E_ACCOUNT_ID"),
		Region:          os.Getenv("CV_E2E_REGION"),
		Role:            envOr("CV_E2E_ROLE", roleProvisioner),
		KubeContext:     os.Getenv("CV_E2E_KUBECONTEXT"),
		CIDR:            envOr("CV_E2E_CIDR", "10.77.0.0/16"),
		Channel:         envOr("CV_E2E_CHANNEL", "RELEASE_CHANNEL_ALPHA"),
		K8sFrom:         envOr("CV_E2E_K8S_FROM", "1.35"),
		ImportCluster:   os.Getenv("CV_E2E_IMPORT_CLUSTER"),
		ImportVPC:       os.Getenv("CV_E2E_IMPORT_VPC"),
		ImportNodeGroup: envOr("CV_E2E_IMPORT_NODE_GROUP", "default"),
		ImportTakeover:  os.Getenv("CV_E2E_IMPORT_TAKEOVER") == "1",
		ImportDrift:     os.Getenv("CV_E2E_IMPORT_DRIFT") == "1",
		ImportPlatform:  os.Getenv("CV_E2E_IMPORT_PLATFORM") == "1",
		StrictStages:    os.Getenv("CV_E2E_STRICT_STAGES") == "1",
		RecreateRunning: os.Getenv("CV_E2E_RECREATE_RUNNING") == "1",
	}
	for _, s := range strings.Split(os.Getenv("CV_E2E_IMPORT_SUBNETS"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			c.ImportSubnets = append(c.ImportSubnets, s)
		}
	}
	c.RoleName = envOr("CV_E2E_ROLE_NAME", "cluster-vending-"+c.Role)
	for _, r := range []struct {
		name, val string
	}{{"CV_E2E_TENANT", c.Tenant}, {"CV_E2E_ACCOUNT_ID", c.AccountID}, {"CV_E2E_REGION", c.Region}} {
		if r.val == "" {
			t.Fatalf("%s is required", r.name)
		}
	}
	switch c.Role {
	case roleProvisioner, roleReader, roleUpdater:
	default:
		t.Fatalf("CV_E2E_ROLE %q: want provisioner, reader or updater", c.Role)
	}
	c.Poll = duration(t, "CV_E2E_POLL", 10*time.Second)
	c.RequestTimeout = duration(t, "CV_E2E_REQUEST_TIMEOUT", 60*time.Second)
	c.NetworkTimeout = duration(t, "CV_E2E_NETWORK_TIMEOUT", 30*time.Minute)
	c.ClusterTimeout = duration(t, "CV_E2E_CLUSTER_TIMEOUT", 60*time.Minute)
	c.ComponentsWait = duration(t, "CV_E2E_COMPONENTS_TIMEOUT", 45*time.Minute)
	c.UpdateTimeout = duration(t, "CV_E2E_UPDATE_TIMEOUT", 30*time.Minute)
	c.UpgradeTimeout = duration(t, "CV_E2E_UPGRADE_TIMEOUT", 120*time.Minute)
	c.DeleteTimeout = duration(t, "CV_E2E_DELETE_TIMEOUT", 60*time.Minute)
	c.ScanTimeout = duration(t, "CV_E2E_SCAN_TIMEOUT", 20*time.Minute)
	return c
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// duration reads a Go duration ("45m") or a number of minutes ("45").
func duration(t *testing.T, name string, def time.Duration) time.Duration {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Minute
	}
	t.Fatalf("%s=%q: want a duration such as 45m, or minutes", name, v)
	return 0
}

// namespace is the tenant's namespace on the management cluster.
func (c config) namespace() string { return fmt.Sprintf("tenant-%s", c.Tenant) }
