package aws

import (
	"encoding/json"
	"errors"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	awscwl "github.com/upbound/provider-aws/v2/apis/namespaced/cloudwatchlogs/v1beta1"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
)

// Flow log defaults (cloud.v1 NetworkFlowLogs).
const (
	DefaultFlowLogTrafficType        = "ALL"
	DefaultFlowLogRetentionDays      = 30
	DefaultFlowLogAggregationSeconds = 600

	flowLogDestinationCloudWatch = "cloud-watch-logs"
)

// FlowLogPlan is a network's flow logs resolved to concrete values.
type FlowLogPlan struct {
	// TrafficType is ALL, ACCEPT or REJECT.
	TrafficType string
	// RetentionDays is how long the log group keeps the records.
	RetentionDays int
	// AggregationSeconds is 60 or 600.
	AggregationSeconds int
}

// FlowLogGroupName is the CloudWatch Logs log group of a network's flow
// logs; it is the Group's external name (the provider identifies a log
// group by its name).
func FlowLogGroupName(prefix string) string {
	return "/cluster-vending/network/" + prefix + "/flow-logs"
}

// flowLogs renders the network's flow logs, in creation order:
//
//	Group "<prefix>-flow-logs"           the log group
//	Role "<prefix>-flow-logs-role"       assumed by the flow logs service
//	Policy + RolePolicyAttachment        lets the role write to the group
//	FlowLog "<prefix>-flow-log"          on the VPC
//	Usage x2                             the Group and the Role cannot be
//	                                     deleted while the FlowLog exists
func (b networkBuilder) flowLogs() ([]client.Object, error) {
	f := b.p.FlowLogs
	if b.p.AccountID == "" {
		return nil, errors.New("flow logs need the network's account ID (the log group ARN in the delivery policy)")
	}
	group, role, policy, flowLog := b.name("flow-logs"), b.name("flow-logs-role"), b.name("flow-logs-policy"), b.name("flow-log")
	groupMeta := b.meta("flow-logs", nil)
	groupMeta.Annotations = map[string]string{ExternalNameAnnotation: FlowLogGroupName(b.p.Prefix)}
	out := []client.Object{
		&awscwl.Group{
			ObjectMeta: groupMeta,
			Spec: awscwl.GroupSpec{ManagedResourceSpec: b.spec(), ForProvider: awscwl.GroupParameters{
				Region: ptr.To(b.p.Region), RetentionInDays: ptr.To(float64(f.RetentionDays)), Tags: b.tags("flow-logs", nil),
			}},
		},
		&awsiam.Role{
			ObjectMeta: b.meta("flow-logs-role", nil),
			Spec: awsiam.RoleSpec{ManagedResourceSpec: b.spec(), ForProvider: awsiam.RoleParameters{
				AssumeRolePolicy: ptr.To(b.flowLogTrust()), Tags: b.tags("flow-logs-role", nil),
			}},
		},
		&awsiam.Policy{
			ObjectMeta: b.meta("flow-logs-policy", nil),
			Spec: awsiam.PolicySpec{ManagedResourceSpec: b.spec(), ForProvider: awsiam.PolicyParameters{
				Policy: ptr.To(b.flowLogPolicy()), Description: ptr.To("Flow log delivery of network " + b.p.NetworkName),
				Tags: b.tags("flow-logs-policy", nil),
			}},
		},
		&awsiam.RolePolicyAttachment{
			ObjectMeta: b.meta("flow-logs-attachment", nil),
			Spec: awsiam.RolePolicyAttachmentSpec{ManagedResourceSpec: b.spec(), ForProvider: awsiam.RolePolicyAttachmentParameters{
				PolicyArnRef: b.ref(policy), RoleRef: b.ref(role),
			}},
		},
		&awsec2.FlowLog{
			ObjectMeta: b.meta("flow-log", nil),
			Spec: awsec2.FlowLogSpec{ManagedResourceSpec: b.spec(), ForProvider: awsec2.FlowLogParameters{
				Region: ptr.To(b.p.Region), VPCIDRef: b.ref(b.name("vpc")),
				LogDestinationType: ptr.To(flowLogDestinationCloudWatch), LogDestinationRef: b.ref(group),
				IAMRoleArnRef: b.ref(role), TrafficType: ptr.To(f.TrafficType),
				MaxAggregationInterval: ptr.To(float64(f.AggregationSeconds)), Tags: b.tags("flow-log", nil),
			}},
		},
	}
	by := flowLogKind.named(flowLog)
	for _, u := range Usages(b.p.Namespace, b.p.Prefix, crossplane.NetworkLabels(b.p.TenantID, b.p.NetworkID), []UsagePair{
		{Of: logGroupKind.named(group), By: by},
		{Of: roleKind.named(role), By: by},
	}) {
		out = append(out, u)
	}
	return out, nil
}

// flowLogTrust lets the flow logs service assume the delivery role, for
// flow logs of this account and region only.
func (b networkBuilder) flowLogTrust() string {
	partition := Partition(b.p.Region)
	doc, _ := json.Marshal(map[string]any{ // maps and strings: cannot fail
		"Version": "2012-10-17",
		"Statement": []map[string]any{{
			"Effect": "Allow", "Principal": map[string]string{"Service": "vpc-flow-logs.amazonaws.com"}, "Action": "sts:AssumeRole",
			"Condition": map[string]any{
				"StringEquals": map[string]string{"aws:SourceAccount": b.p.AccountID},
				"ArnLike": map[string]string{
					"aws:SourceArn": "arn:" + partition + ":ec2:" + b.p.Region + ":" + b.p.AccountID + ":vpc-flow-log/*"},
			},
		}},
	})
	return string(doc)
}

// flowLogPolicy lets the delivery role write to the network's log group
// and nothing else.
func (b networkBuilder) flowLogPolicy() string {
	group := "arn:" + Partition(b.p.Region) + ":logs:" + b.p.Region + ":" + b.p.AccountID + ":log-group:" + FlowLogGroupName(b.p.Prefix)
	doc, _ := json.Marshal(map[string]any{ // maps and strings: cannot fail
		"Version": "2012-10-17",
		"Statement": []map[string]any{
			{"Sid": "WriteFlowLogs", "Effect": "Allow",
				"Action":   []string{"logs:CreateLogStream", "logs:PutLogEvents", "logs:DescribeLogStreams"},
				"Resource": []string{group, group + ":*"}},
			{"Sid": "FindLogGroup", "Effect": "Allow", "Action": []string{"logs:DescribeLogGroups"}, "Resource": "*"},
		},
	})
	return string(doc)
}

// RetainAnnotation marks an imported object whose cloud resource others
// use too (a log group or role shared with other VPCs): a takeover manages
// it without Delete, so deleting the network leaves it in the cloud.
const RetainAnnotation = "cluster-vending.io/retain"

// PolicyNoDelete owns a cloud resource except for deleting it.
var PolicyNoDelete = xpv2.ManagementPolicies{xpv2.ManagementActionObserve, xpv2.ManagementActionCreate,
	xpv2.ManagementActionUpdate, xpv2.ManagementActionLateInitialize}

// ImportedFlowLog is a flow log of the VPC that delivers to CloudWatch
// Logs, with its log group and delivery role; Name, Group.Name and
// Role.Name are their managed resource names.
type ImportedFlowLog struct {
	Name, ID, TrafficType, LogFormat string
	MaxAggregationInterval           int
	Tags                             map[string]string
	Group                            ImportedLogGroup
	Role                             ImportedRole
}

// ImportedLogGroup is a CloudWatch Logs log group; LogGroupName is its
// external name. Shared: other VPCs' flow logs write to it too.
type ImportedLogGroup struct {
	Name, LogGroupName, ARN, KMSKeyID, Class string
	RetentionDays                            int
	Tags                                     map[string]string
	Shared                                   bool
}

// ImportedRole is an IAM role; RoleName is its external name. Shared:
// other VPCs' flow logs deliver with it too.
type ImportedRole struct {
	Name, RoleName, ARN, Path, AssumeRolePolicy, Description, PermissionsBoundary string
	MaxSessionDuration                                                            int
	Tags                                                                          map[string]string
	Shared                                                                        bool
}

// flowLogs renders the imported flow logs: each log group and role once
// (flow logs of one VPC may share them), then the flow logs.
func (b importBuilder) flowLogs() []client.Object {
	var out []client.Object
	seen := map[string]bool{}
	for _, f := range b.n.FlowLogs {
		if !seen[f.Group.Name] {
			seen[f.Group.Name] = true
			out = append(out, b.logGroup(f.Group))
		}
		if !seen[f.Role.Name] {
			seen[f.Role.Name] = true
			out = append(out, b.role(f.Role))
		}
	}
	for _, f := range b.n.FlowLogs {
		out = append(out, b.flowLog(f))
	}
	return out
}

// retain marks a shared resource (RetainAnnotation).
func retain(meta *metav1.ObjectMeta, shared bool) {
	if shared {
		meta.Annotations[RetainAnnotation] = "true"
	}
}

func (b importBuilder) logGroup(g ImportedLogGroup) *awscwl.Group {
	meta := b.meta(g.Name, g.LogGroupName, nil)
	retain(&meta, g.Shared)
	params := awscwl.GroupParameters{Region: ptr.To(b.n.Region), Tags: tagPtrs(g.Tags)}
	if g.RetentionDays > 0 {
		params.RetentionInDays = ptr.To(float64(g.RetentionDays))
	}
	if g.KMSKeyID != "" {
		params.KMSKeyID = ptr.To(g.KMSKeyID)
	}
	if g.Class != "" {
		params.LogGroupClass = ptr.To(g.Class)
	}
	return &awscwl.Group{ObjectMeta: meta, Spec: awscwl.GroupSpec{ManagedResourceSpec: b.spec(), ForProvider: params}}
}

func (b importBuilder) role(r ImportedRole) *awsiam.Role {
	meta := b.meta(r.Name, r.RoleName, nil)
	retain(&meta, r.Shared)
	params := awsiam.RoleParameters{AssumeRolePolicy: ptr.To(r.AssumeRolePolicy), Tags: tagPtrs(r.Tags)}
	if r.Path != "" {
		params.Path = ptr.To(r.Path)
	}
	if r.Description != "" {
		params.Description = ptr.To(r.Description)
	}
	if r.PermissionsBoundary != "" {
		params.PermissionsBoundary = ptr.To(r.PermissionsBoundary)
	}
	if r.MaxSessionDuration > 0 {
		params.MaxSessionDuration = ptr.To(float64(r.MaxSessionDuration))
	}
	if !r.Shared {
		// The role's policies are not imported: deleting the network deletes
		// the role with whatever is attached to it.
		params.ForceDetachPolicies = ptr.To(true)
	}
	return &awsiam.Role{ObjectMeta: meta, Spec: awsiam.RoleSpec{ManagedResourceSpec: b.spec(), ForProvider: params}}
}

func (b importBuilder) flowLog(f ImportedFlowLog) *awsec2.FlowLog {
	params := awsec2.FlowLogParameters{
		Region: ptr.To(b.n.Region), VPCID: ptr.To(b.n.VPC.ID), LogDestinationType: ptr.To(flowLogDestinationCloudWatch),
		LogDestination: ptr.To(f.Group.ARN), IAMRoleArn: ptr.To(f.Role.ARN), TrafficType: ptr.To(f.TrafficType),
		Tags: tagPtrs(f.Tags),
	}
	if f.LogFormat != "" {
		params.LogFormat = ptr.To(f.LogFormat)
	}
	if f.MaxAggregationInterval > 0 {
		params.MaxAggregationInterval = ptr.To(float64(f.MaxAggregationInterval))
	}
	return &awsec2.FlowLog{ObjectMeta: b.meta(f.Name, f.ID, nil),
		Spec: awsec2.FlowLogSpec{ManagedResourceSpec: b.spec(), ForProvider: params}}
}
