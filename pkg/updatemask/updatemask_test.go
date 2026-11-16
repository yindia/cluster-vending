package updatemask

import (
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// current is a cluster as GetCluster returns it: the stored spec, with
// the defaults the server filled in.
func current() *cloudv1.Cluster {
	return &cloudv1.Cluster{Id: "c1", Name: "prod-eks", Etag: "e1", DisplayName: "Prod", Labels: map[string]string{"env": "prod"},
		Spec: &cloudv1.ClusterSpec{Region: "us-west-1",
			ControlPlane:   &cloudv1.ControlPlaneSpec{KubernetesVersion: "1.36"},
			ReleaseChannel: cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA,
			UpgradeMode:    cloudv1.UpgradeMode_UPGRADE_MODE_AUTOMATIC,
			Provider:       &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{Irsa: proto.Bool(true)}}},
		Status: &cloudv1.ClusterStatus{State: cloudv1.ClusterState_CLUSTER_STATE_RUNNING}}
}

// desired is the manifest: what it states, nothing else.
func desired(edit func(c *cloudv1.Cluster)) *cloudv1.Cluster {
	c := &cloudv1.Cluster{Id: "c1", Name: "prod-eks", Spec: &cloudv1.ClusterSpec{Region: "us-west-1",
		ControlPlane: &cloudv1.ControlPlaneSpec{KubernetesVersion: "1.36"}}}
	if edit != nil {
		edit(c)
	}
	return c
}

func TestDiff(t *testing.T) {
	tests := []struct {
		name      string
		desired   *cloudv1.Cluster
		wantPaths []string
		wantErr   string
	}{
		{name: "nothing changed: the manifest restates what is there", desired: desired(nil)},
		{name: "a channel change", wantPaths: []string{"spec.release_channel"},
			desired: desired(func(c *cloudv1.Cluster) { c.Spec.ReleaseChannel = cloudv1.ReleaseChannel_RELEASE_CHANNEL_ALPHA })},
		{name: "a maintenance window added", wantPaths: []string{"spec.maintenance_window"},
			desired: desired(func(c *cloudv1.Cluster) {
				c.Spec.MaintenanceWindow = &cloudv1.MaintenanceWindow{Start: "02:00", Duration: "4h"}
			})},
		{name: "labels changed: compared whole", wantPaths: []string{"labels"},
			desired: desired(func(c *cloudv1.Cluster) { c.Labels = map[string]string{"env": "prod", "team": "a"} })},
		{name: "the same labels", desired: desired(func(c *cloudv1.Cluster) { c.Labels = map[string]string{"env": "prod"} })},
		{name: "a version and the display name", wantPaths: []string{"display_name", "spec.control_plane.kubernetes_version"},
			desired: desired(func(c *cloudv1.Cluster) { c.DisplayName = "Production"; c.Spec.ControlPlane.KubernetesVersion = "1.37" })},
		{name: "a field nested in the cloud's settings", wantPaths: []string{"spec.aws.irsa"},
			desired: desired(func(c *cloudv1.Cluster) {
				c.Spec.Provider = &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{Irsa: proto.Bool(false)}}
			})},
		{name: "the region cannot change", wantErr: "spec.region cannot be changed",
			desired: desired(func(c *cloudv1.Cluster) { c.Spec.Region = "us-east-1" })},
		{name: "the name cannot change", wantErr: "name cannot be changed",
			desired: desired(func(c *cloudv1.Cluster) { c.Name = "other" })},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changes, err := Diff(current(), tt.desired, Cluster)

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := Paths(changes); !slices.Equal(got, tt.wantPaths) {
				t.Errorf("paths = %v, want %v", got, tt.wantPaths)
			}
		})
	}
}

// A changed field records what it was and what it becomes.
func TestDiffRecordsBeforeAndAfter(t *testing.T) {
	changes, err := Diff(current(), desired(func(c *cloudv1.Cluster) {
		c.Spec.ReleaseChannel = cloudv1.ReleaseChannel_RELEASE_CHANNEL_ALPHA
		c.Spec.MaintenanceWindow = &cloudv1.MaintenanceWindow{Start: "02:00", Duration: "4h"}
	}), Cluster)
	if err != nil || len(changes) != 2 {
		t.Fatalf("changes = %v, %v", changes, err)
	}
	window, channel := changes[0], changes[1]
	if channel.Path != "spec.release_channel" || !channel.WasSet ||
		channel.From.Enum() != protoreflect.EnumNumber(cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA) ||
		channel.To.Enum() != protoreflect.EnumNumber(cloudv1.ReleaseChannel_RELEASE_CHANNEL_ALPHA) {
		t.Errorf("channel = %+v", channel)
	}
	if window.WasSet {
		t.Errorf("window was not set before: %+v", window)
	}
}

// Every network path the server accepts.
func TestNetworkPaths(t *testing.T) {
	cur := &cloudv1.Network{Name: "prod", Spec: &cloudv1.NetworkSpec{Region: "us-west-1"}}
	want := &cloudv1.Network{Name: "prod", Spec: &cloudv1.NetworkSpec{Region: "us-west-1",
		FlowLogs: &cloudv1.NetworkFlowLogs{Enabled: proto.Bool(true)}}}

	changes, err := Diff(cur, want, Network)

	if err != nil || !slices.Equal(Paths(changes), []string{"spec.flow_logs"}) {
		t.Fatalf("changes = %v, %v", Paths(changes), err)
	}
}
