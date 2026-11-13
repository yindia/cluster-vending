package aws

import "testing"

// Only what cannot become a second cloud resource is created again; a
// kind nobody listed is not.
func TestRecreatable(t *testing.T) {
	tests := []struct {
		apiVersion, kind string
		want             bool
	}{
		{"helm.m.crossplane.io/v1beta1", "ProviderConfig", true},
		{"protection.crossplane.io/v1beta1", "Usage", true},
		{"eks.aws.m.upbound.io/v1beta1", "ClusterAuth", true},
		{"eks.aws.m.upbound.io/v1beta1", "NodeGroup", true},
		{"eks.aws.m.upbound.io/v1beta1", "Addon", true},
		{"eks.aws.m.upbound.io/v1beta1", "AccessEntry", true},
		{"iam.aws.m.upbound.io/v1beta1", "Role", true},
		{"iam.aws.m.upbound.io/v1beta1", "RolePolicyAttachment", true},
		{"cloudwatchlogs.aws.m.upbound.io/v1beta1", "Group", true},
		{"kms.aws.m.upbound.io/v1beta1", "Alias", true},
		// AWS assigns the ID: a new object is a new resource.
		{"kms.aws.m.upbound.io/v1beta1", "Key", false},
		{"ec2.aws.m.upbound.io/v1beta1", "VPC", false},
		{"ec2.aws.m.upbound.io/v1beta1", "Subnet", false},
		{"ec2.aws.m.upbound.io/v1beta1", "NATGateway", false},
		{"ec2.aws.m.upbound.io/v1beta1", "EIP", false},
		{"ec2.aws.m.upbound.io/v1beta1", "RouteTable", false},
		{"ec2.aws.m.upbound.io/v1beta1", "SecurityGroup", false},
		{"ec2.aws.m.upbound.io/v1beta1", "LaunchTemplate", false},
		{"eks.aws.m.upbound.io/v1beta1", "PodIdentityAssociation", false},
		{"iam.aws.m.upbound.io/v1beta1", "OpenIDConnectProvider", false},
		// Nothing of a cluster is applied without its EKS cluster.
		{"eks.aws.m.upbound.io/v1beta1", "Cluster", false},
		// The AWS ProviderConfig is the account's, not a cluster's Helm one.
		{"aws.m.upbound.io/v1beta1", "ProviderConfig", false},
		{"helm.m.crossplane.io/v1beta1", "Release", false},
		{"not a version/at/all", "Role", false},
		{"", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.apiVersion+" "+tt.kind, func(t *testing.T) {
			if got := Recreatable(tt.apiVersion, tt.kind); got != tt.want {
				t.Errorf("Recreatable = %v, want %v", got, tt.want)
			}
		})
	}
}
