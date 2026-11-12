package aws

import (
	"context"
	"errors"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
)

const (
	clusterRoleARN = "arn:aws:iam::" + tenantAccount + ":role/svc/flow-logs" // the role fakeIAM has
	issuer         = "https://oidc.eks.us-east-1.amazonaws.com/id/ABCDEF"
)

// importEKS is an EKS cluster "tf" with two node groups, two add-ons, an
// access entry with a policy, a pod identity association and an OIDC
// identity provider config.
type importEKS struct{ missing bool }

func (f importEKS) DescribeCluster(context.Context, *eks.DescribeClusterInput, ...func(*eks.Options)) (*eks.DescribeClusterOutput, error) {
	if f.missing {
		return nil, &ekstypes.ResourceNotFoundException{Message: awssdk.String("No cluster found")}
	}
	return &eks.DescribeClusterOutput{Cluster: &ekstypes.Cluster{
		Name: awssdk.String("tf"), Version: awssdk.String("1.31"), Status: ekstypes.ClusterStatusActive, RoleArn: awssdk.String(clusterRoleARN),
		ResourcesVpcConfig: &ekstypes.VpcConfigResponse{SubnetIds: []string{"subnet-b", "subnet-a"}, SecurityGroupIds: []string{"sg-extra"},
			ClusterSecurityGroupId: awssdk.String("sg-eks"), EndpointPrivateAccess: true, EndpointPublicAccess: true,
			PublicAccessCidrs: []string{"0.0.0.0/0"}},
		Logging: &ekstypes.Logging{ClusterLogging: []ekstypes.LogSetup{
			{Enabled: awssdk.Bool(true), Types: []ekstypes.LogType{ekstypes.LogTypeAudit, ekstypes.LogTypeApi}},
			{Enabled: awssdk.Bool(false), Types: []ekstypes.LogType{ekstypes.LogTypeScheduler}}}},
		AccessConfig:            &ekstypes.AccessConfigResponse{AuthenticationMode: ekstypes.AuthenticationModeApiAndConfigMap},
		UpgradePolicy:           &ekstypes.UpgradePolicyResponse{SupportType: ekstypes.SupportTypeStandard},
		KubernetesNetworkConfig: &ekstypes.KubernetesNetworkConfigResponse{ServiceIpv4Cidr: awssdk.String("172.20.0.0/16"), IpFamily: ekstypes.IpFamilyIpv4},
		EncryptionConfig: []ekstypes.EncryptionConfig{{Resources: []string{"secrets"},
			Provider: &ekstypes.Provider{KeyArn: awssdk.String("arn:aws:kms:us-east-1:" + tenantAccount + ":key/k1")}}},
		Identity: &ekstypes.Identity{Oidc: &ekstypes.OIDC{Issuer: awssdk.String(issuer)}},
		Tags:     map[string]string{"terraform": "true"},
	}}, nil
}

func (importEKS) DescribeClusterVersions(context.Context, *eks.DescribeClusterVersionsInput, ...func(*eks.Options)) (*eks.DescribeClusterVersionsOutput, error) {
	return nil, errors.New("not used")
}

func (importEKS) DescribeAddonVersions(context.Context, *eks.DescribeAddonVersionsInput, ...func(*eks.Options)) (*eks.DescribeAddonVersionsOutput, error) {
	return nil, errors.New("not used")
}

func (importEKS) ListNodegroups(context.Context, *eks.ListNodegroupsInput, ...func(*eks.Options)) (*eks.ListNodegroupsOutput, error) {
	return &eks.ListNodegroupsOutput{Nodegroups: []string{"gpu", "default"}}, nil
}

func (importEKS) DescribeNodegroup(_ context.Context, in *eks.DescribeNodegroupInput, _ ...func(*eks.Options)) (*eks.DescribeNodegroupOutput, error) {
	g := &ekstypes.Nodegroup{NodegroupName: in.NodegroupName, Status: ekstypes.NodegroupStatusActive, Version: awssdk.String("1.31"),
		NodeRole: awssdk.String(clusterRoleARN), InstanceTypes: []string{"m6i.large"}, Subnets: []string{"subnet-a"},
		CapacityType: ekstypes.CapacityTypesOnDemand, AmiType: ekstypes.AMITypesAl2023X8664Standard, DiskSize: awssdk.Int32(50),
		ScalingConfig: &ekstypes.NodegroupScalingConfig{MinSize: awssdk.Int32(1), MaxSize: awssdk.Int32(4), DesiredSize: awssdk.Int32(2)},
		Taints:        []ekstypes.Taint{{Key: awssdk.String("k"), Value: awssdk.String("v"), Effect: ekstypes.TaintEffectNoSchedule}}}
	if awssdk.ToString(in.NodegroupName) == "gpu" {
		g.LaunchTemplate = &ekstypes.LaunchTemplateSpecification{Id: awssdk.String("lt-1"), Name: awssdk.String("gpu"), Version: awssdk.String("3")}
	}
	return &eks.DescribeNodegroupOutput{Nodegroup: g}, nil
}

func (importEKS) ListAddons(context.Context, *eks.ListAddonsInput, ...func(*eks.Options)) (*eks.ListAddonsOutput, error) {
	return &eks.ListAddonsOutput{Addons: []string{"vpc-cni", "coredns"}}, nil
}

func (importEKS) DescribeAddon(_ context.Context, in *eks.DescribeAddonInput, _ ...func(*eks.Options)) (*eks.DescribeAddonOutput, error) {
	return &eks.DescribeAddonOutput{Addon: &ekstypes.Addon{AddonName: in.AddonName, AddonVersion: awssdk.String("v1-" + *in.AddonName)}}, nil
}

func (importEKS) ListAccessEntries(context.Context, *eks.ListAccessEntriesInput, ...func(*eks.Options)) (*eks.ListAccessEntriesOutput, error) {
	return &eks.ListAccessEntriesOutput{AccessEntries: []string{"arn:aws:iam::" + tenantAccount + ":role/dev"}}, nil
}

func (importEKS) DescribeAccessEntry(_ context.Context, in *eks.DescribeAccessEntryInput, _ ...func(*eks.Options)) (*eks.DescribeAccessEntryOutput, error) {
	return &eks.DescribeAccessEntryOutput{AccessEntry: &ekstypes.AccessEntry{PrincipalArn: in.PrincipalArn, Type: awssdk.String("STANDARD"),
		KubernetesGroups: []string{"devs"}}}, nil
}

func (importEKS) ListAssociatedAccessPolicies(context.Context, *eks.ListAssociatedAccessPoliciesInput, ...func(*eks.Options)) (*eks.ListAssociatedAccessPoliciesOutput, error) {
	return &eks.ListAssociatedAccessPoliciesOutput{AssociatedAccessPolicies: []ekstypes.AssociatedAccessPolicy{{
		PolicyArn:   awssdk.String("arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy"),
		AccessScope: &ekstypes.AccessScope{Type: ekstypes.AccessScopeTypeNamespace, Namespaces: []string{"b", "a"}}}}}, nil
}

func (importEKS) ListPodIdentityAssociations(context.Context, *eks.ListPodIdentityAssociationsInput, ...func(*eks.Options)) (*eks.ListPodIdentityAssociationsOutput, error) {
	return &eks.ListPodIdentityAssociationsOutput{Associations: []ekstypes.PodIdentityAssociationSummary{
		{AssociationId: awssdk.String("a-1"), Namespace: awssdk.String("apps"), ServiceAccount: awssdk.String("api")}}}, nil
}

func (importEKS) ListIdentityProviderConfigs(context.Context, *eks.ListIdentityProviderConfigsInput, ...func(*eks.Options)) (*eks.ListIdentityProviderConfigsOutput, error) {
	return &eks.ListIdentityProviderConfigsOutput{IdentityProviderConfigs: []ekstypes.IdentityProviderConfig{
		{Name: awssdk.String("okta"), Type: awssdk.String("oidc")}}}, nil
}

func (importEKS) DescribeIdentityProviderConfig(context.Context, *eks.DescribeIdentityProviderConfigInput, ...func(*eks.Options)) (*eks.DescribeIdentityProviderConfigOutput, error) {
	return &eks.DescribeIdentityProviderConfigOutput{IdentityProviderConfig: &ekstypes.IdentityProviderConfigResponse{
		Oidc: &ekstypes.OidcIdentityProviderConfig{IdentityProviderConfigName: awssdk.String("okta"),
			IssuerUrl: awssdk.String("https://example.okta.com"), ClientId: awssdk.String("kubernetes"), GroupsClaim: awssdk.String("groups")}}}, nil
}

type importIAM struct{}

func (importIAM) ListAttachedRolePolicies(context.Context, *iam.ListAttachedRolePoliciesInput, ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error) {
	return &iam.ListAttachedRolePoliciesOutput{AttachedPolicies: []iamtypes.AttachedPolicy{
		{PolicyArn: awssdk.String("arn:aws:iam::aws:policy/B")}, {PolicyArn: awssdk.String("arn:aws:iam::aws:policy/A")}}}, nil
}

func (importIAM) ListOpenIDConnectProviders(context.Context, *iam.ListOpenIDConnectProvidersInput, ...func(*iam.Options)) (*iam.ListOpenIDConnectProvidersOutput, error) {
	return &iam.ListOpenIDConnectProvidersOutput{OpenIDConnectProviderList: []iamtypes.OpenIDConnectProviderListEntry{
		{Arn: awssdk.String("arn:aws:iam::" + tenantAccount + ":oidc-provider/oidc.eks.us-east-1.amazonaws.com/id/OTHER")},
		{Arn: awssdk.String("arn:aws:iam::" + tenantAccount + ":oidc-provider/oidc.eks.us-east-1.amazonaws.com/id/ABCDEF")}}}, nil
}

func importTenant(api importEKS) *Tenant {
	return &Tenant{AccountID: tenantAccount, Region: "us-east-1", eks: api, eksImport: api, iam: fakeIAM{}, iamImport: importIAM{}, logs: fakeLogs{}}
}

func TestDiscoverCluster(t *testing.T) {
	c, err := importTenant(importEKS{}).DiscoverCluster(t.Context(), "tf", "default")
	if err != nil {
		t.Fatalf("DiscoverCluster: %v", err)
	}
	if c.Version != "1.31" || c.Status != "ACTIVE" || c.SubnetIDs[0] != "subnet-a" || c.SecurityGroupIDs[0] != "sg-extra" ||
		!c.EndpointPrivateAccess || !c.EndpointPublicAccess || c.PublicAccessCIDRs[0] != "0.0.0.0/0" ||
		len(c.LogTypes) != 2 || c.LogTypes[0] != "api" || c.AuthenticationMode != "API_AND_CONFIG_MAP" || c.SupportType != "STANDARD" ||
		c.ServiceCIDR != "172.20.0.0/16" || c.IPFamily != "ipv4" || c.EncryptionKeyARN == "" || c.OIDCIssuer != issuer ||
		c.Tags["terraform"] != "true" {
		t.Errorf("cluster = %+v", c)
	}
	if c.Role.Name != "flow-logs" || c.Role.Unreadable != "" || len(c.Role.PolicyARNs) != 2 || c.Role.PolicyARNs[0] != "arn:aws:iam::aws:policy/A" ||
		c.NodeRole.Name != "flow-logs" {
		t.Errorf("roles = %+v / %+v, want them read with their policies, sorted", c.Role, c.NodeRole)
	}
	if len(c.NodeGroups) != 2 || c.NodeGroups[0].Name != "default" || c.NodeGroups[0].DiskSizeGB != 50 || c.NodeGroups[0].MaxSize != 4 ||
		c.NodeGroups[0].Taints[0].Effect != "NO_SCHEDULE" || c.NodeGroups[0].LaunchTemplateID != "" ||
		c.NodeGroups[1].LaunchTemplateID != "lt-1" || c.NodeGroups[1].LaunchTemplateVersion != "3" {
		t.Errorf("node groups = %+v", c.NodeGroups)
	}
	if len(c.Addons) != 2 || c.Addons[0].Name != "coredns" || c.Addons[0].Version != "v1-coredns" {
		t.Errorf("add-ons = %+v", c.Addons)
	}
	if e := c.AccessEntries; len(e) != 1 || e[0].Type != "STANDARD" || e[0].KubernetesGroups[0] != "devs" ||
		e[0].Policies[0].Scope != "namespace" || e[0].Policies[0].Namespaces[0] != "a" {
		t.Errorf("access entries = %+v", e)
	}
	if len(c.PodIdentityAssociations) != 1 || c.PodIdentityAssociations[0].ID != "a-1" ||
		len(c.IdentityProviderConfigs) != 1 || c.IdentityProviderConfigs[0].GroupsClaim != "groups" {
		t.Errorf("pod identities %+v, identity providers %+v", c.PodIdentityAssociations, c.IdentityProviderConfigs)
	}
	if c.OIDCProviderARN != "arn:aws:iam::"+tenantAccount+":oidc-provider/oidc.eks.us-east-1.amazonaws.com/id/ABCDEF" {
		t.Errorf("OIDC provider = %q, want the one of the cluster's issuer", c.OIDCProviderARN)
	}
	// fakeLogs has no /aws/eks/tf/cluster: no log group, not an error.
	if c.LogGroup != nil {
		t.Errorf("log group = %+v", c.LogGroup)
	}

	if _, err := importTenant(importEKS{missing: true}).DiscoverCluster(t.Context(), "tf", "default"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing cluster: %v, want ErrNotFound", err)
	}
	// A node group that is not the cluster's leaves the node role unread.
	c, err = importTenant(importEKS{}).DiscoverCluster(t.Context(), "tf", "nope")
	if err != nil || c.NodeRole.Name != "" {
		t.Errorf("unknown default node group: %v, node role %+v", err, c.NodeRole)
	}
}
