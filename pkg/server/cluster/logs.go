package cluster

import (
	"net/url"
	"strings"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// withControlPlaneLogs fills status.aws.control_plane_log_group and
// control_plane_logs_url from the spec when control plane logging is on:
// EKS writes to "/aws/eks/<name>/cluster" whether it or the platform made
// the group. Derived on every read; nothing is stored.
func withControlPlaneLogs(c *cloudv1.Cluster) {
	aws := c.GetSpec().GetAws()
	if aws == nil || (len(aws.GetEnabledClusterLogTypes()) == 0 && !aws.GetControlPlaneLogGroup().GetEnabled()) || c.GetName() == "" {
		return
	}
	if c.Status == nil {
		c.Status = &cloudv1.ClusterStatus{}
	}
	st := c.Status.GetAws()
	if st == nil {
		if c.Status.GetProvider() != nil {
			return // another cloud's status
		}
		st = &cloudv1.AwsClusterStatus{}
		c.Status.Provider = &cloudv1.ClusterStatus_Aws{Aws: st}
	}
	group := crossplaneaws.ControlPlaneLogGroupName(c.GetName())
	st.ControlPlaneLogGroup = group
	st.ControlPlaneLogsUrl = logGroupConsoleURL(c.GetSpec().GetRegion(), group)
}

// logGroupConsoleURL is the CloudWatch console page of a log group in
// region, in the region's partition; empty without a region. The console
// escapes the group in its fragment with "$25" for "%".
func logGroupConsoleURL(region, group string) string {
	if region == "" {
		return ""
	}
	host := region + ".console.aws.amazon.com"
	switch crossplaneaws.Partition(region) {
	case "aws-us-gov":
		host = "console.amazonaws-us-gov.com"
	case "aws-cn":
		host = "console.amazonaws.cn"
	}
	escaped := strings.ReplaceAll(url.PathEscape(group), "/", "%2F")
	escaped = strings.ReplaceAll(escaped, "%", "$25")
	return "https://" + host + "/cloudwatch/home?region=" + region + "#logsV2:log-groups/log-group/" + escaped
}
