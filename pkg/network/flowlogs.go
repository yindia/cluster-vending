package network

import (
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// ResolveFlowLogs is spec.flow_logs as stored: with its defaults when
// enabled, so later renders do not depend on them; unchanged otherwise.
func ResolveFlowLogs(in *cloudv1.NetworkFlowLogs) *cloudv1.NetworkFlowLogs {
	if !in.GetEnabled() {
		return in
	}
	out := proto.CloneOf(in)
	if out.GetTrafficType() == cloudv1.FlowLogTrafficType_FLOW_LOG_TRAFFIC_TYPE_UNSPECIFIED {
		out.TrafficType = cloudv1.FlowLogTrafficType_FLOW_LOG_TRAFFIC_TYPE_ALL
	}
	if out.GetRetentionDays() == 0 {
		out.RetentionDays = crossplaneaws.DefaultFlowLogRetentionDays
	}
	if out.GetAggregationIntervalSeconds() == 0 {
		out.AggregationIntervalSeconds = crossplaneaws.DefaultFlowLogAggregationSeconds
	}
	return out
}

// FlowLogPlan is the render plan of spec.flow_logs: nil unless enabled.
func FlowLogPlan(in *cloudv1.NetworkFlowLogs) *crossplaneaws.FlowLogPlan {
	if !in.GetEnabled() {
		return nil
	}
	in = ResolveFlowLogs(in)
	traffic := map[cloudv1.FlowLogTrafficType]string{
		cloudv1.FlowLogTrafficType_FLOW_LOG_TRAFFIC_TYPE_ACCEPT: "ACCEPT",
		cloudv1.FlowLogTrafficType_FLOW_LOG_TRAFFIC_TYPE_REJECT: "REJECT",
	}[in.GetTrafficType()]
	if traffic == "" {
		traffic = crossplaneaws.DefaultFlowLogTrafficType
	}
	return &crossplaneaws.FlowLogPlan{TrafficType: traffic, RetentionDays: int(in.GetRetentionDays()),
		AggregationSeconds: int(in.GetAggregationIntervalSeconds())}
}

// numbered is base for the first of its kind, "<base>-<n>" for the n-th.
func numbered(base string, n int) string {
	if n <= 1 {
		return base
	}
	return fmt.Sprintf("%s-%d", base, n)
}

// flowLogs imports the VPC's flow logs that deliver to CloudWatch Logs,
// with their log group and delivery role, as CARRIED_OVER: no spec field
// describes them, the takeover owns them as they are. A log group or role
// that flow logs of other VPCs use too is owned without Delete. Flow logs
// to S3 or Firehose, or whose log group or role cannot be read, are
// NOT_IMPORTED.
func (p *planner) flowLogs() {
	groups, roles := map[string]string{}, map[string]string{} // cloud name -> managed resource name
	for _, f := range p.d.FlowLogs {
		switch {
		case f.DestinationType != awscloud.FlowLogToCloudWatch:
			p.line("FlowLog", "", f.ID, "", cloudv1.Disposition_DISPOSITION_NOT_IMPORTED,
				fmt.Sprintf("delivers to %s (%s): only flow logs to CloudWatch Logs are imported", f.DestinationType, f.Destination))
			continue
		case f.Unreadable != "" || f.LogGroup == nil || f.Role == nil:
			p.line("FlowLog", "", f.ID, "", cloudv1.Disposition_DISPOSITION_NOT_IMPORTED, "flow log to CloudWatch Logs, but "+f.Unreadable)
			continue
		}
		g, r := *f.LogGroup, *f.Role
		if groups[g.Name] == "" {
			groups[g.Name] = numbered(p.base.Prefix+"-flow-logs", len(groups)+1)
			p.lineOf(LogsAPIVersion, "Group", groups[g.Name], g.Name, "", cloudv1.Disposition_DISPOSITION_CARRIED_OVER,
				sharedDetail(g.Shared, "log group"))
		}
		if roles[r.Name] == "" {
			roles[r.Name] = numbered(p.base.Prefix+"-flow-logs-role", len(roles)+1)
			p.lineOf(IAMAPIVersion, "Role", roles[r.Name], r.Name, "", cloudv1.Disposition_DISPOSITION_CARRIED_OVER,
				sharedDetail(r.Shared, "role"))
		}
		name := numbered(p.base.Prefix+"-flow-log", len(p.plan.Resources.FlowLogs)+1)
		p.line("FlowLog", name, f.ID, "", cloudv1.Disposition_DISPOSITION_CARRIED_OVER, "")
		p.plan.Resources.FlowLogs = append(p.plan.Resources.FlowLogs, crossplaneaws.ImportedFlowLog{
			Name: name, ID: f.ID, TrafficType: f.TrafficType, LogFormat: f.LogFormat,
			MaxAggregationInterval: f.MaxAggregationInterval, Tags: f.Tags,
			Group: crossplaneaws.ImportedLogGroup{Name: groups[g.Name], LogGroupName: g.Name, ARN: g.ARN, KMSKeyID: g.KMSKeyID,
				Class: g.Class, RetentionDays: g.RetentionDays, Tags: g.Tags, Shared: g.Shared},
			Role: crossplaneaws.ImportedRole{Name: roles[r.Name], RoleName: r.Name, ARN: r.ARN, Path: r.Path,
				AssumeRolePolicy: r.AssumeRolePolicy, Description: r.Description, PermissionsBoundary: r.PermissionsBoundary,
				MaxSessionDuration: r.MaxSessionDuration, Tags: r.Tags, Shared: r.Shared},
		})
	}
}

// sharedDetail says what a takeover does with a shared log group or role.
func sharedDetail(shared bool, what string) string {
	if !shared {
		return ""
	}
	return "flow logs of other VPCs use this " + what + " too: it is owned without Delete and stays when the network is deleted"
}

// FlowLogsChangeable refuses what AWS cannot change on a flow log that
// exists: its traffic type and aggregation interval.
func FlowLogsChangeable(before, after *cloudv1.NetworkFlowLogs) error {
	if !before.GetEnabled() || !after.GetEnabled() {
		return nil // turned on or off: created or deleted
	}
	if before.GetTrafficType() != after.GetTrafficType() ||
		before.GetAggregationIntervalSeconds() != after.GetAggregationIntervalSeconds() {
		return errors.New(
			"the traffic type and aggregation interval of existing flow logs cannot be changed; turn flow logs off, then on with the new settings")
	}
	return nil
}
