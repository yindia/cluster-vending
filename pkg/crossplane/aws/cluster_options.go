package aws

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	protectionv1beta1 "github.com/crossplane/crossplane/apis/v2/protection/v1beta1"
	awscwl "github.com/upbound/provider-aws/v2/apis/namespaced/cloudwatchlogs/v1beta1"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	awskms "github.com/upbound/provider-aws/v2/apis/namespaced/kms/v1beta1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// DefaultControlPlaneLogRetentionDays is the retention of a cluster's
	// control plane log group.
	DefaultControlPlaneLogRetentionDays = 90

	accessEntryStandard = "STANDARD"
	amiTypeCustom       = "CUSTOM"
	oidcClientID        = "sts.amazonaws.com"
	// eksOIDCThumbprint is the SHA-1 thumbprint of the root CA of EKS OIDC
	// issuers (Starfield Services Root Certificate Authority - G2). IAM
	// verifies EKS issuers against its own trusted CAs and ignores the
	// thumbprint, but the API still wants one.
	eksOIDCThumbprint = "9e99a48a9960b14926bb7f3b02e22da2b0ab7280"
)

// ControlPlaneLogGroupName is the CloudWatch Logs log group EKS writes a
// cluster's control plane logs to: the Group's external name.
func ControlPlaneLogGroupName(clusterName string) string {
	return "/aws/eks/" + clusterName + "/cluster"
}

// EncryptionAliasName is the alias of the KMS key a cluster's secrets are
// encrypted with: the Alias' external name.
func EncryptionAliasName(clusterName string) string { return "alias/" + clusterName + "-eks" }

// LaunchTemplateName is the rendered launch template of a node group: the
// object's name and the template's.
func LaunchTemplateName(nodeGroup string) string { return nodeGroup + "-lt" }

// KnownAfterApply stands, in a plan, for a value that only exists once
// another resource does (the encryption key's ARN, the cluster's issuer).
const KnownAfterApply = "(known after apply)"

// Waits are the kinds ClusterResources leaves out because something they
// need does not exist yet, each with what it waits for; the plan is
// rendered again later. The EKS cluster (with its node group and
// kubeconfig) waits for ClusterHeld and for the encryption key's ARN; the
// OIDC provider for the cluster's issuer; a rendered launch template with
// security groups (and its node group) for the cluster security group.
func (p ClusterPlan) Waits() map[string]string {
	waits := map[string]string{}
	var cluster []string
	if p.ClusterHeld {
		cluster = append(cluster, "the control plane's log group is ready")
	}
	if p.CreateEncryptionKey && p.EncryptionKeyARN == "" {
		cluster = append(cluster, "the encryption key exists")
	}
	if len(cluster) > 0 {
		for _, kind := range []string{awseks.Cluster_Kind, awseks.NodeGroup_Kind, awseks.ClusterAuth_Kind, awsec2.LaunchTemplate_Kind} {
			waits[kind] = strings.Join(cluster, " and ")
		}
	}
	if p.IRSA && p.OIDCIssuer == "" {
		waits[awsiam.OpenIDConnectProvider_Kind] = "the EKS cluster is ACTIVE (its OIDC issuer is known then)"
	}
	if lt := p.NodePool.LaunchTemplate; lt.Rendered() && len(lt.SecurityGroupIDs) > 0 && p.ClusterSecurityGroupID == "" {
		for _, kind := range []string{awsec2.LaunchTemplate_Kind, awseks.NodeGroup_Kind} {
			if waits[kind] == "" {
				waits[kind] = "the EKS cluster is ACTIVE (its security group is known then)"
			}
		}
	}
	return waits
}

// Pending are the kinds of Waits, sorted.
func (p ClusterPlan) Pending() []string {
	waits := p.Waits()
	kinds := make([]string, 0, len(waits))
	for kind := range waits {
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	return kinds
}

// AssumeLive fills what the plan waits for with KnownAfterApply, so that
// it renders in full: the cluster as it ends up, for a plan.
func (p *ClusterPlan) AssumeLive() {
	p.ClusterHeld = false
	if p.CreateEncryptionKey && p.EncryptionKeyARN == "" {
		p.EncryptionKeyARN = KnownAfterApply
	}
	if p.IRSA && p.OIDCIssuer == "" {
		p.OIDCIssuer = KnownAfterApply
	}
	if lt := p.NodePool.LaunchTemplate; lt.Rendered() && len(lt.SecurityGroupIDs) > 0 && p.ClusterSecurityGroupID == "" {
		p.ClusterSecurityGroupID = KnownAfterApply
	}
}

// withoutPending drops the objects of the plan's Pending kinds.
func (b clusterBuilder) withoutPending(objects []client.Object) []client.Object {
	pending := b.p.Pending()
	if len(pending) == 0 {
		return objects
	}
	out := make([]client.Object, 0, len(objects))
	for _, obj := range objects {
		if !slices.Contains(pending, kindName(obj)) {
			out = append(out, obj)
		}
	}
	return out
}

// kindName is the kind of a rendered object, from its Go type.
func kindName(obj client.Object) string {
	name := fmt.Sprintf("%T", obj)
	return name[strings.LastIndex(name, ".")+1:]
}

// isSubnetUsage reports whether a Usage keeps a network subnet: its
// object is the network's, not rendered here.
func isSubnetUsage(u *protectionv1beta1.Usage) bool { return u.Spec.Of.Kind == awsec2.Subnet_Kind }

// roleNamed is role with the IAM name externalName when it is not the
// object's.
func (b clusterBuilder) roleNamed(suffix, trust, externalName string) *awsiam.Role {
	r := b.role(suffix, trust)
	if externalName != "" && externalName != r.Name {
		r.Annotations = map[string]string{ExternalNameAnnotation: externalName}
	}
	return r
}

// beforeCluster are the objects that must exist before the EKS cluster:
// its control plane log group (or EKS creates one itself, unmanaged) and
// the KMS key of its secrets encryption with the key's alias.
func (b clusterBuilder) beforeCluster() []client.Object {
	var out []client.Object
	p := b.p
	if p.LogGroupRetentionDays > 0 {
		meta := b.meta(b.name("logs"))
		meta.Annotations = map[string]string{ExternalNameAnnotation: ControlPlaneLogGroupName(p.ClusterName)}
		out = append(out, &awscwl.Group{ObjectMeta: meta,
			Spec: awscwl.GroupSpec{ManagedResourceSpec: b.spec(), ForProvider: awscwl.GroupParameters{
				Region: ptr.To(p.Region), RetentionInDays: ptr.To(float64(p.LogGroupRetentionDays)), Tags: b.tags(b.name("logs")),
			}}})
	}
	if p.CreateEncryptionKey {
		alias := b.meta(b.name("secrets-alias"))
		alias.Annotations = map[string]string{ExternalNameAnnotation: EncryptionAliasName(p.ClusterName)}
		out = append(out,
			&awskms.Key{ObjectMeta: b.meta(b.name("secrets-key")),
				Spec: awskms.KeySpec{ManagedResourceSpec: b.spec(), ForProvider: awskms.KeyParameters{
					Region: ptr.To(p.Region), Description: ptr.To("Secrets encryption of EKS cluster " + p.ClusterName),
					EnableKeyRotation: ptr.To(true), Tags: b.tags(b.name("secrets-key")),
				}}},
			&awskms.Alias{ObjectMeta: alias,
				Spec: awskms.AliasSpec{ManagedResourceSpec: b.spec(), ForProvider: awskms.AliasParameters{
					Region: ptr.To(p.Region), TargetKeyIDRef: b.ref(b.name("secrets-key")),
				}}})
	}
	return out
}

// launchTemplate is the node group's rendered launch template, nil when it
// has none: the node security groups (the cluster's own first, EKS only
// adds it by itself to nodes without a template's groups) and / or a
// custom AMI with its user data.
func (b clusterBuilder) launchTemplate() *awsec2.LaunchTemplate {
	lt := b.p.NodePool.LaunchTemplate
	if !lt.Rendered() {
		return nil
	}
	name := LaunchTemplateName(b.nodeGroupName())
	params := awsec2.LaunchTemplateParameters_2{
		Region: ptr.To(b.p.Region), Name: ptr.To(name),
		Description: ptr.To("Nodes of EKS cluster " + b.p.ClusterName), Tags: b.tags(name),
		ImageID: optional(lt.ImageID), UserData: optional(lt.UserData),
	}
	if len(lt.SecurityGroupIDs) > 0 {
		params.VPCSecurityGroupIds = ptrs(append([]string{b.p.ClusterSecurityGroupID}, lt.SecurityGroupIDs...))
	}
	return &awsec2.LaunchTemplate{ObjectMeta: b.meta(name),
		Spec: awsec2.LaunchTemplateSpec{ManagedResourceSpec: b.spec(), ForProvider: params}}
}

// accessEntries renders the plan's access entries: an AccessEntry
// "<prefix>-access-<hash8 of principal and type>" each, and an
// AccessPolicyAssociation "<entry>-<hash8 of policy and scope>" per
// policy. EKS cannot change an entry's type or an association's scope, so
// both are part of the name: a change renders a new object and the old one
// is pruned.
func (b clusterBuilder) accessEntries() []client.Object {
	var out []client.Object
	for _, e := range b.p.AccessEntries {
		entryType := e.Type
		if entryType == "" {
			entryType = accessEntryStandard
		}
		suffix := "access-" + shortHash(e.Principal, entryType)
		out = append(out, b.accessEntryOf(suffix, e.Principal, entryType))
		for _, policy := range e.Policies {
			out = append(out, b.accessPolicyOf(suffix, e.Principal, policy))
		}
	}
	return out
}

func (b clusterBuilder) accessPolicyOf(entry, principal string, policy AccessPolicyPlan) *awseks.AccessPolicyAssociation {
	scope := &awseks.AccessScopeParameters{Type: ptr.To(policy.Scope)}
	if policy.Scope == "" {
		scope.Type = ptr.To("cluster")
	}
	namespaces := slices.Clone(policy.Namespaces)
	slices.Sort(namespaces)
	scope.Namespaces = ptrs(namespaces)
	name := b.name(entry + "-" + shortHash(policy.PolicyARN, *scope.Type, strings.Join(namespaces, ",")))
	return &awseks.AccessPolicyAssociation{
		ObjectMeta: b.meta(name),
		Spec: awseks.AccessPolicyAssociationSpec{ManagedResourceSpec: b.spec(), ForProvider: awseks.AccessPolicyAssociationParameters{
			Region: ptr.To(b.p.Region), ClusterNameRef: b.ref(b.p.Prefix), PrincipalArn: ptr.To(principal),
			PolicyArn: ptr.To(policy.PolicyARN), AccessScope: scope,
		}},
	}
}

// managedIdentityProvider is the OIDC identity provider of the Kubernetes
// API the plan asks for: an IdentityProviderConfig
// "<prefix>-idp-<hash8 of all its settings>" whose external name is the
// association's name. EKS cannot change an association, so every setting
// is part of the object's name: a change renders a new object and the old
// one is pruned. EKS takes one association per cluster, so the new one
// only exists once the old one is gone: until then the new object fails
// to create or, when the name is unchanged, observes the old association.
func (b clusterBuilder) managedIdentityProvider(idp IdentityProviderPlan) *awseks.IdentityProviderConfig {
	claims := make([]string, 0, len(idp.RequiredClaims))
	for k, v := range idp.RequiredClaims {
		claims = append(claims, k+"="+v)
	}
	slices.Sort(claims)
	name := b.name("idp-" + shortHash(idp.Name, idp.IssuerURL, idp.ClientID, idp.UsernameClaim, idp.UsernamePrefix,
		idp.GroupsClaim, idp.GroupsPrefix, strings.Join(claims, ",")))
	meta := b.meta(name)
	meta.Annotations = map[string]string{ExternalNameAnnotation: idp.Name}
	return &awseks.IdentityProviderConfig{ObjectMeta: meta,
		Spec: awseks.IdentityProviderConfigSpec{ManagedResourceSpec: b.spec(), ForProvider: awseks.IdentityProviderConfigParameters{
			Region: ptr.To(b.p.Region), ClusterNameRef: b.ref(b.p.Prefix), Oidc: idp.oidc(), Tags: b.tags(name),
		}}}
}

// oidc is the plan as the provider's parameters; the association's name is
// the object's external name.
func (idp IdentityProviderPlan) oidc() *awseks.IdentityProviderConfigOidcParameters {
	return &awseks.IdentityProviderConfigOidcParameters{
		ClientID: ptr.To(idp.ClientID), IssuerURL: ptr.To(idp.IssuerURL),
		GroupsClaim: optional(idp.GroupsClaim), GroupsPrefix: optional(idp.GroupsPrefix),
		UsernameClaim: optional(idp.UsernameClaim), UsernamePrefix: optional(idp.UsernamePrefix),
		RequiredClaims: strPtrs(idp.RequiredClaims),
	}
}

// shortHash is the first 8 hex characters of the parts' sha256.
func shortHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:4])
}

// oidcProvider is the IAM OIDC provider of the cluster's issuer, for IAM
// roles for service accounts.
func (b clusterBuilder) oidcProvider() *awsiam.OpenIDConnectProvider {
	name := b.name("oidc")
	return &awsiam.OpenIDConnectProvider{ObjectMeta: b.meta(name),
		Spec: awsiam.OpenIDConnectProviderSpec{ManagedResourceSpec: b.spec(), ForProvider: awsiam.OpenIDConnectProviderParameters{
			URL: ptr.To(b.p.OIDCIssuer), ClientIDList: ptrs([]string{oidcClientID}),
			ThumbprintList: ptrs([]string{eksOIDCThumbprint}), Tags: b.tags(name),
		}}}
}

// optionUsagePairs are the deletion dependencies of the optional objects:
// the log group, the encryption key and the OIDC provider outlive the EKS
// cluster; a rendered launch template outlives its node group.
func (b clusterBuilder) optionUsagePairs(eks, nodeGroup UsageRef) []UsagePair {
	var pairs []UsagePair
	p := b.p
	if p.LogGroupRetentionDays > 0 {
		pairs = append(pairs, UsagePair{Of: logGroupKind.named(b.name("logs")), By: eks})
	}
	if p.CreateEncryptionKey {
		pairs = append(pairs, UsagePair{Of: kmsKeyKind.named(b.name("secrets-key")), By: eks})
	}
	if p.IRSA {
		pairs = append(pairs, UsagePair{Of: oidcKind.named(b.name("oidc")), By: eks})
	}
	if p.NodePool.LaunchTemplate.Rendered() {
		pairs = append(pairs, UsagePair{Of: launchTmplKind.named(LaunchTemplateName(nodeGroup.Name)), By: nodeGroup})
	}
	return pairs
}
