package aws_test

import (
	"reflect"
	"slices"
	"testing"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	awscwl "github.com/upbound/provider-aws/v2/apis/namespaced/cloudwatchlogs/v1beta1"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// importedForOwning is a network with a public and a private subnet, a
// route (a kind without tags) and a flow log whose log group other VPCs
// use too.
func importedForOwning() crossplaneaws.ImportedNetwork {
	return crossplaneaws.ImportedNetwork{
		Namespace: "tenant-acme", ProviderConfigName: "aws-1", TenantID: "acme", NetworkID: "id1", Prefix: "tf", Region: "us-east-1",
		VPC: crossplaneaws.ImportedVPC{ID: "vpc-1", CIDRBlock: "10.0.0.0/16", Tags: map[string]string{"Name": "tf", "team": "theirs"}},
		Subnets: []crossplaneaws.ImportedSubnet{
			{Name: "tf-public-use1-az1", ID: "subnet-1", CIDRBlock: "10.0.0.0/24", ZoneID: "use1-az1", Role: crossplaneaws.SubnetPublic},
			{Name: "tf-private-use1-az1", ID: "subnet-2", CIDRBlock: "10.0.16.0/20", ZoneID: "use1-az1", Role: crossplaneaws.SubnetPrivate},
		},
		RouteTables: []crossplaneaws.ImportedRouteTable{{Key: "public", ID: "rtb-1",
			DefaultRoute: &crossplaneaws.ImportedRoute{GatewayID: "igw-1"}}},
		FlowLogs: []crossplaneaws.ImportedFlowLog{{Name: "tf-flow-log", ID: "fl-1", TrafficType: "ALL",
			Group: crossplaneaws.ImportedLogGroup{Name: "tf-flow-logs", LogGroupName: "/vpc/tf", Shared: true},
			Role:  crossplaneaws.ImportedRole{Name: "tf-flow-logs-role", RoleName: "tf-flow-logs", AssumeRolePolicy: "{}"}}},
	}
}

func policiesOf(t *testing.T, obj client.Object) xpv2.ManagementPolicies {
	t.Helper()
	m, ok := obj.(interface {
		GetManagementPolicies() xpv2.ManagementPolicies
	})
	if !ok {
		t.Fatalf("%T is not a managed resource", obj)
	}
	return m.GetManagementPolicies()
}

// The owned render is the imported render with other management
// policies, the load balancer role tag on subnets and the labels as tags:
// the same objects, in the same order, and nothing else of them changed.
func TestOwnedNetworkResources(t *testing.T) {
	n := importedForOwning()
	observed := crossplaneaws.ImportedNetworkResources(n)
	owned := crossplaneaws.OwnedNetworkResources(n, map[string]string{"team": "net", "env": "prod"})
	if len(owned) != len(observed) {
		t.Fatalf("%d owned objects for %d imported ones", len(owned), len(observed))
	}
	for i, obj := range owned {
		if obj.GetName() != observed[i].GetName() || reflect.TypeOf(obj) != reflect.TypeOf(observed[i]) {
			t.Fatalf("object %d is %T %s, the import has %T %s", i, obj, obj.GetName(), observed[i], observed[i].GetName())
		}
		if !slices.Equal(policiesOf(t, observed[i]), crossplaneaws.PolicyObserve) {
			t.Errorf("%s: the imported render is not Observe-only any more: %v", obj.GetName(), policiesOf(t, observed[i]))
		}
		retained := obj.GetAnnotations()[crossplaneaws.RetainAnnotation] == "true"
		want := crossplaneaws.PolicyAll
		if retained {
			want = crossplaneaws.PolicyNoDelete
		}
		if got := policiesOf(t, obj); !slices.Equal(got, want) {
			t.Errorf("%s policies = %v, want %v", obj.GetName(), got, want)
		}
		if obj.GetAnnotations()[crossplaneaws.ExternalNameAnnotation] != observed[i].GetAnnotations()[crossplaneaws.ExternalNameAnnotation] ||
			obj.GetAnnotations()["cluster-vending.io/carried-over"] != "true" || !reflect.DeepEqual(obj.GetLabels(), observed[i].GetLabels()) {
			t.Errorf("%s: its identity changed: %v %v", obj.GetName(), obj.GetAnnotations(), obj.GetLabels())
		}
		tagged := obj.GetAnnotations()[crossplaneaws.LabelTagsAnnotation]
		switch o := obj.(type) {
		case *awsec2.VPC:
			tags := o.Spec.ForProvider.Tags
			if *tags["Name"] != "tf" || *tags["team"] != "net" || *tags["env"] != "prod" || len(tags) != 3 || tagged != "env,team" {
				t.Errorf("VPC tags = %v (label tags %q), want the import's Name, the labels over its team", deref(tags), tagged)
			}
		case *awsec2.Subnet:
			role := crossplaneaws.TagInternalELB
			if o.Name == "tf-public-use1-az1" {
				role = crossplaneaws.TagELB
			}
			if tags := o.Spec.ForProvider.Tags; tags[role] == nil || *tags[role] != "1" || len(tags) != 3 || tagged != "env,team" {
				t.Errorf("subnet %s tags = %v, want %s and the labels", o.Name, deref(tags), role)
			}
		case *awscwl.Group:
			if !retained || o.Spec.ForProvider.Tags != nil || tagged != "" {
				t.Errorf("the shared log group is tagged: %v (%q)", deref(o.Spec.ForProvider.Tags), tagged)
			}
		case *awsec2.Route:
			if tagged != "" {
				t.Errorf("a route (no tags) lists label tags: %q", tagged)
			}
		}
	}
	// Without labels the only tags a takeover adds are the subnets' roles.
	for _, obj := range crossplaneaws.OwnedNetworkResources(n, nil) {
		if _, has := obj.GetAnnotations()[crossplaneaws.LabelTagsAnnotation]; has {
			t.Errorf("%s lists label tags without labels", obj.GetName())
		}
		if vpc, ok := obj.(*awsec2.VPC); ok && (len(vpc.Spec.ForProvider.Tags) != 2 || *vpc.Spec.ForProvider.Tags["team"] != "theirs") {
			t.Errorf("VPC tags without labels = %v, want the import's", deref(vpc.Spec.ForProvider.Tags))
		}
	}
}

func deref(tags map[string]*string) map[string]string {
	out := make(map[string]string, len(tags))
	for k, v := range tags {
		out[k] = ptr.Deref(v, "")
	}
	return out
}
