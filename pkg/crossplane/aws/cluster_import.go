package aws

import (
	"slices"
	"strings"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	awscwl "github.com/upbound/provider-aws/v2/apis/namespaced/cloudwatchlogs/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	awskms "github.com/upbound/provider-aws/v2/apis/namespaced/kms/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
)

// PolicyObserveLateInit reads the cloud resource and fills the fields the
// object leaves unset from it; nothing in the cloud changes (a cluster
// import).
var PolicyObserveLateInit = xpv2.ManagementPolicies{xpv2.ManagementActionObserve, xpv2.ManagementActionLateInitialize}

// ClusterIdentities are the cloud identities of an existing cluster's
// resources, by which the objects ClusterResources renders adopt them.
type ClusterIdentities struct {
	// ClusterName is the EKS cluster's name; NodeGroupName its default
	// node group's.
	ClusterName, NodeGroupName string
	// RoleNames maps the role objects ("<prefix>-cluster-role",
	// "<prefix>-node-role") to their IAM role names.
	RoleNames map[string]string
	// AttachedPolicies are the managed policies attached to each IAM role,
	// by role name.
	AttachedPolicies map[string][]string
	// Addons are the names of the add-ons the cluster has.
	Addons []string
	// AccessEntries are the policies associated with each principal that
	// has an access entry.
	AccessEntries map[string][]AccessPolicyPlan
	// PodIdentities maps "<namespace>/<service account>" to the pod
	// identity association's ID.
	PodIdentities map[string]string
}

// managed is what Adopt changes on a managed resource.
type managed interface {
	client.Object
	SetManagementPolicies(xpv2.ManagementPolicies)
}

// Adopt turns an object ClusterResources rendered into the import of the
// cloud resource it describes, when ids has one: it only observes
// (PolicyObserveLateInit). It returns the resource's external name, in the
// provider's format for the kind, and whether the cloud resource exists;
// an object without one (ClusterAuth, Usages, what the platform adds) is
// left alone and created at the takeover. The caller keeps the external
// name (Adoption.ExternalNames) so every later render carries it.
func Adopt(obj client.Object, ids ClusterIdentities) (string, bool) {
	name, exists := externalName(obj, ids)
	m, ok := obj.(managed)
	if !exists || !ok {
		return "", false
	}
	m.SetManagementPolicies(PolicyObserveLateInit)
	return name, true
}

// externalName is the external name of the cloud resource obj describes,
// as provider-upjet-aws v2.9.0 builds it (config/externalname.go):
//
//	Role                     the role name                    (NameAsIdentifier)
//	RolePolicyAttachment     <role name>/<policy ARN>         (IdentifierFromProvider)
//	Cluster                  the cluster name                 (NameAsIdentifier)
//	NodeGroup                the node group name              ({{cluster}}:{{external_name}})
//	Addon                    <cluster>:<add-on>               (FormattedIdentifierFromProvider)
//	AccessEntry              <cluster>:<principal ARN>
//	AccessPolicyAssociation  <cluster>#<principal ARN>#<policy ARN>
//	PodIdentityAssociation   the association ID               (IdentifierFromProvider)
func externalName(obj client.Object, ids ClusterIdentities) (string, bool) {
	switch o := obj.(type) {
	case *awsiam.Role:
		name, ok := ids.RoleNames[o.Name]
		return name, ok
	case *awsiam.RolePolicyAttachment:
		role, policy := "", ptr.Deref(o.Spec.ForProvider.PolicyArn, "")
		if ref := o.Spec.ForProvider.RoleRef; ref != nil {
			role = ids.RoleNames[ref.Name]
		}
		return role + "/" + policy, role != "" && policy != "" && slices.Contains(ids.AttachedPolicies[role], policy)
	case *awseks.Cluster:
		return ids.ClusterName, true
	case *awseks.NodeGroup:
		return ids.NodeGroupName, ids.NodeGroupName != ""
	case *awseks.Addon:
		addon := ptr.Deref(o.Spec.ForProvider.AddonName, "")
		return ids.ClusterName + ":" + addon, slices.Contains(ids.Addons, addon)
	case *awseks.AccessEntry:
		principal := ptr.Deref(o.Spec.ForProvider.PrincipalArn, "")
		_, ok := ids.AccessEntries[principal]
		return ids.ClusterName + ":" + principal, ok
	case *awseks.AccessPolicyAssociation:
		fp := o.Spec.ForProvider
		principal, policy := ptr.Deref(fp.PrincipalArn, ""), ptr.Deref(fp.PolicyArn, "")
		return ids.ClusterName + "#" + principal + "#" + policy, slices.ContainsFunc(ids.AccessEntries[principal], func(p AccessPolicyPlan) bool {
			return p.PolicyARN == policy && sameScope(p, fp.AccessScope)
		})
	case *awseks.PodIdentityAssociation:
		id, ok := ids.PodIdentities[ptr.Deref(o.Spec.ForProvider.Namespace, "")+"/"+ptr.Deref(o.Spec.ForProvider.ServiceAccount, "")]
		return id, ok
	default:
		return "", false
	}
}

// sameScope reports whether an associated policy has the scope an
// AccessPolicyAssociation renders (EKS cannot change it in place).
func sameScope(p AccessPolicyPlan, scope *awseks.AccessScopeParameters) bool {
	if scope == nil {
		return false
	}
	want := make([]string, 0, len(scope.Namespaces))
	for _, n := range scope.Namespaces {
		want = append(want, ptr.Deref(n, ""))
	}
	have := slices.Clone(p.Namespaces)
	slices.Sort(have)
	slices.Sort(want)
	return p.Scope == ptr.Deref(scope.Type, "") && slices.Equal(have, want)
}

// CarriedOverResources are the resources of an existing cluster that no
// spec field describes: imported as they are and owned with the cluster.
type CarriedOverResources struct {
	// ExtraPolicies are the managed policies attached to the cluster and
	// node roles beyond the ones the platform attaches, by role object
	// suffix ("cluster-role", "node-role"); RoleNames their IAM role names.
	ExtraPolicies map[string][]string
	RoleNames     map[string]string
	// OIDCProviderARN is the IAM OIDC provider of the cluster's issuer.
	OIDCProviderARN, OIDCIssuer string
	// LogGroupName is the control plane's log group; its retention is 0
	// when the logs never expire.
	LogGroupName          string
	LogGroupRetentionDays int
	// EncryptionKeyARN is the KMS key of the secrets encryption. It is
	// never deleted with the cluster: the platform cannot tell what else
	// uses it.
	EncryptionKeyARN  string
	IdentityProviders []IdentityProviderPlan
	// Addons are add-ons the contract has no name for.
	Addons []AddonPlan
	// PodIdentities are the associations that are not an add-on's.
	PodIdentities []PodIdentityPlan
}

// IdentityProviderPlan is an OIDC identity provider config of the cluster.
type IdentityProviderPlan struct {
	Name, IssuerURL, ClientID                                string
	GroupsClaim, GroupsPrefix, UsernameClaim, UsernamePrefix string
	RequiredClaims                                           map[string]string
}

// PodIdentityPlan is a pod identity association.
type PodIdentityPlan struct{ ID, Namespace, ServiceAccount string }

// CarriedOver renders c for the cluster of p: Observe-only objects marked
// crossplane.CarriedOverAnnotation, with these names and external names:
//
//	RolePolicyAttachment "<prefix>-<role>-x-<hash8>"   <role name>/<policy ARN>
//	OpenIDConnectProvider "<prefix>-oidc"              its ARN
//	Group "<prefix>-logs"                              the log group name
//	Key "<prefix>-secrets-key"                         the key ID (retained)
//	IdentityProviderConfig "<prefix>-idp-<hash8>"      the config's name
//	Addon "<prefix>-addon-<name>"                      <cluster>:<add-on>
//	PodIdentityAssociation "<prefix>-identity-<id>"    the association ID
func CarriedOver(p ClusterPlan, c CarriedOverResources) []client.Object {
	b := clusterBuilder{p: p}
	var out []client.Object
	for _, role := range []string{"cluster-role", "node-role"} {
		for _, policy := range c.ExtraPolicies[role] {
			a := b.attach(role+"-x-"+shortHash(policy), role, policy)
			a.ObjectMeta = b.carried(a.Name, c.RoleNames[role]+"/"+policy, false)
			a.Spec.ManagementPolicies = PolicyObserveLateInit
			out = append(out, a)
		}
	}
	if c.OIDCProviderARN != "" {
		out = append(out, &awsiam.OpenIDConnectProvider{ObjectMeta: b.carried(b.name("oidc"), c.OIDCProviderARN, false),
			Spec: awsiam.OpenIDConnectProviderSpec{ManagedResourceSpec: b.observe(), ForProvider: awsiam.OpenIDConnectProviderParameters{
				URL: ptr.To(c.OIDCIssuer), ClientIDList: ptrs([]string{oidcClientID}),
			}}})
	}
	if c.LogGroupName != "" {
		params := awscwl.GroupParameters{Region: ptr.To(p.Region)}
		if c.LogGroupRetentionDays > 0 {
			params.RetentionInDays = ptr.To(float64(c.LogGroupRetentionDays))
		}
		out = append(out, &awscwl.Group{ObjectMeta: b.carried(b.name("logs"), c.LogGroupName, false),
			Spec: awscwl.GroupSpec{ManagedResourceSpec: b.observe(), ForProvider: params}})
	}
	if c.EncryptionKeyARN != "" {
		id := c.EncryptionKeyARN[strings.LastIndex(c.EncryptionKeyARN, "/")+1:]
		out = append(out, &awskms.Key{ObjectMeta: b.carried(b.name("secrets-key"), id, true),
			Spec: awskms.KeySpec{ManagedResourceSpec: b.observe(), ForProvider: awskms.KeyParameters{Region: ptr.To(p.Region)}}})
	}
	for _, idp := range c.IdentityProviders {
		out = append(out, b.identityProvider(idp))
	}
	for _, a := range c.Addons {
		addon := b.addon(a)
		addon.ObjectMeta = b.carried(addon.Name, p.ClusterName+":"+a.Name, false)
		addon.Spec.ManagementPolicies = PolicyObserveLateInit
		addon.Spec.ForProvider.ResolveConflictsOnCreate, addon.Spec.ForProvider.ResolveConflictsOnUpdate = nil, nil
		addon.Spec.ForProvider.Tags = nil
		out = append(out, addon)
	}
	for _, pi := range c.PodIdentities {
		out = append(out, &awseks.PodIdentityAssociation{ObjectMeta: b.carried(b.name("identity-"+pi.ID), pi.ID, false),
			Spec: awseks.PodIdentityAssociationSpec{ManagedResourceSpec: b.observe(), ForProvider: awseks.PodIdentityAssociationParameters_2{
				Region: ptr.To(p.Region), ClusterNameRef: b.ref(p.Prefix), Namespace: ptr.To(pi.Namespace), ServiceAccount: ptr.To(pi.ServiceAccount),
			}}})
	}
	return out
}

func (b clusterBuilder) identityProvider(idp IdentityProviderPlan) *awseks.IdentityProviderConfig {
	return &awseks.IdentityProviderConfig{ObjectMeta: b.carried(b.name("idp-"+shortHash(idp.Name)), idp.Name, false),
		Spec: awseks.IdentityProviderConfigSpec{ManagedResourceSpec: b.observe(), ForProvider: awseks.IdentityProviderConfigParameters{
			Region: ptr.To(b.p.Region), ClusterNameRef: b.ref(b.p.Prefix),
			Oidc: &awseks.IdentityProviderConfigOidcParameters{
				ClientID: ptr.To(idp.ClientID), IssuerURL: ptr.To(idp.IssuerURL), // the config's name is its external name
				GroupsClaim: optional(idp.GroupsClaim), GroupsPrefix: optional(idp.GroupsPrefix),
				UsernameClaim: optional(idp.UsernameClaim), UsernamePrefix: optional(idp.UsernamePrefix),
				RequiredClaims: strPtrs(idp.RequiredClaims),
			},
		}}}
}

// carried is the metadata of a carried-over object; retained marks one
// that a takeover owns without Delete (RetainAnnotation).
func (b clusterBuilder) carried(name, externalName string, retained bool) metav1.ObjectMeta {
	meta := b.meta(name)
	meta.Annotations = map[string]string{ExternalNameAnnotation: externalName, crossplane.CarriedOverAnnotation: "true"}
	if retained {
		meta.Annotations[RetainAnnotation] = "true"
	}
	return meta
}

// observe is the spec of an imported object: it only reads.
func (b clusterBuilder) observe() xpv2.ManagedResourceSpec {
	spec := b.spec()
	spec.ManagementPolicies = PolicyObserveLateInit
	return spec
}
