package aws

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
)

// EKSImportAPI is the part of the EKS client cluster discovery uses beyond
// EKSAPI: read-only calls only.
type EKSImportAPI interface {
	ListNodegroups(context.Context, *eks.ListNodegroupsInput, ...func(*eks.Options)) (*eks.ListNodegroupsOutput, error)
	DescribeNodegroup(context.Context, *eks.DescribeNodegroupInput, ...func(*eks.Options)) (*eks.DescribeNodegroupOutput, error)
	ListAddons(context.Context, *eks.ListAddonsInput, ...func(*eks.Options)) (*eks.ListAddonsOutput, error)
	DescribeAddon(context.Context, *eks.DescribeAddonInput, ...func(*eks.Options)) (*eks.DescribeAddonOutput, error)
	ListAccessEntries(context.Context, *eks.ListAccessEntriesInput, ...func(*eks.Options)) (*eks.ListAccessEntriesOutput, error)
	DescribeAccessEntry(context.Context, *eks.DescribeAccessEntryInput, ...func(*eks.Options)) (*eks.DescribeAccessEntryOutput, error)
	ListAssociatedAccessPolicies(context.Context, *eks.ListAssociatedAccessPoliciesInput, ...func(*eks.Options)) (*eks.ListAssociatedAccessPoliciesOutput, error)
	ListPodIdentityAssociations(context.Context, *eks.ListPodIdentityAssociationsInput, ...func(*eks.Options)) (*eks.ListPodIdentityAssociationsOutput, error)
	ListIdentityProviderConfigs(context.Context, *eks.ListIdentityProviderConfigsInput, ...func(*eks.Options)) (*eks.ListIdentityProviderConfigsOutput, error)
	DescribeIdentityProviderConfig(context.Context, *eks.DescribeIdentityProviderConfigInput, ...func(*eks.Options)) (*eks.DescribeIdentityProviderConfigOutput, error)
}

// IAMImportAPI is the part of the IAM client cluster discovery uses beyond
// IAMAPI: read-only calls only.
type IAMImportAPI interface {
	ListAttachedRolePolicies(context.Context, *iam.ListAttachedRolePoliciesInput, ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error)
	ListOpenIDConnectProviders(context.Context, *iam.ListOpenIDConnectProvidersInput, ...func(*iam.Options)) (*iam.ListOpenIDConnectProvidersOutput, error)
}

// Cluster is what an existing EKS cluster is made of: the input of an
// import plan. Lists are ordered by name or ID.
type Cluster struct {
	Name, ARN, Version, Status string
	// SubnetIDs are the control plane's subnets; SecurityGroupIDs the
	// additional security groups (not the one EKS created).
	SubnetIDs, SecurityGroupIDs                 []string
	EndpointPrivateAccess, EndpointPublicAccess bool
	PublicAccessCIDRs                           []string
	// LogTypes are the enabled control plane log types.
	LogTypes                                               []string
	AuthenticationMode, SupportType, ServiceCIDR, IPFamily string
	// EncryptionKeyARN is the KMS key of the secrets encryption, if on.
	EncryptionKeyARN string
	OIDCIssuer       string
	Tags             map[string]string

	// Role is the cluster's IAM role.
	Role ClusterRole
	// NodeGroups are all the cluster's node groups.
	NodeGroups []NodeGroup
	// NodeRole is the IAM role of the default node group (the one the
	// caller named); zero when that node group does not exist.
	NodeRole                ClusterRole
	Addons                  []Addon
	AccessEntries           []AccessEntry
	PodIdentityAssociations []PodIdentityAssociation
	IdentityProviderConfigs []IdentityProviderConfig
	// OIDCProviderARN is the IAM OIDC provider of the cluster's issuer;
	// empty when the account has none.
	OIDCProviderARN string
	// LogGroup is the control plane's log group, nil when there is none.
	LogGroup *LogGroup
}

// ClusterRole is an IAM role with the managed policies attached to it.
// Unreadable says why it could not be read.
type ClusterRole struct {
	Role
	PolicyARNs []string
	Unreadable string
}

// NodeGroup is an EKS managed node group.
type NodeGroup struct {
	Name, Status, Version, NodeRoleARN string
	InstanceTypes, SubnetIDs           []string
	CapacityType, AMIType              string
	DiskSizeGB                         int
	MinSize, MaxSize, DesiredSize      int
	Labels                             map[string]string
	Taints                             []NodeTaint
	// LaunchTemplateID, Name and Version are set for a node group with a
	// launch template.
	LaunchTemplateID, LaunchTemplateName, LaunchTemplateVersion string
	Tags                                                        map[string]string
}

// NodeTaint is a node group taint; Effect is the EKS spelling.
type NodeTaint struct{ Key, Value, Effect string }

// Addon is an EKS add-on.
type Addon struct {
	Name, Version, ConfigurationValues, ServiceAccountRoleARN string
	Tags                                                      map[string]string
}

// AccessEntry is an EKS access entry with its associated policies.
type AccessEntry struct {
	PrincipalARN, Type, Username string
	KubernetesGroups             []string
	Policies                     []AccessPolicy
	Tags                         map[string]string
}

// AccessPolicy is an access policy associated with an entry; Scope is
// "cluster" or "namespace".
type AccessPolicy struct {
	PolicyARN, Scope string
	Namespaces       []string
}

// PodIdentityAssociation is an EKS Pod Identity association.
type PodIdentityAssociation struct{ ID, Namespace, ServiceAccount string }

// IdentityProviderConfig is an OIDC identity provider of the cluster.
type IdentityProviderConfig struct {
	Name, IssuerURL, ClientID                                string
	GroupsClaim, GroupsPrefix, UsernameClaim, UsernamePrefix string
	RequiredClaims                                           map[string]string
	Tags                                                     map[string]string
}

// ClusterDiscoverer looks an existing cluster up (ImportCluster).
type ClusterDiscoverer interface {
	DiscoverCluster(ctx context.Context, access Access, region, name, defaultNodeGroup string) (*Cluster, error)
}

var _ ClusterDiscoverer = (*Client)(nil)

// DiscoverCluster is TenantFor(access, region).DiscoverCluster, after
// VerifyAccess (see DiscoverNetwork).
func (c *Client) DiscoverCluster(ctx context.Context, access Access, region, name, defaultNodeGroup string) (*Cluster, error) {
	t, err := c.TenantFor(access, region)
	if err != nil {
		return nil, err
	}
	if err := t.VerifyAccess(ctx); err != nil {
		return nil, err
	}
	return t.DiscoverCluster(ctx, name, defaultNodeGroup)
}

// DiscoverCluster reads an EKS cluster and what it is made of. A cluster
// that does not exist is ErrNotFound. IAM roles, the OIDC provider and the
// log group that cannot be read do not fail discovery: the plan says so.
//
// Calls: eks:DescribeCluster, ListNodegroups, DescribeNodegroup,
// ListAddons, DescribeAddon, ListAccessEntries, DescribeAccessEntry,
// ListAssociatedAccessPolicies, ListPodIdentityAssociations,
// ListIdentityProviderConfigs, DescribeIdentityProviderConfig; iam:GetRole,
// ListAttachedRolePolicies, ListOpenIDConnectProviders;
// logs:DescribeLogGroups, ListTagsForResource.
func (t *Tenant) DiscoverCluster(ctx context.Context, name, defaultNodeGroup string) (*Cluster, error) {
	out, err := t.eks.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: awssdk.String(name)})
	if err != nil {
		return nil, translateEKS(err, "describe EKS cluster "+name)
	}
	c := eksCluster(out.Cluster)
	c.Role = t.clusterRole(ctx, awssdk.ToString(out.Cluster.RoleArn))
	steps := []func(context.Context, *Cluster) error{
		t.nodeGroups, t.addons, t.accessEntries, t.podIdentityAssociations, t.identityProviderConfigs,
	}
	for _, step := range steps {
		if err := step(ctx, c); err != nil {
			return nil, err
		}
	}
	for _, g := range c.NodeGroups {
		if g.Name == defaultNodeGroup {
			c.NodeRole = t.clusterRole(ctx, g.NodeRoleARN)
		}
	}
	c.OIDCProviderARN = t.oidcProvider(ctx, c.OIDCIssuer)
	if group, err := t.logGroup(ctx, "/aws/eks/"+name+"/cluster"); err == nil {
		c.LogGroup = &group
	}
	return c, nil
}

// translateEKS is translate with EKS's not-found error.
func translateEKS(err error, what string) error {
	var notFound *ekstypes.ResourceNotFoundException
	if asError(err, &notFound) {
		return fmt.Errorf("%w: %s", ErrNotFound, what)
	}
	return translate(err, what)
}

func eksCluster(c *ekstypes.Cluster) *Cluster {
	out := &Cluster{Name: awssdk.ToString(c.Name), ARN: awssdk.ToString(c.Arn), Version: awssdk.ToString(c.Version),
		Status: string(c.Status), Tags: nonEmpty(c.Tags)}
	if v := c.ResourcesVpcConfig; v != nil {
		out.SubnetIDs, out.SecurityGroupIDs = sorted(v.SubnetIds), sorted(v.SecurityGroupIds)
		out.EndpointPrivateAccess, out.EndpointPublicAccess = v.EndpointPrivateAccess, v.EndpointPublicAccess
		out.PublicAccessCIDRs = sorted(v.PublicAccessCidrs)
	}
	if c.Logging != nil {
		for _, setup := range c.Logging.ClusterLogging {
			if awssdk.ToBool(setup.Enabled) {
				for _, t := range setup.Types {
					out.LogTypes = append(out.LogTypes, string(t))
				}
			}
		}
		sort.Strings(out.LogTypes)
	}
	if c.AccessConfig != nil {
		out.AuthenticationMode = string(c.AccessConfig.AuthenticationMode)
	}
	if c.UpgradePolicy != nil {
		out.SupportType = string(c.UpgradePolicy.SupportType)
	}
	if n := c.KubernetesNetworkConfig; n != nil {
		out.ServiceCIDR, out.IPFamily = awssdk.ToString(n.ServiceIpv4Cidr), string(n.IpFamily)
	}
	for _, e := range c.EncryptionConfig {
		if e.Provider != nil {
			out.EncryptionKeyARN = awssdk.ToString(e.Provider.KeyArn)
		}
	}
	if c.Identity != nil && c.Identity.Oidc != nil {
		out.OIDCIssuer = awssdk.ToString(c.Identity.Oidc.Issuer)
	}
	return out
}

func sorted(in []string) []string {
	out := slices.Clone(in)
	sort.Strings(out)
	return out
}

// clusterRole reads an IAM role and its attached managed policies.
func (t *Tenant) clusterRole(ctx context.Context, arn string) ClusterRole {
	role, err := t.role(ctx, arn)
	if err != nil {
		return ClusterRole{Role: Role{ARN: arn}, Unreadable: err.Error()}
	}
	out := ClusterRole{Role: role}
	p := iam.NewListAttachedRolePoliciesPaginator(t.iamImport, &iam.ListAttachedRolePoliciesInput{RoleName: awssdk.String(role.Name)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			out.Unreadable = translate(err, "list the policies of role "+role.Name).Error()
			return out
		}
		for _, a := range page.AttachedPolicies {
			out.PolicyARNs = append(out.PolicyARNs, awssdk.ToString(a.PolicyArn))
		}
	}
	sort.Strings(out.PolicyARNs)
	return out
}

func (t *Tenant) nodeGroups(ctx context.Context, c *Cluster) error {
	p := eks.NewListNodegroupsPaginator(t.eksImport, &eks.ListNodegroupsInput{ClusterName: awssdk.String(c.Name)})
	var names []string
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return translateEKS(err, "list node groups")
		}
		names = append(names, page.Nodegroups...)
	}
	sort.Strings(names)
	for _, name := range names {
		out, err := t.eksImport.DescribeNodegroup(ctx, &eks.DescribeNodegroupInput{ClusterName: awssdk.String(c.Name), NodegroupName: awssdk.String(name)})
		if err != nil {
			return translateEKS(err, "describe node group "+name)
		}
		c.NodeGroups = append(c.NodeGroups, nodeGroup(out.Nodegroup))
	}
	return nil
}

func nodeGroup(g *ekstypes.Nodegroup) NodeGroup {
	out := NodeGroup{Name: awssdk.ToString(g.NodegroupName), Status: string(g.Status), Version: awssdk.ToString(g.Version),
		NodeRoleARN: awssdk.ToString(g.NodeRole), InstanceTypes: sorted(g.InstanceTypes), SubnetIDs: sorted(g.Subnets),
		CapacityType: string(g.CapacityType), AMIType: string(g.AmiType), DiskSizeGB: int(awssdk.ToInt32(g.DiskSize)),
		Labels: nonEmpty(g.Labels), Tags: nonEmpty(g.Tags)}
	if s := g.ScalingConfig; s != nil {
		out.MinSize, out.MaxSize, out.DesiredSize = int(awssdk.ToInt32(s.MinSize)), int(awssdk.ToInt32(s.MaxSize)), int(awssdk.ToInt32(s.DesiredSize))
	}
	for _, taint := range g.Taints {
		out.Taints = append(out.Taints, NodeTaint{Key: awssdk.ToString(taint.Key), Value: awssdk.ToString(taint.Value), Effect: string(taint.Effect)})
	}
	if lt := g.LaunchTemplate; lt != nil {
		out.LaunchTemplateID, out.LaunchTemplateName, out.LaunchTemplateVersion =
			awssdk.ToString(lt.Id), awssdk.ToString(lt.Name), awssdk.ToString(lt.Version)
	}
	return out
}

func (t *Tenant) addons(ctx context.Context, c *Cluster) error {
	p := eks.NewListAddonsPaginator(t.eksImport, &eks.ListAddonsInput{ClusterName: awssdk.String(c.Name)})
	var names []string
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return translateEKS(err, "list add-ons")
		}
		names = append(names, page.Addons...)
	}
	sort.Strings(names)
	for _, name := range names {
		out, err := t.eksImport.DescribeAddon(ctx, &eks.DescribeAddonInput{ClusterName: awssdk.String(c.Name), AddonName: awssdk.String(name)})
		if err != nil {
			return translateEKS(err, "describe add-on "+name)
		}
		a := out.Addon
		c.Addons = append(c.Addons, Addon{Name: name, Version: awssdk.ToString(a.AddonVersion),
			ConfigurationValues: awssdk.ToString(a.ConfigurationValues), ServiceAccountRoleARN: awssdk.ToString(a.ServiceAccountRoleArn),
			Tags: nonEmpty(a.Tags)})
	}
	return nil
}

// accessEntries reads the cluster's access entries with their policies. A
// cluster in CONFIG_MAP mode has none (EKS refuses the call): not an
// error.
func (t *Tenant) accessEntries(ctx context.Context, c *Cluster) error {
	if c.AuthenticationMode == string(ekstypes.AuthenticationModeConfigMap) {
		return nil
	}
	p := eks.NewListAccessEntriesPaginator(t.eksImport, &eks.ListAccessEntriesInput{ClusterName: awssdk.String(c.Name)})
	var principals []string
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return translateEKS(err, "list access entries")
		}
		principals = append(principals, page.AccessEntries...)
	}
	sort.Strings(principals)
	for _, principal := range principals {
		out, err := t.eksImport.DescribeAccessEntry(ctx, &eks.DescribeAccessEntryInput{ClusterName: awssdk.String(c.Name), PrincipalArn: awssdk.String(principal)})
		if err != nil {
			return translateEKS(err, "describe access entry "+principal)
		}
		e := out.AccessEntry
		entry := AccessEntry{PrincipalARN: principal, Type: awssdk.ToString(e.Type), Username: awssdk.ToString(e.Username),
			KubernetesGroups: sorted(e.KubernetesGroups), Tags: nonEmpty(e.Tags)}
		pp := eks.NewListAssociatedAccessPoliciesPaginator(t.eksImport,
			&eks.ListAssociatedAccessPoliciesInput{ClusterName: awssdk.String(c.Name), PrincipalArn: awssdk.String(principal)})
		for pp.HasMorePages() {
			page, err := pp.NextPage(ctx)
			if err != nil {
				return translateEKS(err, "list access policies of "+principal)
			}
			for _, a := range page.AssociatedAccessPolicies {
				policy := AccessPolicy{PolicyARN: awssdk.ToString(a.PolicyArn)}
				if a.AccessScope != nil {
					policy.Scope, policy.Namespaces = string(a.AccessScope.Type), sorted(a.AccessScope.Namespaces)
				}
				entry.Policies = append(entry.Policies, policy)
			}
		}
		sort.Slice(entry.Policies, func(i, j int) bool { return entry.Policies[i].PolicyARN < entry.Policies[j].PolicyARN })
		c.AccessEntries = append(c.AccessEntries, entry)
	}
	return nil
}

func (t *Tenant) podIdentityAssociations(ctx context.Context, c *Cluster) error {
	p := eks.NewListPodIdentityAssociationsPaginator(t.eksImport, &eks.ListPodIdentityAssociationsInput{ClusterName: awssdk.String(c.Name)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return translateEKS(err, "list pod identity associations")
		}
		for _, a := range page.Associations {
			c.PodIdentityAssociations = append(c.PodIdentityAssociations, PodIdentityAssociation{ID: awssdk.ToString(a.AssociationId),
				Namespace: awssdk.ToString(a.Namespace), ServiceAccount: awssdk.ToString(a.ServiceAccount)})
		}
	}
	sort.Slice(c.PodIdentityAssociations, func(i, j int) bool { return c.PodIdentityAssociations[i].ID < c.PodIdentityAssociations[j].ID })
	return nil
}

func (t *Tenant) identityProviderConfigs(ctx context.Context, c *Cluster) error {
	p := eks.NewListIdentityProviderConfigsPaginator(t.eksImport, &eks.ListIdentityProviderConfigsInput{ClusterName: awssdk.String(c.Name)})
	var configs []ekstypes.IdentityProviderConfig
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return translateEKS(err, "list identity provider configs")
		}
		configs = append(configs, page.IdentityProviderConfigs...)
	}
	for _, cfg := range configs {
		out, err := t.eksImport.DescribeIdentityProviderConfig(ctx, &eks.DescribeIdentityProviderConfigInput{
			ClusterName: awssdk.String(c.Name), IdentityProviderConfig: &cfg})
		if err != nil {
			return translateEKS(err, "describe identity provider config "+awssdk.ToString(cfg.Name))
		}
		if out.IdentityProviderConfig == nil || out.IdentityProviderConfig.Oidc == nil {
			continue
		}
		o := out.IdentityProviderConfig.Oidc
		c.IdentityProviderConfigs = append(c.IdentityProviderConfigs, IdentityProviderConfig{
			Name: awssdk.ToString(o.IdentityProviderConfigName), IssuerURL: awssdk.ToString(o.IssuerUrl), ClientID: awssdk.ToString(o.ClientId),
			GroupsClaim: awssdk.ToString(o.GroupsClaim), GroupsPrefix: awssdk.ToString(o.GroupsPrefix),
			UsernameClaim: awssdk.ToString(o.UsernameClaim), UsernamePrefix: awssdk.ToString(o.UsernamePrefix),
			RequiredClaims: nonEmpty(o.RequiredClaims), Tags: nonEmpty(o.Tags)})
	}
	sort.Slice(c.IdentityProviderConfigs, func(i, j int) bool { return c.IdentityProviderConfigs[i].Name < c.IdentityProviderConfigs[j].Name })
	return nil
}

// oidcProvider is the ARN of the account's IAM OIDC provider for issuer
// ("https://oidc.eks.<region>.amazonaws.com/id/<id>": the provider's ARN
// ends with the issuer without its scheme); empty when there is none or
// the providers cannot be listed.
func (t *Tenant) oidcProvider(ctx context.Context, issuer string) string {
	host := strings.TrimPrefix(issuer, "https://")
	if host == "" {
		return ""
	}
	out, err := t.iamImport.ListOpenIDConnectProviders(ctx, &iam.ListOpenIDConnectProvidersInput{})
	if err != nil {
		return ""
	}
	for _, p := range out.OpenIDConnectProviderList {
		if arn := awssdk.ToString(p.Arn); strings.HasSuffix(arn, ":oidc-provider/"+host) {
			return arn
		}
	}
	return ""
}
