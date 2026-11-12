package network

import (
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	protectionv1beta1 "github.com/crossplane/crossplane/apis/v2/protection/v1beta1"
	awscwl "github.com/upbound/provider-aws/v2/apis/namespaced/cloudwatchlogs/v1beta1"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func TestCreateNetworkWithoutFlowLogs(t *testing.T) {
	f := newFixture(t)
	for name, flowLogs := range map[string]*cloudv1.NetworkFlowLogs{
		"unset": nil, "disabled": {Enabled: ptr.To(false), RetentionDays: 7}, "empty": {},
	} {
		req := f.request(name, nil)
		req.Network.Spec.FlowLogs = flowLogs
		res, err := f.svc.CreateNetwork(t.Context(), req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		n := f.attachGateway(t, res.GetNetwork().GetId())
		if f.count(t, &awsec2.FlowLogList{}, n.GetId())+f.count(t, &awscwl.GroupList{}, n.GetId())+
			f.count(t, &awsiam.RoleList{}, n.GetId())+f.count(t, &protectionv1beta1.UsageList{}, n.GetId()) != 0 {
			t.Errorf("%s: flow log resources rendered", name)
		}
		// 3 zones: vpc, igw, 6 subnets, eip + nat, 2 route tables, 2 routes, 6 associations.
		if got := len(n.GetStatus().GetManagedResources()); got != 20 {
			t.Errorf("%s: %d managed resources, want 20 as before flow logs existed", name, got)
		}
	}
}

func TestCreateNetworkWithFlowLogs(t *testing.T) {
	f := newFixture(t)
	req := f.request("prod", nil)
	req.Network.Spec.FlowLogs = &cloudv1.NetworkFlowLogs{Enabled: ptr.To(true)}
	res, err := f.svc.CreateNetwork(t.Context(), req)
	if err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	n := f.attachGateway(t, res.GetNetwork().GetId())
	// The stored spec holds the defaults.
	if fl := n.GetSpec().GetFlowLogs(); fl.GetTrafficType() != cloudv1.FlowLogTrafficType_FLOW_LOG_TRAFFIC_TYPE_ALL ||
		fl.GetRetentionDays() != 30 || fl.GetAggregationIntervalSeconds() != 600 {
		t.Errorf("stored flow logs = %v, want ALL / 30 days / 600s", fl)
	}
	flowLog := &awsec2.FlowLog{}
	if err := f.kube.Get(t.Context(), key(n, "flow-log"), flowLog); err != nil {
		t.Fatal(err)
	}
	if *flowLog.Spec.ForProvider.TrafficType != "ALL" || flowLog.Spec.ForProvider.LogDestinationRef.Name != key(n, "flow-logs").Name {
		t.Errorf("flow log = %+v", flowLog.Spec.ForProvider)
	}
	policy := &awsiam.Policy{}
	if err := f.kube.Get(t.Context(), key(n, "flow-logs-policy"), policy); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(*policy.Spec.ForProvider.Policy, ":444455556666:log-group:") {
		t.Errorf("delivery policy lacks the account's log group ARN: %s", *policy.Spec.ForProvider.Policy)
	}
	if got := len(n.GetStatus().GetManagedResources()); got != 27 {
		t.Errorf("managed resources = %d, want 20 + group, role, policy, attachment, flow log, 2 usages", got)
	}

	bad := f.request("bad", nil)
	bad.Network.Spec.FlowLogs = &cloudv1.NetworkFlowLogs{Enabled: ptr.To(true), RetentionDays: 31}
	if _, err := f.svc.CreateNetwork(t.Context(), bad); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("retention of 31 days = %v, want INVALID_ARGUMENT", err)
	}
}

// The flow log goes before its log group and role, everything before the
// VPC, and the network ends with nothing left.
func TestDeleteNetworkWithFlowLogs(t *testing.T) {
	f := newFixture(t)
	req := f.request("prod", nil)
	req.Network.Spec.FlowLogs = &cloudv1.NetworkFlowLogs{Enabled: ptr.To(true)}
	res, err := f.svc.CreateNetwork(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	n := f.attachGateway(t, res.GetNetwork().GetId())
	ctx := t.Context()
	count := func(list client.ObjectList) int { return f.count(t, list, n.GetId()) }
	if _, err := f.svc.DeleteNetwork(ctx, &cloudv1.DeleteNetworkRequest{TenantId: tenant, NetworkId: n.GetId()}); err != nil {
		t.Fatalf("DeleteNetwork: %v", err)
	}
	if count(&awsec2.FlowLogList{}) != 0 || count(&awscwl.GroupList{}) != 1 || count(&awsiam.RoleList{}) != 1 {
		t.Fatalf("after delete: flow logs %d, groups %d, roles %d; want the flow log gone first",
			count(&awsec2.FlowLogList{}), count(&awscwl.GroupList{}), count(&awsiam.RoleList{}))
	}
	for i := 0; ; i++ {
		if _, err := f.svc.GetNetwork(ctx, &cloudv1.GetNetworkRequest{TenantId: tenant, NetworkId: n.GetId()}); connect.CodeOf(err) == connect.CodeNotFound {
			break
		} else if err != nil || i > 10 {
			t.Fatalf("get %d: %v", i, err)
		}
		if err := f.svc.Reconcile(ctx, tenant, n.GetId()); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
		if count(&awsiam.RolePolicyAttachmentList{}) > 0 && (count(&awsiam.RoleList{}) == 0 || count(&awsiam.PolicyList{}) == 0) {
			t.Fatal("role or policy deleted before their attachment")
		}
		if count(&awsec2.VPCList{}) == 0 && count(&awscwl.GroupList{})+count(&awsiam.RoleList{})+count(&awsiam.PolicyList{}) > 0 {
			t.Fatal("VPC deleted before the flow log's log group and role")
		}
	}
	if left := count(&awsec2.VPCList{}) + count(&awscwl.GroupList{}) + count(&awsiam.RoleList{}) + count(&protectionv1beta1.UsageList{}); left != 0 {
		t.Errorf("%d objects left after the network is gone", left)
	}
}

func TestDeleteOrderTiers(t *testing.T) {
	tier := func(kind string) int {
		return slices.IndexFunc(deleteOrder, func(kinds []string) bool { return slices.Contains(kinds, kind) })
	}
	for _, o := range []struct{ first, then string }{
		{"FlowLog", "Group"}, {"FlowLog", "Role"}, {"FlowLog", "Usage"}, {"RolePolicyAttachment", "Role"},
		{"RolePolicyAttachment", "Policy"}, {"Group", "VPC"}, {"Role", "VPC"}, {"Policy", "VPC"},
		{"Route", "RouteTable"}, {"NATGateway", "EIP"}, {"Subnet", "VPC"},
	} {
		if a, b := tier(o.first), tier(o.then); a < 0 || b < 0 || a >= b {
			t.Errorf("%s (tier %d) must be deleted before %s (tier %d)", o.first, a, o.then, b)
		}
	}
}

// vpcWithFlowLogs is existingVPC with a flow log to a CloudWatch Logs
// group other VPCs write to as well, a second one to the same group and
// role, one to S3 and one whose role cannot be read.
func vpcWithFlowLogs() *awscloud.Network {
	d := existingVPC()
	group := &awscloud.LogGroup{Name: "/vpc/tf", ARN: "arn:aws:logs:us-east-1:444455556666:log-group:/vpc/tf",
		RetentionDays: 90, Shared: true}
	role := &awscloud.Role{Name: "tf-flow-logs", ARN: "arn:aws:iam::444455556666:role/tf-flow-logs", AssumeRolePolicy: "{}"}
	d.FlowLogs = []awscloud.FlowLog{
		{ID: "fl-accept", DestinationType: awscloud.FlowLogToCloudWatch, TrafficType: "ACCEPT", LogGroup: group, Role: role},
		{ID: "fl-reject", DestinationType: awscloud.FlowLogToCloudWatch, TrafficType: "REJECT", LogGroup: group, Role: role},
		{ID: "fl-s3", DestinationType: awscloud.FlowLogToS3, Destination: "arn:aws:s3:::flow-logs"},
		{ID: "fl-denied", DestinationType: awscloud.FlowLogToCloudWatch, Unreadable: "its delivery role cannot be read: access denied"},
	}
	return d
}

func TestImportNetworkFlowLogs(t *testing.T) {
	f := newFixture(t)
	f.svc.deps.AWSDiscovery = fakeDiscoverer{network: vpcWithFlowLogs()}
	f.svc.deps.AWS.StaticKeys = testKeys
	res, err := f.svc.ImportNetwork(t.Context(), importRequest(f.accountID))
	if err != nil {
		t.Fatalf("ImportNetwork: %v", err)
	}
	n := res.GetNetwork()
	lines := map[string]*cloudv1.ImportedResource{}
	for _, l := range n.GetStatus().GetImport().GetResources() {
		lines[l.GetKind()+"/"+l.GetExternalName()] = l
	}
	carried, notImported := cloudv1.Disposition_DISPOSITION_CARRIED_OVER, cloudv1.Disposition_DISPOSITION_NOT_IMPORTED
	for _, w := range []struct {
		line, apiVersion string
		disposition      cloudv1.Disposition
		detail           string
	}{
		{"FlowLog/fl-accept", ec2APIVersion, carried, ""},
		{"FlowLog/fl-reject", ec2APIVersion, carried, ""},
		{"Group//vpc/tf", logsAPIVersion, carried, "owned without Delete"},
		{"Role/tf-flow-logs", iamAPIVersion, carried, ""},
		{"FlowLog/fl-s3", ec2APIVersion, notImported, "delivers to s3"},
		{"FlowLog/fl-denied", ec2APIVersion, notImported, "delivery role cannot be read"},
	} {
		l := lines[w.line]
		if l.GetDisposition() != w.disposition || l.GetApiVersion() != w.apiVersion || !strings.Contains(l.GetDetail(), w.detail) ||
			(w.disposition == carried) != (l.GetName() != "") {
			t.Errorf("%s = %v, want %s %q", w.line, l, w.disposition, w.detail)
		}
	}
	if n.GetStatus().GetState() != cloudv1.NetworkState_NETWORK_STATE_IMPORTED || n.GetSpec().GetFlowLogs() != nil {
		t.Errorf("state %s, spec.flow_logs %v: flow logs do not block and are not part of the spec",
			n.GetStatus().GetState(), n.GetSpec().GetFlowLogs())
	}
	// 20 as without flow logs + the group and the role once + 2 flow logs.
	if got := len(n.GetStatus().GetManagedResources()); got != 24 {
		t.Errorf("managed resources = %d, want 24", got)
	}
	group, role, flowLog := &awscwl.Group{}, &awsiam.Role{}, &awsec2.FlowLog{}
	for name, obj := range map[string]client.Object{"flow-logs": group, "flow-logs-role": role, "flow-log-2": flowLog} {
		if err := f.kube.Get(t.Context(), key(n, name), obj); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if group.Spec.ManagementPolicies[0] != "Observe" || len(group.Spec.ManagementPolicies) != 1 ||
		*flowLog.Spec.ForProvider.TrafficType != "REJECT" {
		t.Errorf("imported: group policies %v, flow log %+v", group.Spec.ManagementPolicies, flowLog.Spec.ForProvider)
	}

	// The takeover owns everything; the shared log group without Delete.
	if _, err := f.svc.TakeOverNetwork(t.Context(), &cloudv1.TakeOverNetworkRequest{TenantId: tenant, NetworkId: n.GetId(),
		AcknowledgeNotImported: true}); err != nil {
		t.Fatalf("TakeOverNetwork: %v", err)
	}
	for name, obj := range map[string]client.Object{"flow-logs": group, "flow-logs-role": role, "flow-log": flowLog} {
		if err := f.kube.Get(t.Context(), key(n, name), obj); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if slices.Contains(group.Spec.ManagementPolicies, "Delete") || slices.Contains(group.Spec.ManagementPolicies, "*") ||
		!slices.Contains(group.Spec.ManagementPolicies, "Update") {
		t.Errorf("shared log group after takeover = %v, want everything but Delete", group.Spec.ManagementPolicies)
	}
	if role.Spec.ManagementPolicies[0] != "*" || flowLog.Spec.ManagementPolicies[0] != "*" {
		t.Errorf("after takeover: role %v, flow log %v", role.Spec.ManagementPolicies, flowLog.Spec.ManagementPolicies)
	}
}

// Flow logs are turned on and off, and their retention changed, on a
// network that exists.
func TestUpdateNetworkFlowLogs(t *testing.T) {
	f := newFixture(t)
	n := f.create(t, "prod", nil)
	ctx := t.Context()
	count := func(list client.ObjectList) int { return f.count(t, list, n.GetId()) }
	update := func(flowLogs *cloudv1.NetworkFlowLogs) (*cloudv1.Network, error) {
		res, err := f.svc.UpdateNetwork(ctx, &cloudv1.UpdateNetworkRequest{TenantId: tenant, UpdateMask: mask("spec.flow_logs"),
			Network: &cloudv1.Network{Id: n.GetId(), Spec: &cloudv1.NetworkSpec{FlowLogs: flowLogs}}})
		return res.GetNetwork(), err
	}

	on, err := update(&cloudv1.NetworkFlowLogs{Enabled: ptr.To(true)})
	if err != nil {
		t.Fatalf("turn on: %v", err)
	}
	if count(&awsec2.FlowLogList{}) != 1 || count(&awscwl.GroupList{}) != 1 || on.GetSpec().GetFlowLogs().GetRetentionDays() != 30 ||
		len(on.GetStatus().GetManagedResources()) != 27 {
		t.Fatalf("after turning on: %d flow logs, %d groups, spec %v, %d resources", count(&awsec2.FlowLogList{}), count(&awscwl.GroupList{}),
			on.GetSpec().GetFlowLogs(), len(on.GetStatus().GetManagedResources()))
	}
	if _, err := update(&cloudv1.NetworkFlowLogs{Enabled: ptr.To(true), RetentionDays: 90}); err != nil {
		t.Fatalf("change retention: %v", err)
	}
	group := &awscwl.Group{}
	if err := f.kube.Get(ctx, key(n, "flow-logs"), group); err != nil || *group.Spec.ForProvider.RetentionInDays != 90 {
		t.Errorf("retention = %v (%v), want 90", group.Spec.ForProvider.RetentionInDays, err)
	}
	// What AWS cannot change on an existing flow log is refused.
	if _, err := update(&cloudv1.NetworkFlowLogs{Enabled: ptr.To(true), RetentionDays: 90,
		TrafficType: cloudv1.FlowLogTrafficType_FLOW_LOG_TRAFFIC_TYPE_REJECT}); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("traffic type change = %v, want FAILED_PRECONDITION", err)
	}

	off, err := update(nil)
	if err != nil {
		t.Fatalf("turn off: %v", err)
	}
	if left := count(&awsec2.FlowLogList{}) + count(&awscwl.GroupList{}) + count(&awsiam.RoleList{}) + count(&awsiam.PolicyList{}) +
		count(&awsiam.RolePolicyAttachmentList{}) + count(&protectionv1beta1.UsageList{}); left != 0 {
		t.Errorf("%d flow log objects left after turning off", left)
	}
	// The network itself is untouched.
	if count(&awsec2.VPCList{}) != 1 || count(&awsec2.NATGatewayList{}) != 1 || len(off.GetStatus().GetManagedResources()) != 20 ||
		off.GetSpec().GetFlowLogs() != nil {
		t.Errorf("after turning off: %d VPCs, %d NAT gateways, %d resources, spec %v", count(&awsec2.VPCList{}),
			count(&awsec2.NATGatewayList{}), len(off.GetStatus().GetManagedResources()), off.GetSpec().GetFlowLogs())
	}
}

// An imported network's labels are set as tags on the objects it has; its
// other tags stay, and it is not rendered again.
func TestUpdateImportedNetworkLabels(t *testing.T) {
	f := newFixture(t)
	f.svc.deps.AWSDiscovery = fakeDiscoverer{network: existingVPC()}
	f.svc.deps.AWS.StaticKeys = testKeys
	ctx := t.Context()
	res, err := f.svc.ImportNetwork(ctx, importRequest(f.accountID))
	if err != nil {
		t.Fatal(err)
	}
	n := res.GetNetwork()
	if _, err := f.svc.TakeOverNetwork(ctx, &cloudv1.TakeOverNetworkRequest{TenantId: tenant, NetworkId: n.GetId(),
		AcknowledgeNotImported: true}); err != nil {
		t.Fatal(err)
	}
	before := len(n.GetStatus().GetManagedResources())
	label := func(labels map[string]string) *cloudv1.Network {
		t.Helper()
		out, err := f.svc.UpdateNetwork(ctx, &cloudv1.UpdateNetworkRequest{TenantId: tenant,
			Network: &cloudv1.Network{Id: n.GetId(), Labels: labels}, UpdateMask: mask("labels")})
		if err != nil {
			t.Fatalf("UpdateNetwork: %v", err)
		}
		return out.GetNetwork()
	}
	tags := func() map[string]*string {
		t.Helper()
		vpc := &awsec2.VPC{}
		if err := f.kube.Get(ctx, key(n, "vpc"), vpc); err != nil {
			t.Fatal(err)
		}
		return vpc.Spec.ForProvider.Tags
	}

	got := label(map[string]string{"team": "net", "env": "prod"})
	if vpcTags := tags(); *vpcTags["team"] != "net" || *vpcTags["env"] != "prod" || *vpcTags["Name"] != "tf" {
		t.Errorf("VPC tags = %v, want the labels next to the tags it was imported with", vpcTags)
	}
	if len(got.GetStatus().GetManagedResources()) != before {
		t.Errorf("%d resources after a label update, want the %d it was imported with", len(got.GetStatus().GetManagedResources()), before)
	}
	// A removed label is removed; the imported tags stay.
	label(map[string]string{"team": "net"})
	if vpcTags := tags(); vpcTags["env"] != nil || *vpcTags["team"] != "net" || *vpcTags["Name"] != "tf" {
		t.Errorf("VPC tags after removing a label = %v", vpcTags)
	}
	// Kinds without tags are left alone.
	route := &awsec2.Route{}
	if err := f.kube.Get(ctx, key(n, "route-public"), route); err != nil {
		t.Fatal(err)
	}
	row, err := f.store.Networks().Get(ctx, tenant, n.GetId())
	if err != nil || row.AppliedGeneration != row.Generation {
		t.Errorf("applied generation %d of %d (%v), want it settled", row.AppliedGeneration, row.Generation, err)
	}
}
