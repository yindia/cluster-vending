package aws

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
)

// LogsAPI is the part of the CloudWatch Logs client discovery uses:
// read-only calls only.
type LogsAPI interface {
	DescribeLogGroups(context.Context, *cloudwatchlogs.DescribeLogGroupsInput, ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.DescribeLogGroupsOutput, error)
	ListTagsForResource(context.Context, *cloudwatchlogs.ListTagsForResourceInput, ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.ListTagsForResourceOutput, error)
}

// IAMAPI is the part of the IAM client discovery uses: read-only calls
// only.
type IAMAPI interface {
	GetRole(context.Context, *iam.GetRoleInput, ...func(*iam.Options)) (*iam.GetRoleOutput, error)
}

// Flow log destination types (ec2 LogDestinationType).
const (
	FlowLogToCloudWatch = "cloud-watch-logs"
	FlowLogToS3         = "s3"
)

// FlowLog is an ec2 FlowLog on the VPC.
type FlowLog struct {
	ID string
	// DestinationType is FlowLogToCloudWatch, FlowLogToS3 or
	// kinesis-data-firehose; Destination the ARN it delivers to.
	DestinationType, Destination string
	TrafficType, LogFormat       string
	MaxAggregationInterval       int
	Tags                         map[string]string
	// LogGroup and Role are set for a CloudWatch Logs flow log whose log
	// group and delivery role could be read; else Unreadable says why not.
	LogGroup   *LogGroup
	Role       *Role
	Unreadable string
}

// LogGroup is a CloudWatch Logs log group.
type LogGroup struct {
	Name, ARN, KMSKeyID, Class string
	// RetentionDays is 0 when records never expire.
	RetentionDays int
	Tags          map[string]string
	// Shared: flow logs of other resources in the region write to it too.
	Shared bool
}

// Role is an IAM role.
type Role struct {
	Name, ARN, Path, Description, PermissionsBoundary string
	// AssumeRolePolicy is the trust policy document (JSON).
	AssumeRolePolicy   string
	MaxSessionDuration int
	Tags               map[string]string
	// Shared: flow logs of other resources in the region deliver with it
	// too. Other regions are not looked at.
	Shared bool
}

// flowLogs reads the VPC's flow logs and, for those delivering to
// CloudWatch Logs, their log group and delivery role. A log group or role
// that cannot be read does not fail discovery: the flow log says why
// (Unreadable). Every flow log of the region is listed once, to tell
// which log groups and roles others share.
//
// Calls: ec2:DescribeFlowLogs, logs:DescribeLogGroups,
// logs:ListTagsForResource, iam:GetRole.
func (t *Tenant) flowLogs(ctx context.Context, vpcID string) ([]FlowLog, error) {
	var all []types.FlowLog
	p := ec2.NewDescribeFlowLogsPaginator(t.ec2, &ec2.DescribeFlowLogsInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, translate(err, "describe flow logs")
		}
		all = append(all, page.FlowLogs...)
	}
	var out []FlowLog
	for _, f := range all {
		if awssdk.ToString(f.ResourceId) != vpcID {
			continue
		}
		fl := FlowLog{
			ID: awssdk.ToString(f.FlowLogId), DestinationType: string(f.LogDestinationType),
			Destination: awssdk.ToString(f.LogDestination), TrafficType: string(f.TrafficType),
			LogFormat: awssdk.ToString(f.LogFormat), MaxAggregationInterval: int(awssdk.ToInt32(f.MaxAggregationInterval)),
			Tags: tags(f.Tags),
		}
		if fl.DestinationType == FlowLogToCloudWatch {
			t.flowLogTargets(ctx, &fl, f, all)
		}
		out = append(out, fl)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// flowLogTargets reads the log group and delivery role of a CloudWatch
// Logs flow log.
func (t *Tenant) flowLogTargets(ctx context.Context, fl *FlowLog, f types.FlowLog, all []types.FlowLog) {
	group, err := t.logGroup(ctx, awssdk.ToString(f.LogGroupName))
	if err != nil {
		fl.Unreadable = "its log group cannot be read: " + err.Error()
		return
	}
	role, err := t.role(ctx, awssdk.ToString(f.DeliverLogsPermissionArn))
	if err != nil {
		fl.Unreadable = "its delivery role cannot be read: " + err.Error()
		return
	}
	for _, o := range all {
		if awssdk.ToString(o.ResourceId) == awssdk.ToString(f.ResourceId) {
			continue
		}
		group.Shared = group.Shared || awssdk.ToString(o.LogGroupName) == group.Name
		role.Shared = role.Shared || awssdk.ToString(o.DeliverLogsPermissionArn) == role.ARN
	}
	fl.LogGroup, fl.Role = &group, &role
}

// logGroup reads one log group of the account and region by name.
func (t *Tenant) logGroup(ctx context.Context, name string) (LogGroup, error) {
	if name == "" {
		return LogGroup{}, fmt.Errorf("%w: the flow log names no log group", ErrNotFound)
	}
	out, err := t.logs.DescribeLogGroups(ctx, &cloudwatchlogs.DescribeLogGroupsInput{LogGroupNamePrefix: awssdk.String(name)})
	if err != nil {
		return LogGroup{}, translate(err, "describe log group "+name)
	}
	for _, g := range out.LogGroups {
		if awssdk.ToString(g.LogGroupName) != name {
			continue
		}
		// ListTagsForResource and the FlowLog's logDestination take the ARN
		// without the ":*" DescribeLogGroups appends.
		arn := strings.TrimSuffix(awssdk.ToString(g.Arn), ":*")
		tagged, err := t.logs.ListTagsForResource(ctx, &cloudwatchlogs.ListTagsForResourceInput{ResourceArn: awssdk.String(arn)})
		if err != nil {
			return LogGroup{}, translate(err, "list tags of log group "+name)
		}
		return LogGroup{Name: name, ARN: arn, KMSKeyID: awssdk.ToString(g.KmsKeyId), Class: string(g.LogGroupClass),
			RetentionDays: int(awssdk.ToInt32(g.RetentionInDays)), Tags: nonEmpty(tagged.Tags)}, nil
	}
	return LogGroup{}, fmt.Errorf("%w: log group %s in %s/%s", ErrNotFound, name, t.AccountID, t.Region)
}

// role reads an IAM role of the account by ARN.
func (t *Tenant) role(ctx context.Context, arn string) (Role, error) {
	_, resource, ok := strings.Cut(arn, ":role/")
	if !ok || !strings.Contains(arn, ":"+t.AccountID+":") {
		return Role{}, fmt.Errorf("%w: %q is not a role of account %s", ErrNotFound, arn, t.AccountID)
	}
	name := resource[strings.LastIndex(resource, "/")+1:] // after the path
	out, err := t.iam.GetRole(ctx, &iam.GetRoleInput{RoleName: awssdk.String(name)})
	if err != nil {
		return Role{}, translate(err, "get role "+name)
	}
	r := out.Role
	// IAM returns the trust policy URL-encoded.
	trust, err := url.QueryUnescape(awssdk.ToString(r.AssumeRolePolicyDocument))
	if err != nil {
		return Role{}, fmt.Errorf("aws: trust policy of role %s: %w", name, err)
	}
	role := Role{Name: name, ARN: awssdk.ToString(r.Arn), Path: awssdk.ToString(r.Path), Description: awssdk.ToString(r.Description),
		AssumeRolePolicy: trust, MaxSessionDuration: int(awssdk.ToInt32(r.MaxSessionDuration))}
	if r.PermissionsBoundary != nil {
		role.PermissionsBoundary = awssdk.ToString(r.PermissionsBoundary.PermissionsBoundaryArn)
	}
	if len(r.Tags) > 0 {
		role.Tags = make(map[string]string, len(r.Tags))
		for _, tag := range r.Tags {
			role.Tags[awssdk.ToString(tag.Key)] = awssdk.ToString(tag.Value)
		}
	}
	return role, nil
}

func nonEmpty(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
}
