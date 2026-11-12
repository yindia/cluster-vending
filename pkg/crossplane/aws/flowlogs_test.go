package aws_test

import (
	"encoding/json"
	"strings"
	"testing"

	protectionv1beta1 "github.com/crossplane/crossplane/apis/v2/protection/v1beta1"
	awscwl "github.com/upbound/provider-aws/v2/apis/namespaced/cloudwatchlogs/v1beta1"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

func twoZonePlan() crossplaneaws.NetworkPlan {
	return plan(
		crossplaneaws.SubnetPlan{Role: crossplaneaws.SubnetPublic, CIDR: "10.0.0.0/24", ZoneID: "use1-az1"},
		crossplaneaws.SubnetPlan{Role: crossplaneaws.SubnetPrivate, CIDR: "10.0.16.0/20", ZoneID: "use1-az1"},
		crossplaneaws.SubnetPlan{Role: crossplaneaws.SubnetPrivate, CIDR: "10.0.32.0/20", ZoneID: "use1-az2"},
	)
}

func render(t *testing.T, p crossplaneaws.NetworkPlan) (map[string]client.Object, string) {
	t.Helper()
	objects, err := crossplaneaws.NetworkResources(p)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(objects)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]client.Object{}
	for _, o := range objects {
		byName[o.GetName()] = o
	}
	return byName, string(raw)
}

func TestNetworkResourcesFlowLogs(t *testing.T) {
	without, base := render(t, twoZonePlan())
	// The account ID alone changes nothing: networks without flow logs
	// render as before.
	p := twoZonePlan()
	p.AccountID = "111122223333"
	if _, same := render(t, p); same != base {
		t.Error("the render changed without flow logs")
	}
	p.FlowLogs = &crossplaneaws.FlowLogPlan{TrafficType: "REJECT", RetentionDays: 30, AggregationSeconds: 60}
	with, _ := render(t, p)
	if len(with) != len(without)+7 {
		t.Fatalf("flow logs add %d objects, want 7 (group, role, policy, attachment, flow log, 2 usages)", len(with)-len(without))
	}

	group := with["prod-flow-logs"].(*awscwl.Group)
	// cloudwatchlogs Group: config.NameAsIdentifier, the log group name.
	if got := group.Annotations[crossplaneaws.ExternalNameAnnotation]; got != "/cluster-vending/network/prod/flow-logs" ||
		got != crossplaneaws.FlowLogGroupName("prod") || *group.Spec.ForProvider.RetentionInDays != 30 {
		t.Errorf("log group = %v %+v", group.Annotations, group.Spec.ForProvider)
	}
	role := with["prod-flow-logs-role"].(*awsiam.Role)
	if trust := *role.Spec.ForProvider.AssumeRolePolicy; !strings.Contains(trust, "vpc-flow-logs.amazonaws.com") ||
		!strings.Contains(trust, `"aws:SourceAccount":"111122223333"`) {
		t.Errorf("trust policy = %s", trust)
	}
	policy := with["prod-flow-logs-policy"].(*awsiam.Policy)
	if doc := *policy.Spec.ForProvider.Policy; !strings.Contains(doc,
		"arn:aws:logs:us-east-1:111122223333:log-group:/cluster-vending/network/prod/flow-logs:*") {
		t.Errorf("delivery policy is not scoped to the log group: %s", doc)
	}
	attachment := with["prod-flow-logs-attachment"].(*awsiam.RolePolicyAttachment)
	if attachment.Spec.ForProvider.RoleRef.Name != role.Name || attachment.Spec.ForProvider.PolicyArnRef.Name != policy.Name {
		t.Errorf("attachment = %+v", attachment.Spec.ForProvider)
	}
	fl := with["prod-flow-log"].(*awsec2.FlowLog).Spec.ForProvider
	if fl.VPCIDRef.Name != "prod-vpc" || fl.LogDestinationRef.Name != group.Name || fl.IAMRoleArnRef.Name != role.Name ||
		*fl.LogDestinationType != "cloud-watch-logs" || *fl.TrafficType != "REJECT" || *fl.MaxAggregationInterval != 60 {
		t.Errorf("flow log = %+v", fl)
	}
	// Usages: the group and the role cannot go while the flow log exists.
	held := map[string]bool{}
	for _, o := range with {
		u, ok := o.(*protectionv1beta1.Usage)
		if !ok {
			continue
		}
		if u.Spec.By.Kind != "FlowLog" || u.Spec.By.ResourceRef.Name != "prod-flow-log" || with[u.Spec.Of.ResourceRef.Name] == nil {
			t.Errorf("usage %s = %+v", u.Name, u.Spec)
		}
		held[u.Spec.Of.Kind] = true
	}
	if !held["Group"] || !held["Role"] || len(held) != 2 {
		t.Errorf("usages hold %v, want the Group and the Role", held)
	}

	p.AccountID = ""
	if _, err := crossplaneaws.NetworkResources(p); err == nil {
		t.Error("flow logs without an account ID accepted")
	}
}

// External names as provider-upjet-aws v2.9.0 config/externalname.go has
// them: aws_flow_log IdentifierFromProvider (the fl- ID),
// aws_cloudwatch_log_group and aws_iam_role NameAsIdentifier (the name).
func TestImportedFlowLogExternalNames(t *testing.T) {
	group := crossplaneaws.ImportedLogGroup{Name: "tf-flow-logs", LogGroupName: "/vpc/prod",
		ARN: "arn:aws:logs:us-east-1:111122223333:log-group:/vpc/prod", RetentionDays: 14, Shared: true}
	role := crossplaneaws.ImportedRole{Name: "tf-flow-logs-role", RoleName: "flow-logs",
		ARN: "arn:aws:iam::111122223333:role/svc/flow-logs", Path: "/svc/", AssumeRolePolicy: "{}"}
	objects := crossplaneaws.ImportedNetworkResources(crossplaneaws.ImportedNetwork{
		Namespace: "tenant-acme", ProviderConfigName: "aws-1", TenantID: "acme", NetworkID: "id1", Prefix: "tf", Region: "us-east-1",
		VPC: crossplaneaws.ImportedVPC{ID: "vpc-1", CIDRBlock: "10.0.0.0/16"},
		FlowLogs: []crossplaneaws.ImportedFlowLog{
			{Name: "tf-flow-log", ID: "fl-1", TrafficType: "ACCEPT", Group: group, Role: role},
			{Name: "tf-flow-log-2", ID: "fl-2", TrafficType: "REJECT", Group: group, Role: role},
		},
	})
	want := []struct{ kind, name, externalName string }{
		{"*v1beta1.VPC", "tf-vpc", "vpc-1"},
		{"*v1beta1.Group", "tf-flow-logs", "/vpc/prod"},
		{"*v1beta1.Role", "tf-flow-logs-role", "flow-logs"},
		{"*v1beta1.FlowLog", "tf-flow-log", "fl-1"},
		{"*v1beta1.FlowLog", "tf-flow-log-2", "fl-2"},
	}
	if len(objects) != len(want) {
		t.Fatalf("got %d objects, want %d (the shared group and role once)", len(objects), len(want))
	}
	for i, w := range want {
		o := objects[i]
		if o.GetName() != w.name || o.GetAnnotations()[crossplaneaws.ExternalNameAnnotation] != w.externalName {
			t.Errorf("object %d = %s (external name %s), want %s (%s)", i, o.GetName(),
				o.GetAnnotations()[crossplaneaws.ExternalNameAnnotation], w.name, w.externalName)
		}
	}
	g := objects[1].(*awscwl.Group)
	if g.Annotations[crossplaneaws.RetainAnnotation] != "true" || g.Spec.ManagementPolicies[0] != "Observe" ||
		*g.Spec.ForProvider.RetentionInDays != 14 {
		t.Errorf("shared log group = %v %+v", g.Annotations, g.Spec)
	}
	r := objects[2].(*awsiam.Role)
	if _, retained := r.Annotations[crossplaneaws.RetainAnnotation]; retained || !*r.Spec.ForProvider.ForceDetachPolicies ||
		*r.Spec.ForProvider.Path != "/svc/" {
		t.Errorf("role = %v %+v", r.Annotations, r.Spec.ForProvider)
	}
	fl := objects[3].(*awsec2.FlowLog).Spec.ForProvider
	if *fl.VPCID != "vpc-1" || *fl.LogDestination != group.ARN || *fl.IAMRoleArn != role.ARN || *fl.TrafficType != "ACCEPT" {
		t.Errorf("flow log = %+v", fl)
	}
}
