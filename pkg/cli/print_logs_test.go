package cli

import (
	"strings"
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func TestClusterGetShowsControlPlaneLogs(t *testing.T) {
	const url = "https://eu-west-1.console.aws.amazon.com/cloudwatch/home?region=eu-west-1#logsV2:log-groups/log-group/$252Faws$252Feks$252Fprod$252Fcluster"
	on := &cloudv1.Cluster{Name: "prod", Spec: &cloudv1.ClusterSpec{Provider: &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{}}},
		Status: &cloudv1.ClusterStatus{Provider: &cloudv1.ClusterStatus_Aws{Aws: &cloudv1.AwsClusterStatus{
			ControlPlaneLogGroup: "/aws/eks/prod/cluster", ControlPlaneLogsUrl: url}}}}
	got := printed(t, &cloudv1.GetClusterResponse{Cluster: on}, OutputTable)
	if !strings.Contains(got, "/aws/eks/prod/cluster") || !strings.Contains(got, url) {
		t.Fatalf("log group or console URL missing:\n%s", got)
	}
	off := &cloudv1.Cluster{Name: "prod", Spec: &cloudv1.ClusterSpec{Provider: &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{}}}}
	if got := printed(t, &cloudv1.GetClusterResponse{Cluster: off}, OutputTable); !strings.Contains(got, "off (spec.aws.enabledClusterLogTypes is empty)") {
		t.Fatalf("logging off not said:\n%s", got)
	}
}
