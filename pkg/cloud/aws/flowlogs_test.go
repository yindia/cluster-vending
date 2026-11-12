package aws

import (
	"context"
	"strings"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	logstypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/smithy-go"
)

func (f *fakeEC2) DescribeFlowLogs(context.Context, *ec2.DescribeFlowLogsInput, ...func(*ec2.Options)) (*ec2.DescribeFlowLogsOutput, error) {
	return &ec2.DescribeFlowLogsOutput{FlowLogs: f.flowLogs}, nil
}

// fakeLogs has the log groups "/vpc/prod" and "/vpc/prod-extra" (a name
// with the same prefix).
type fakeLogs struct{}

func (fakeLogs) DescribeLogGroups(_ context.Context, in *cloudwatchlogs.DescribeLogGroupsInput, _ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.DescribeLogGroupsOutput, error) {
	out := &cloudwatchlogs.DescribeLogGroupsOutput{}
	for _, name := range []string{"/vpc/prod-extra", "/vpc/prod"} {
		if strings.HasPrefix(name, awssdk.ToString(in.LogGroupNamePrefix)) {
			out.LogGroups = append(out.LogGroups, logstypes.LogGroup{LogGroupName: awssdk.String(name),
				Arn:             awssdk.String("arn:aws:logs:us-east-1:" + tenantAccount + ":log-group:" + name + ":*"),
				RetentionInDays: awssdk.Int32(14)})
		}
	}
	return out, nil
}

func (fakeLogs) ListTagsForResource(_ context.Context, in *cloudwatchlogs.ListTagsForResourceInput, _ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.ListTagsForResourceOutput, error) {
	if strings.HasSuffix(awssdk.ToString(in.ResourceArn), ":*") {
		return nil, &smithy.GenericAPIError{Code: "ValidationException", Message: "ARN with :*"}
	}
	return &cloudwatchlogs.ListTagsForResourceOutput{Tags: map[string]string{"terraform": "true"}}, nil
}

// fakeIAM has the role "flow-logs" under the path /svc/.
type fakeIAM struct{}

func (fakeIAM) GetRole(_ context.Context, in *iam.GetRoleInput, _ ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	if awssdk.ToString(in.RoleName) != "flow-logs" {
		return nil, &smithy.GenericAPIError{Code: "NoSuchEntity", Message: "no such role"}
	}
	return &iam.GetRoleOutput{Role: &iamtypes.Role{RoleName: in.RoleName, Path: awssdk.String("/svc/"),
		Arn:                      awssdk.String("arn:aws:iam::" + tenantAccount + ":role/svc/flow-logs"),
		AssumeRolePolicyDocument: awssdk.String("%7B%22Version%22%3A%222012-10-17%22%7D"),
		MaxSessionDuration:       awssdk.Int32(3600),
		Tags:                     []iamtypes.Tag{{Key: awssdk.String("team"), Value: awssdk.String("net")}}}}, nil
}

func cloudWatchFlowLog(id, resource, group, role string) types.FlowLog {
	return types.FlowLog{FlowLogId: awssdk.String(id), ResourceId: awssdk.String(resource),
		LogDestinationType: types.LogDestinationTypeCloudWatchLogs, LogGroupName: awssdk.String(group),
		DeliverLogsPermissionArn: awssdk.String(role), TrafficType: types.TrafficTypeAll, MaxAggregationInterval: awssdk.Int32(600)}
}

func TestDiscoverFlowLogs(t *testing.T) {
	role := "arn:aws:iam::" + tenantAccount + ":role/svc/flow-logs"
	api := &fakeEC2{flowLogs: []types.FlowLog{
		cloudWatchFlowLog("fl-2", vpcID, "/vpc/prod", role),
		{FlowLogId: awssdk.String("fl-1"), ResourceId: awssdk.String(vpcID), LogDestinationType: types.LogDestinationTypeS3,
			LogDestination: awssdk.String("arn:aws:s3:::flow-logs")},
		cloudWatchFlowLog("fl-3", vpcID, "/vpc/gone", role),
		cloudWatchFlowLog("fl-4", vpcID, "/vpc/prod", "arn:aws:iam::"+tenantAccount+":role/deleted"),
		// Another VPC writes to the same log group, with another role.
		cloudWatchFlowLog("fl-9", "vpc-00000000000000bbb", "/vpc/prod", "arn:aws:iam::"+tenantAccount+":role/other"),
	}}
	n, err := fakeTenant(api).DiscoverNetwork(t.Context(), vpcID, platformSubnets)
	if err != nil {
		t.Fatal(err)
	}
	if len(n.FlowLogs) != 4 {
		t.Fatalf("flow logs = %+v, want the VPC's 4, ordered by ID", n.FlowLogs)
	}
	s3, cw, noGroup, noRole := n.FlowLogs[0], n.FlowLogs[1], n.FlowLogs[2], n.FlowLogs[3]
	if s3.DestinationType != FlowLogToS3 || s3.LogGroup != nil || s3.Unreadable != "" {
		t.Errorf("S3 flow log = %+v", s3)
	}
	if cw.Unreadable != "" || cw.LogGroup == nil || cw.Role == nil {
		t.Fatalf("CloudWatch flow log = %+v", cw)
	}
	if g := cw.LogGroup; g.Name != "/vpc/prod" || strings.HasSuffix(g.ARN, ":*") || g.RetentionDays != 14 ||
		g.Tags["terraform"] != "true" || !g.Shared {
		t.Errorf("log group = %+v, want the exact name, its ARN without :*, shared with the other VPC", g)
	}
	if r := cw.Role; r.Name != "flow-logs" || r.Path != "/svc/" || r.AssumeRolePolicy != `{"Version":"2012-10-17"}` ||
		r.Tags["team"] != "net" || r.Shared {
		t.Errorf("role = %+v, want its name without path, the decoded trust policy, not shared", r)
	}
	if !strings.Contains(noGroup.Unreadable, "log group") || !strings.Contains(noRole.Unreadable, "delivery role") {
		t.Errorf("unreadable: %q / %q", noGroup.Unreadable, noRole.Unreadable)
	}
}
