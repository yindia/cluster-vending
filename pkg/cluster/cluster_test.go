package cluster

import (
	"testing"

	"k8s.io/utils/ptr"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

const (
	viewPolicy = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy"
	devRole    = "arn:aws:iam::444455556666:role/dev"
)

func TestChooseSubnets(t *testing.T) {
	subnets := []PrivateSubnet{{ID: "subnet-b", Zone: "use1-az2", Name: "b"}, {ID: "subnet-a", Zone: "use1-az1", Name: "a"},
		{ID: "subnet-c", Zone: "use1-az4", Name: "c"}}
	tests := []struct {
		name      string
		zones     []string
		wantZones []string
		wantIDs   []string
		wantErr   bool
	}{
		{"every zone by default", nil, []string{"use1-az1", "use1-az2", "use1-az4"}, []string{"subnet-a", "subnet-b", "subnet-c"}, false},
		{"requested zones", []string{"use1-az4", "use1-az1"}, []string{"use1-az4", "use1-az1"}, []string{"subnet-c", "subnet-a"}, false},
		{"zone without subnet", []string{"use1-az1", "use1-az6"}, nil, nil, true},
		{"one zone", []string{"use1-az1"}, nil, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			zones, ids, err := ChooseSubnets(subnets, tt.zones)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !equal(zones, tt.wantZones) || !equal(ids, tt.wantIDs) {
				t.Errorf("got %v %v, want %v %v", zones, ids, tt.wantZones, tt.wantIDs)
			}
		})
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func minimalSpec() *cloudv1.ClusterSpec {
	return &cloudv1.ClusterSpec{
		Region:       "us-east-1",
		Network:      &cloudv1.ClusterNetwork{NetworkId: "0190a000-0000-7000-8000-000000000001"},
		ControlPlane: &cloudv1.ControlPlaneSpec{EndpointAccess: &cloudv1.EndpointAccess{PublicAccessCidrs: []string{"203.0.113.0/24"}}},
		DefaultNodePool: &cloudv1.NodePoolSpec{InstanceTypes: []string{"m6i.large"},
			ScalingConfig: &cloudv1.ScalingConfig{MinSize: 2, MaxSize: 4}},
		Provider: &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{}},
	}
}

// mustResolve is ResolveSpec for specs that are valid.
func mustResolve(t *testing.T, in *cloudv1.ClusterSpec, zones []string) *cloudv1.ClusterSpec {
	t.Helper()
	spec, err := ResolveSpec(in, zones)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func TestResolveSpecFillsDefaultsWithoutTouchingInput(t *testing.T) {
	in := minimalSpec()
	spec := mustResolve(t, in, []string{"use1-az1", "use1-az2"})
	if in.GetAws().GetAccessConfig() != nil || len(in.GetNetwork().GetZoneIds()) != 0 {
		t.Fatal("ResolveSpec changed its input")
	}
	if got := spec.GetControlPlane().GetEndpointAccess().GetPublicAccessCidrs(); !equal(got, []string{"203.0.113.0/24"}) {
		t.Errorf("public cidrs = %v, want the client's", got)
	}
	in.ControlPlane = nil
	if _, err := ResolveSpec(in, []string{"use1-az1", "use1-az2"}); err == nil {
		t.Error("no public access CIDRs: want an error, not a 0.0.0.0/0 default")
	}
	plan := RenderPlan(spec, []string{"subnet-a", "subnet-b"}, crossplaneawsBase())
	if plan.AuthenticationMode != DefaultAuthMode || plan.NodePool.CapacityType != DefaultCapacityType ||
		plan.NodePool.AMIType != DefaultAMIType {
		t.Errorf("plan defaults = %q %q %q", plan.AuthenticationMode, plan.NodePool.CapacityType, plan.NodePool.AMIType)
	}
	if plan.NodePool.Desired != 2 {
		t.Errorf("desired = %d, want min_size 2", plan.NodePool.Desired)
	}
}

func crossplaneawsBase() crossplaneaws.ClusterPlan {
	return crossplaneaws.ClusterPlan{Namespace: "tenant-acme", ProviderConfigName: "aws-1", TenantID: "acme",
		ClusterID: "id", ClusterName: "prod", Prefix: "prod-12345678"}
}

func addonNames(addons []*cloudv1.AwsAddon) []string {
	out := make([]string, 0, len(addons))
	for _, a := range addons {
		out = append(out, a.GetName())
	}
	return out
}

func TestMergeAddons(t *testing.T) {
	got := MergeAddons([]*cloudv1.AwsAddon{
		{Name: "aws-efs-csi-driver"}, {Name: "coredns", ConfigurationValues: `{"replicaCount":3}`},
	})
	want := []string{"vpc-cni", "coredns", "kube-proxy", "eks-pod-identity-agent", "aws-ebs-csi-driver", "aws-efs-csi-driver"}
	if !equal(addonNames(got), want) {
		t.Fatalf("merged = %v, want %v", addonNames(got), want)
	}
	if got[1].GetConfigurationValues() != `{"replicaCount":3}` {
		t.Error("an entry with a default's name must replace the default")
	}
}

func TestObjectNames(t *testing.T) {
	subnets := []PrivateSubnet{{ID: "subnet-1", Zone: "az1", Name: "net-private-az1"}, {ID: "subnet-2", Zone: "az2", Name: "net-private-az2"}}
	got := SubnetObjectNames(subnets, []string{"subnet-2", "subnet-1"})
	if !equal(got, []string{"net-private-az2", "net-private-az1"}) {
		t.Errorf("SubnetObjectNames = %v", got)
	}
}

// A spec without the optional settings resolves to what it resolved to
// before they existed; with them, their defaults are filled in.
func TestResolveOptions(t *testing.T) {
	zones := []string{"use1-az1", "use1-az2"}
	plain := mustResolve(t, minimalSpec(), zones)
	if plain.GetAws().GetControlPlaneLogGroup() != nil || plain.GetAccess().GetAccessEntries() != nil ||
		plain.GetControlPlane().GetEndpointAccess().EndpointPublicAccess != nil || plain.GetAws().Irsa != nil ||
		plain.GetAws().GetDefaultNodePool().GetLaunchTemplate() != nil {
		t.Errorf("optional settings appeared in a spec without them: %v", plain)
	}
	base := crossplaneaws.ClusterPlan{Prefix: "prod", ClusterName: "prod"}
	if p := RenderPlan(plain, []string{"subnet-a"}, base); len(p.Pending()) != 0 || p.LogGroupRetentionDays != 0 ||
		p.CreateEncryptionKey || p.EncryptionKeyARN != "" || p.IRSA || p.NodePool.LaunchTemplate != nil || p.AccessEntries != nil ||
		p.EndpointPublicAccess != nil || p.AdditionalSecurityGroupIDs != nil {
		t.Errorf("plan of a spec without options = %+v", p)
	}

	in := minimalSpec()
	in.GetAws().ControlPlaneLogGroup = &cloudv1.AwsControlPlaneLogGroup{Enabled: ptr.To(true)}
	in.Access = &cloudv1.AccessSpec{AccessEntries: []*cloudv1.AccessEntry{{PrincipalArn: devRole,
		Policies: []*cloudv1.AccessPolicy{{PolicyArn: viewPolicy}}}}}
	in.GetAws().DefaultNodePool = &cloudv1.AwsNodePoolSpec{LaunchTemplate: &cloudv1.AwsLaunchTemplate{ImageId: "ami-0123456789abcdef0", UserData: "Zm9v"}}
	in.ControlPlane.SecretsEncryption = &cloudv1.SecretsEncryption{Enabled: true}
	got := mustResolve(t, in, zones)
	entry := got.GetAccess().GetAccessEntries()[0]
	if got.GetAws().GetControlPlaneLogGroup().GetRetentionDays() != 90 ||
		entry.GetType() != cloudv1.AccessEntryType_ACCESS_ENTRY_TYPE_STANDARD ||
		entry.GetPolicies()[0].GetScope() != cloudv1.AccessPolicyScope_ACCESS_POLICY_SCOPE_CLUSTER ||
		got.GetAws().GetDefaultNodePool().GetAmiType() != cloudv1.AwsAmiType_AWS_AMI_TYPE_CUSTOM {
		t.Errorf("defaults not filled: %v", got)
	}
	p := RenderPlan(got, []string{"subnet-a"}, base)
	if p.LogGroupRetentionDays != 90 || !p.CreateEncryptionKey || p.NodePool.AMIType != "CUSTOM" || !p.NodePool.LaunchTemplate.Rendered() ||
		p.AccessEntries[0].Type != "STANDARD" || p.AccessEntries[0].Policies[0].Scope != "cluster" {
		t.Errorf("plan = %+v", p)
	}

	for name, change := range map[string]func(*cloudv1.ClusterSpec){
		"launch template with a disk size": func(s *cloudv1.ClusterSpec) {
			s.DefaultNodePool.DiskSizeGb = 50
			s.GetAws().DefaultNodePool = &cloudv1.AwsNodePoolSpec{LaunchTemplate: &cloudv1.AwsLaunchTemplate{Id: "lt-0123456789abcdef0", Version: "1"}}
		},
		"public endpoint without CIDRs": func(s *cloudv1.ClusterSpec) { s.ControlPlane.EndpointAccess.PublicAccessCidrs = nil },
	} {
		bad := minimalSpec()
		change(bad)
		if _, err := ResolveSpec(bad, zones); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	private := minimalSpec()
	private.ControlPlane.EndpointAccess = &cloudv1.EndpointAccess{EndpointPublicAccess: ptr.To(false)}
	if got, err := ResolveSpec(private, zones); err != nil ||
		ptr.Deref(RenderPlan(got, nil, base).EndpointPublicAccess, true) {
		t.Errorf("private-only endpoint without CIDRs: %v", err)
	}
}
