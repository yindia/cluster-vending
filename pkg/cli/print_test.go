package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// printNow is the clock of the printer tests: ages are counted from it.
var printNow = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

// withClock makes the printers count ages from printNow.
func withClock(t *testing.T) {
	t.Helper()
	before := clock
	clock = func() time.Time { return printNow }
	t.Cleanup(func() { clock = before })
}

func ago(d time.Duration) *timestamppb.Timestamp { return timestamppb.New(printNow.Add(-d)) }

func printed(t *testing.T, msg proto.Message, format string) string {
	t.Helper()
	var out bytes.Buffer
	if err := Print(&out, msg, format); err != nil {
		t.Fatalf("Print: %v", err)
	}
	return out.String()
}

func testCluster() *cloudv1.Cluster {
	return &cloudv1.Cluster{
		TenantId: "acme", Id: "01a1d2f6-12a9-7505-b7b2-8b8acfcab306", Name: "prod-eks", CreateTime: ago(50 * time.Hour),
		Spec: &cloudv1.ClusterSpec{Region: "us-west-1",
			Network:         &cloudv1.ClusterNetwork{NetworkId: "01a1d0a1-3333-7766-a4ff-84f3f49a774f"},
			ControlPlane:    &cloudv1.ControlPlaneSpec{KubernetesVersion: "1.37"},
			DefaultNodePool: &cloudv1.NodePoolSpec{InstanceTypes: []string{"t3.large"}, ScalingConfig: &cloudv1.ScalingConfig{MinSize: 2, MaxSize: 4}},
			MaintenanceWindow: &cloudv1.MaintenanceWindow{Days: []cloudv1.Weekday{cloudv1.Weekday_WEEKDAY_SATURDAY, cloudv1.Weekday_WEEKDAY_SUNDAY},
				Start: "02:00", Duration: "4h"},
			ReleaseChannel: cloudv1.ReleaseChannel_RELEASE_CHANNEL_ALPHA, Tier: cloudv1.ClusterTier_CLUSTER_TIER_2,
			UpgradeMode: cloudv1.UpgradeMode_UPGRADE_MODE_AUTOMATIC},
		Status: &cloudv1.ClusterStatus{
			State: cloudv1.ClusterState_CLUSTER_STATE_RUNNING, CurrentKubernetesVersion: "1.37",
			Endpoint:        &cloudv1.ClusterEndpoint{Url: "https://ABC.sk1.us-west-1.eks.amazonaws.com"},
			PlatformRelease: "shipped-f874cd2c",
			ManagedResources: []*cloudv1.ManagedResource{
				{Kind: "Cluster", Name: "prod-eks-f772324f", ExternalName: "prod-eks", State: cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY,
					Synced: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE, Ready: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE},
				{Kind: "NodeGroup", Name: "prod-eks-f772324f-default", State: cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_CREATING,
					Synced: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE, Ready: cloudv1.ConditionStatus_CONDITION_STATUS_FALSE, Message: "creating\nnode group"},
			},
			Conditions: []*cloudv1.Condition{{Type: "Ready", Status: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "AllResourcesReady"}},
			Provider: &cloudv1.ClusterStatus_Aws{Aws: &cloudv1.AwsClusterStatus{Addons: []*cloudv1.AwsAddonStatus{
				{Name: "coredns", Version: "v1.14.7-eksbuild.35", State: cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY},
				{Name: "aws-ebs-csi-driver", Version: "v1.68.0-eksbuild.1", State: cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_FAILED,
					Message: "ConfigurationConflict"}}}},
			PlatformComponents: []*cloudv1.PlatformComponent{
				{Name: "karpenter", Version: "1.15.1", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_READY, Since: ago(2 * time.Hour)}},
			RecentEvents: []*cloudv1.Event{{Time: ago(5 * time.Minute), Type: cloudv1.EventType_EVENT_TYPE_NORMAL, Reason: "Running",
				Message: "the cluster runs its latest generation"}},
		},
	}
}

func TestPrintClusterList(t *testing.T) {
	withClock(t)
	list := &cloudv1.ListClustersResponse{Clusters: []*cloudv1.Cluster{testCluster()}}
	tests := []struct {
		name, format, want string
	}{
		{"table", OutputTable,
			"NAME       ID                                     PHASE     VERSION   REGION      AGE\n" +
				"prod-eks   01a1d2f6-12a9-7505-b7b2-8b8acfcab306   Running   1.37      us-west-1   2d2h\n"},
		{"wide", OutputWide,
			"NAME       ID                                     PHASE     VERSION   REGION      AGE    NETWORK                                NODE POOL        RELEASE            MESSAGE\n" +
				"prod-eks   01a1d2f6-12a9-7505-b7b2-8b8acfcab306   Running   1.37      us-west-1   2d2h   01a1d0a1-3333-7766-a4ff-84f3f49a774f   t3.large (2-4)   shipped-f874cd2c   <none>\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := printed(t, list, tt.format); got != tt.want {
				t.Fatalf("got\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

func TestPrintClusterDescribe(t *testing.T) {
	withClock(t)

	got := printed(t, &cloudv1.GetClusterResponse{Cluster: testCluster()}, OutputTable)

	want := "Name:          prod-eks\n" +
		"ID:            01a1d2f6-12a9-7505-b7b2-8b8acfcab306\n" +
		"Tenant:        acme\n" +
		"Phase:         Running\n" +
		"Region:        us-west-1\n" +
		"Network:       01a1d0a1-3333-7766-a4ff-84f3f49a774f\n" +
		"Version:       1.37\n" +
		"Endpoint:      https://ABC.sk1.us-west-1.eks.amazonaws.com\n" +
		"Release:       shipped-f874cd2c\n" +
		"Channel:       Alpha\n" +
		"Tier:          2\n" +
		"Upgrades:      Automatic (follow the channel's minor)\n" +
		"Maintenance:   Sat,Sun 02:00 UTC for 4h\n" +
		"Message:       <none>\n" +
		"Age:           2d2h\n" +
		"\nResources:\n" +
		"  KIND        NAME                        EXTERNAL-NAME   STATE      SYNCED   READY   MESSAGE\n" +
		"  Cluster     prod-eks-f772324f           prod-eks        Ready      True     True    <none>\n" +
		"  NodeGroup   prod-eks-f772324f-default   <none>          Creating   True     False   creating node group\n" +
		"\nConditions:\n" +
		"  TYPE    STATUS   REASON              MESSAGE\n" +
		"  Ready   True     AllResourcesReady   <none>\n" +
		"\nAdd-ons:\n" +
		"  NAME                 VERSION               STATE    MESSAGE\n" +
		"  coredns              v1.14.7-eksbuild.35   Ready    <none>\n" +
		"  aws-ebs-csi-driver   v1.68.0-eksbuild.1    Failed   ConfigurationConflict\n" +
		"\nPlatform Components:\n" +
		"  NAME        VERSION   PHASE   AGE    MESSAGE\n" +
		"  karpenter   1.15.1    Ready   120m   <none>\n" +
		"\nEvents:\n" +
		"  LAST SEEN   TYPE     REASON    MESSAGE\n" +
		"  5m          Normal   Running   the cluster runs its latest generation\n"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

// A paused cluster says what is paused (everything, or its upgrades
// only), why, at what scope, who paused it, since and until when; an
// empty section is <none>, as kubectl describe prints it.
func TestPrintClusterDescribePausedAndEmpty(t *testing.T) {
	withClock(t)
	cl := testCluster()
	cl.Status.Pause = &cloudv1.Pause{By: "alice", Since: ago(time.Hour), Reason: "provider upgrade", Scope: "tenant"}
	cl.Status.UpgradePause = &cloudv1.Pause{By: "bob", Since: ago(2 * time.Hour), Reason: "quarter-end freeze",
		Scope: "channel beta tier 0", Until: timestamppb.New(time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC))}
	cl.Status.ManagedResources, cl.Status.RecentEvents = nil, nil

	got := printed(t, &cloudv1.GetClusterStatusResponse{Cluster: cl}, OutputTable)

	for _, want := range []string{
		"Paused:        everything: provider upgrade (tenant, by alice since 2026-11-14T11:00:00Z)\n",
		"Paused:        upgrades only: quarter-end freeze (channel beta tier 0, by bob since 2026-11-14T10:00:00Z, until 2026-12-31T00:00:00Z)\n",
		"\nResources:\n  <none>\n", "\nEvents:\n  <none>\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

func TestPrintMutations(t *testing.T) {
	withClock(t)
	cl := testCluster()
	tests := []struct {
		name string
		msg  proto.Message
		want string
	}{
		{"create", &cloudv1.CreateClusterResponse{Cluster: cl}, "cluster/prod-eks (01a1d2f6-12a9-7505-b7b2-8b8acfcab306) created\n"},
		{"update", &cloudv1.UpdateClusterResponse{Cluster: cl}, "cluster/prod-eks (01a1d2f6-12a9-7505-b7b2-8b8acfcab306) updated\n"},
		{"delete", &cloudv1.DeleteClusterResponse{Cluster: cl}, "cluster/prod-eks (01a1d2f6-12a9-7505-b7b2-8b8acfcab306) deletion requested\n"},
		{"import", &cloudv1.ImportClusterResponse{Cluster: cl}, "cluster/prod-eks (01a1d2f6-12a9-7505-b7b2-8b8acfcab306) imported\n"},
		{"takeover", &cloudv1.TakeOverClusterResponse{Cluster: cl}, "cluster/prod-eks (01a1d2f6-12a9-7505-b7b2-8b8acfcab306) taken over\n"},
		{"network create", &cloudv1.CreateNetworkResponse{Network: testNetwork()}, "network/prod (01a1d0a1-3333-7766-a4ff-84f3f49a774f) created\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := printed(t, tt.msg, OutputTable)
			if !strings.HasPrefix(got, tt.want) || !strings.Contains(got, "NAME ") {
				t.Fatalf("got\n%s\nwant it to start with %q and show the row", got, tt.want)
			}
		})
	}
}

func TestPrintPlan(t *testing.T) {
	withClock(t)
	res := &cloudv1.UpdateClusterResponse{Cluster: testCluster(), Plan: &cloudv1.Plan{PlanId: "p1", Applicable: true,
		Warnings: []string{"node group is replaced"},
		Changes: []*cloudv1.PlannedChange{{Kind: "NodeGroup", Name: "prod-eks-f772324f-default", ExternalName: "default",
			Action: cloudv1.PlannedAction_PLANNED_ACTION_DISRUPTIVE, Detail: "instance types"}}}}

	got := printed(t, res, OutputTable)

	want := "Plan p1 (applicable)\n" +
		"ACTION       KIND        NAME                        EXTERNAL-NAME   DETAIL\n" +
		"Disruptive   NodeGroup   prod-eks-f772324f-default   default         instance types\n" +
		"\nWarnings:\n  node group is replaced\n"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func testNetwork() *cloudv1.Network {
	return &cloudv1.Network{TenantId: "acme", Id: "01a1d0a1-3333-7766-a4ff-84f3f49a774f", Name: "prod", CreateTime: ago(72 * time.Hour),
		Spec: &cloudv1.NetworkSpec{Region: "us-west-1", Layout: &cloudv1.NetworkLayout{CidrBlock: "10.1.0.0/16",
			NatGatewayMode: cloudv1.NatGatewayMode_NAT_GATEWAY_MODE_PER_ZONE,
			Subnets:        []*cloudv1.Subnet{{ZoneId: "usw1-az1"}, {ZoneId: "usw1-az3"}, {ZoneId: "usw1-az1"}}}},
		Status: &cloudv1.NetworkStatus{State: cloudv1.NetworkState_NETWORK_STATE_READY, ClusterIds: []string{"c1"},
			Resources: &cloudv1.NetworkResources{CloudNetworkId: "vpc-0abc"},
			ManagedResources: []*cloudv1.ManagedResource{{Kind: "VPC", Name: "prod-ceaf6f38", ExternalName: "vpc-0abc",
				State: cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY, Synced: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE,
				Ready: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE}}}}
}

func TestPrintNetworks(t *testing.T) {
	withClock(t)
	list := &cloudv1.ListNetworksResponse{Networks: []*cloudv1.Network{testNetwork()}}

	if got, want := printed(t, list, OutputTable),
		"NAME   ID                                     PHASE   REGION      CLUSTERS   AGE\n"+
			"prod   01a1d0a1-3333-7766-a4ff-84f3f49a774f   Ready   us-west-1   1          3d\n"; got != want {
		t.Errorf("table: got\n%s\nwant\n%s", got, want)
	}
	if got, want := printed(t, list, OutputWide),
		"NAME   ID                                     PHASE   REGION      CLUSTERS   AGE   VPC        CIDR          ZONES               NAT\n"+
			"prod   01a1d0a1-3333-7766-a4ff-84f3f49a774f   Ready   us-west-1   1          3d    vpc-0abc   10.1.0.0/16   usw1-az1,usw1-az3   PerZone\n"; got != want {
		t.Errorf("wide: got\n%s\nwant\n%s", got, want)
	}
	got := printed(t, &cloudv1.GetNetworkResponse{Network: testNetwork()}, OutputTable)
	for _, want := range []string{"Name:       prod\n", "VPC:        vpc-0abc\n", "CIDR:       10.1.0.0/16\n",
		"  KIND   NAME            EXTERNAL-NAME   STATE   SYNCED   READY   MESSAGE\n  VPC    prod-ceaf6f38   vpc-0abc        Ready   True     True    <none>\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("describe lacks %q:\n%s", want, got)
		}
	}
}

func TestPrintAccountsAndWhoAmI(t *testing.T) {
	withClock(t)
	account := &cloudv1.CloudAccount{Id: "01a1d0a0-4a34-735f-be7d-19d1662f1138", TenantId: "acme", AccountName: "aws-111122223333",
		CreateTime: ago(24 * time.Hour), Provider: &cloudv1.CloudAccount_Aws{Aws: &cloudv1.AwsAccount{AccountId: "111122223333", Role: "cluster-vending-provisioner"}},
		Status: &cloudv1.CloudAccountStatus{State: cloudv1.CloudAccountState_CLOUD_ACCOUNT_STATE_READY, Namespace: "tenant-acme"}}

	if got, want := printed(t, &cloudv1.ListCloudAccountsResponse{Accounts: []*cloudv1.CloudAccount{account}}, OutputTable),
		"NAME               ID                                     ACCOUNT        STATE   ACCESS      AGE\n"+
			"aws-111122223333   01a1d0a0-4a34-735f-be7d-19d1662f1138   111122223333   Ready   Unchecked   24h\n"; got != want {
		t.Errorf("accounts: got\n%s\nwant\n%s", got, want)
	}
	if got := printed(t, &cloudv1.BootstrapTenantResponse{Account: account}, OutputTable); !strings.HasPrefix(got,
		"cloudaccount/aws-111122223333 (01a1d0a0-4a34-735f-be7d-19d1662f1138) bootstrapped\n") {
		t.Errorf("bootstrap: got\n%s", got)
	}
	who := &cloudv1.WhoAmIResponse{Username: "alice@example.com", Subject: "s1", Issuer: "entra",
		Roles: []string{"Acme.Admin"}, Grants: []*cloudv1.AccessGrant{{Role: cloudv1.AccessRole_ACCESS_ROLE_TENANT_ADMIN, Tenants: []string{"acme"}}}}
	got := printed(t, who, OutputTable)
	for _, want := range []string{"User:      alice@example.com\n", "  ROLE          TENANTS\n  TenantAdmin   acme\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("whoami lacks %q:\n%s", want, got)
		}
	}
}

// yaml and json print the whole object, as before; an unknown format is
// refused with the formats there are.
func TestPrintFormats(t *testing.T) {
	cl := &cloudv1.GetClusterResponse{Cluster: &cloudv1.Cluster{Name: "prod-eks"}}
	if got := printed(t, cl, OutputYAML); !strings.Contains(got, "name: prod-eks") {
		t.Errorf("yaml: %s", got)
	}
	if got := printed(t, cl, OutputJSON); !strings.Contains(got, `"name":  "prod-eks"`) && !strings.Contains(got, `"name": "prod-eks"`) {
		t.Errorf("json: %s", got)
	}
	err := Print(&bytes.Buffer{}, cl, "xml")
	if err == nil || !strings.Contains(err.Error(), "table, wide, yaml or json") {
		t.Errorf("xml: %v", err)
	}
}

// While its owner is deleted, a recorded object that is gone reads Deleted.
func TestResourceRowsShowDeleted(t *testing.T) {
	rows := resourceRows([]*cloudv1.ManagedResource{
		{Kind: "Subnet", Name: "s", ExternalName: "subnet-1", State: cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING, Message: "deleted"},
		{Kind: "Subnet", Name: "t", State: cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING, Message: "deleted outside the platform"},
	})

	if rows[0][3] != "Deleted" || rows[1][3] != "Missing" {
		t.Fatalf("states = %q, %q", rows[0][3], rows[1][3])
	}
}

// cluster get says what the cluster's channel offers: the channel's minor
// next to the version, an upgrade available, and, prominently, a minor
// that is ending.
func TestPrintClusterKubernetes(t *testing.T) {
	withClock(t)
	for _, tc := range []struct {
		name  string
		k     *cloudv1.ClusterKubernetes
		lines []string
	}{
		{"on its channel's minor", &cloudv1.ClusterKubernetes{Channel: "alpha", ChannelMinor: "1.37"},
			[]string{"Version:       1.37 (channel alpha offers 1.37)\n"}},
		{"behind", &cloudv1.ClusterKubernetes{Channel: "alpha", ChannelMinor: "1.38", Available: "1.38",
			Message: "upgrade available: 1.38, the minor channel alpha offers"},
			[]string{"Version:       1.37 (channel alpha offers 1.38)\n", "Available:     1.38\n"}},
		{"ahead", &cloudv1.ClusterKubernetes{Channel: "stable", ChannelMinor: "1.35",
			Message: "runs 1.37, ahead of channel stable's 1.35: it is never downgraded and waits for the channel"},
			[]string{"Note:          runs 1.37, ahead of channel stable's 1.35: it is never downgraded and waits for the channel\n"}},
		{"ending", &cloudv1.ClusterKubernetes{Ending: true, Message: "Kubernetes 1.37 is on no release channel: it is ending; move to 1.38"},
			[]string{"Version:       1.37\n", "WARNING:       Kubernetes 1.37 is on no release channel: it is ending; move to 1.38\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testCluster()
			c.Status.Kubernetes = tc.k
			got := printed(t, &cloudv1.GetClusterResponse{Cluster: c}, OutputTable)
			for _, line := range tc.lines {
				if !strings.Contains(got, line) {
					t.Errorf("missing %q in\n%s", line, got)
				}
			}
		})
	}
}
