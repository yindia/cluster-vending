package cluster

import (
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func TestWithControlPlaneLogs(t *testing.T) {
	logTypes := []cloudv1.AwsClusterLogType{cloudv1.AwsClusterLogType_AWS_CLUSTER_LOG_TYPE_API}
	enabled := true
	for _, tt := range []struct {
		name      string
		spec      *cloudv1.ClusterSpec
		wantGroup string
		wantURL   string
	}{
		{"logging off", &cloudv1.ClusterSpec{Region: "eu-west-1", Provider: &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{}}}, "", ""},
		{"log types on", &cloudv1.ClusterSpec{Region: "eu-west-1", Provider: &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{EnabledClusterLogTypes: logTypes}}},
			"/aws/eks/prod/cluster",
			"https://eu-west-1.console.aws.amazon.com/cloudwatch/home?region=eu-west-1#logsV2:log-groups/log-group/$252Faws$252Feks$252Fprod$252Fcluster"},
		{"platform-owned group in GovCloud", &cloudv1.ClusterSpec{Region: "us-gov-west-1", Provider: &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{
			ControlPlaneLogGroup: &cloudv1.AwsControlPlaneLogGroup{Enabled: &enabled}}}},
			"/aws/eks/prod/cluster",
			"https://console.amazonaws-us-gov.com/cloudwatch/home?region=us-gov-west-1#logsV2:log-groups/log-group/$252Faws$252Feks$252Fprod$252Fcluster"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := &cloudv1.Cluster{Name: "prod", Spec: tt.spec}
			withControlPlaneLogs(c)
			if got := c.GetStatus().GetAws().GetControlPlaneLogGroup(); got != tt.wantGroup {
				t.Errorf("group = %q, want %q", got, tt.wantGroup)
			}
			if got := c.GetStatus().GetAws().GetControlPlaneLogsUrl(); got != tt.wantURL {
				t.Errorf("url = %q, want %q", got, tt.wantURL)
			}
		})
	}
}

func TestLogGroupConsoleURLChina(t *testing.T) {
	if got := logGroupConsoleURL("cn-north-1", "/aws/eks/a/cluster"); got != "https://console.amazonaws.cn/cloudwatch/home?region=cn-north-1#logsV2:log-groups/log-group/$252Faws$252Feks$252Fa$252Fcluster" {
		t.Fatal(got)
	}
	if logGroupConsoleURL("", "/x") != "" {
		t.Fatal("no region must give no URL")
	}
}
