package aws

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
)

// Access modes of a component's IAM role (ComponentIAMPlan.Access).
const (
	ComponentAccessIRSA        = "IRSA"
	ComponentAccessPodIdentity = "PodIdentity"
)

const (
	// maxIAMRoleName is IAM's limit on role names.
	maxIAMRoleName = 64
	// componentRoleSuffix ends a component's role name.
	componentRoleSuffix = "-role"
	// stsAudience is the audience of the tokens EKS projects for IRSA.
	stsAudience = "sts.amazonaws.com"
	// oidcProviderARNMarker separates an OIDC provider ARN's issuer.
	oidcProviderARNMarker = ":oidc-provider/"
)

var policyKind = UsageRef{APIVersion: awsiam.CRDGroupVersion.String(), Kind: awsiam.Policy_Kind}

// ServiceAccountPlan is a service account in the workload cluster.
type ServiceAccountPlan struct{ Namespace, Name string }

// ComponentIAMPlan is a component's IAM role on one cluster, resolved to
// concrete values: its placeholders already filled in
// (pkg/component.Expand). ComponentIAMResources only renders it.
type ComponentIAMPlan struct {
	// The cluster's: tenant namespace, ProviderConfig, IDs, EKS name,
	// prefix (ClusterPlan.Prefix), region and tags.
	Namespace, ProviderConfigName, TenantID, ClusterID, ClusterName, ClusterPrefix, Region string
	Tags                                                                                   map[string]string
	// Component is the catalog name.
	Component string
	// Access is ComponentAccessIRSA or ComponentAccessPodIdentity.
	Access          string
	ServiceAccounts []ServiceAccountPlan
	// Policy is the policy document the platform creates; empty: none.
	Policy string
	// ManagedPolicyARNs are existing policies to attach.
	ManagedPolicyARNs []string
	// OIDCProviderARN is the ARN of the cluster's IAM OIDC provider
	// (arn:...:oidc-provider/<issuer host and path>): what an IRSA role
	// trusts. Known once the provider exists; until then an IRSA plan
	// renders no role (ComponentIAMSet reads it when it applies).
	OIDCProviderARN string
}

// ComponentIAMBase is what every object name of a component on a cluster
// starts with: "<cluster prefix>-<component>", cut and suffixed with a
// hash when longer than the role name allows.
func ComponentIAMBase(clusterPrefix, component string) string {
	base := clusterPrefix + "-" + component
	limit := maxIAMRoleName - len(componentRoleSuffix)
	if len(base) <= limit {
		return base
	}
	sum := sha256.Sum256([]byte(base))
	return strings.TrimRight(base[:limit-9], "-.") + "-" + hex.EncodeToString(sum[:])[:8]
}

// ComponentRoleName is the object name, and the IAM name, of a component's
// role: what iam.roleName is.
func ComponentRoleName(p ComponentIAMPlan) string { return p.base() + componentRoleSuffix }

// ComponentPolicyName is the object name of a component's own policy.
func ComponentPolicyName(p ComponentIAMPlan) string { return p.base() + "-policy" }

// NodeRoleName is the IAM name of a cluster's node role: the name an
// import recorded, or the object's own ("<prefix>-node-role").
func NodeRoleName(p ClusterPlan) string {
	return orDefault(p.NodeRoleName, clusterBuilder{p: p}.name("node-role"))
}

func (p ComponentIAMPlan) base() string { return ComponentIAMBase(p.ClusterPrefix, p.Component) }

func (p ComponentIAMPlan) builder() clusterBuilder {
	return clusterBuilder{p: ClusterPlan{Namespace: p.Namespace, ProviderConfigName: p.ProviderConfigName,
		TenantID: p.TenantID, ClusterID: p.ClusterID, ClusterName: p.ClusterName, Prefix: p.ClusterPrefix,
		Region: p.Region, Tags: p.Tags}}
}

func (p ComponentIAMPlan) meta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: p.Namespace,
		Labels: crossplane.ComponentLabels(p.TenantID, p.ClusterID, p.Component)}
}

func (p ComponentIAMPlan) attachmentNames() []string {
	var out []string
	if p.Policy != "" {
		out = append(out, p.base()+"-attach-policy")
	}
	for i := range p.ManagedPolicyARNs {
		out = append(out, fmt.Sprintf("%s-attach-%d", p.base(), i+1))
	}
	return out
}

func (p ComponentIAMPlan) associationNames() []string {
	if p.Access != ComponentAccessPodIdentity {
		return nil
	}
	out := make([]string, 0, len(p.ServiceAccounts))
	for i := range p.ServiceAccounts {
		out = append(out, fmt.Sprintf("%s-identity-%d", p.base(), i+1))
	}
	return out
}

// ComponentIAMResources renders the plan in creation order: the policy,
// the role (an IRSA role only once OIDCProviderARN is known), the
// attachments, one pod identity association per service account
// (PodIdentity), then the Usages that delete them in reverse: the
// associations first, then the attachments, then the role and the policy.
func ComponentIAMResources(p ComponentIAMPlan) []client.Object {
	b := p.builder()
	var out []client.Object
	if p.Policy != "" {
		name := ComponentPolicyName(p)
		out = append(out, &awsiam.Policy{ObjectMeta: p.meta(name),
			Spec: awsiam.PolicySpec{ManagedResourceSpec: b.spec(), ForProvider: awsiam.PolicyParameters{
				Policy:      ptr.To(p.Policy),
				Description: ptr.To(fmt.Sprintf("Component %s of EKS cluster %s", p.Component, p.ClusterName)),
				Tags:        b.tags(name),
			}}})
	}
	if role := p.role(); role != nil {
		out = append(out, role)
	}
	out = append(out, p.attachments()...)
	for i, name := range p.associationNames() {
		sa := p.ServiceAccounts[i]
		out = append(out, &awseks.PodIdentityAssociation{ObjectMeta: p.meta(name),
			Spec: awseks.PodIdentityAssociationSpec{ManagedResourceSpec: b.spec(), ForProvider: awseks.PodIdentityAssociationParameters_2{
				Region: ptr.To(p.Region), ClusterNameRef: b.ref(p.ClusterPrefix), RoleArnRef: b.ref(ComponentRoleName(p)),
				Namespace: ptr.To(sa.Namespace), ServiceAccount: ptr.To(sa.Name), Tags: b.tags(name),
			}}})
	}
	rendered := map[string]bool{}
	for _, obj := range out {
		rendered[obj.GetName()] = true
	}
	// A Usage only for objects rendered: an IRSA role may not be yet.
	for _, u := range Usages(p.Namespace, p.base(), crossplane.ComponentLabels(p.TenantID, p.ClusterID, p.Component), p.usagePairs()) {
		if rendered[u.Spec.Of.ResourceRef.Name] && rendered[u.Spec.By.ResourceRef.Name] {
			out = append(out, u)
		}
	}
	return out
}

// role is the component's role with the platform's trust policy; nil for
// an IRSA role while the OIDC provider is not known.
func (p ComponentIAMPlan) role() *awsiam.Role {
	trust := trustPodIdentity
	if p.Access != ComponentAccessPodIdentity {
		if p.OIDCProviderARN == "" {
			return nil
		}
		trust = trustIRSA(p.OIDCProviderARN, p.ServiceAccounts)
	}
	name := ComponentRoleName(p)
	b := p.builder()
	return &awsiam.Role{ObjectMeta: p.meta(name),
		Spec: awsiam.RoleSpec{ManagedResourceSpec: b.spec(), ForProvider: awsiam.RoleParameters{
			AssumeRolePolicy: ptr.To(trust), Tags: b.tags(name),
		}}}
}

func (p ComponentIAMPlan) attachments() []client.Object {
	b := p.builder()
	names := p.attachmentNames()
	out := make([]client.Object, 0, len(names))
	attach := func(name string, params awsiam.RolePolicyAttachmentParameters) {
		params.RoleRef = b.ref(ComponentRoleName(p))
		out = append(out, &awsiam.RolePolicyAttachment{ObjectMeta: p.meta(name),
			Spec: awsiam.RolePolicyAttachmentSpec{ManagedResourceSpec: b.spec(), ForProvider: params}})
	}
	i := 0
	if p.Policy != "" {
		attach(names[0], awsiam.RolePolicyAttachmentParameters{PolicyArnRef: b.ref(ComponentPolicyName(p))})
		i = 1
	}
	for _, arn := range p.ManagedPolicyARNs {
		attach(names[i], awsiam.RolePolicyAttachmentParameters{PolicyArn: ptr.To(arn)})
		i++
	}
	return out
}

// usagePairs: an association keeps every attachment, an attachment keeps
// the role, the policy's attachment keeps the policy.
func (p ComponentIAMPlan) usagePairs() []UsagePair {
	role := roleKind.named(ComponentRoleName(p))
	var pairs []UsagePair
	attachments := p.attachmentNames()
	for _, assoc := range p.associationNames() {
		for _, a := range attachments {
			pairs = append(pairs, UsagePair{Of: attachmentKind.named(a), By: podIdentityKind.named(assoc)})
		}
	}
	for _, a := range attachments {
		pairs = append(pairs, UsagePair{Of: role, By: attachmentKind.named(a)})
	}
	if p.Policy != "" {
		pairs = append(pairs, UsagePair{Of: policyKind.named(ComponentPolicyName(p)), By: attachmentKind.named(attachments[0])})
	}
	return pairs
}

// trustIRSA lets the service accounts assume the role with the tokens of
// the cluster's OIDC provider.
func trustIRSA(providerARN string, accounts []ServiceAccountPlan) string {
	issuer := providerARN[strings.Index(providerARN, oidcProviderARNMarker)+len(oidcProviderARNMarker):]
	subjects := make([]string, 0, len(accounts))
	for _, sa := range accounts {
		subjects = append(subjects, "system:serviceaccount:"+sa.Namespace+":"+sa.Name)
	}
	doc, _ := json.Marshal(map[string]any{ // maps and strings: cannot fail
		"Version": "2012-10-17",
		"Statement": []map[string]any{{
			"Effect": "Allow", "Principal": map[string]any{"Federated": providerARN},
			"Action": "sts:AssumeRoleWithWebIdentity",
			"Condition": map[string]any{"StringEquals": map[string]any{
				issuer + ":aud": stsAudience, issuer + ":sub": subjects,
			}},
		}},
	})
	return string(doc)
}

// ComponentIAMSet is ComponentIAMResources as a set to apply in order:
// the attachments and associations wait for the role to be Ready (the
// policy's attachment for the policy too), and each Usage for its "by"
// object. An IRSA role is built when the set is applied, from the
// cluster's IAM OIDC provider ("<cluster prefix>-oidc") as the management
// cluster has it: until that provider has its ARN the role, and so
// everything after it, waits.
func ComponentIAMSet(p ComponentIAMPlan) sequence.Set {
	full := p
	if p.Access != ComponentAccessPodIdentity && full.OIDCProviderARN == "" {
		// The identities of all objects; the role is built at apply time.
		full.OIDCProviderARN = "arn:aws:iam::000000000000" + oidcProviderARNMarker + "pending"
	}
	set := sequence.Of(ComponentIAMResources(full)...)
	role := &awsiam.Role{ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: ComponentRoleName(p)}}
	roleReady := []sequence.Condition{sequence.Ready(role)}
	for i, item := range set {
		switch obj := item.Object.(type) {
		case *awsiam.Role:
			if p.Access != ComponentAccessPodIdentity && p.OIDCProviderARN == "" {
				set[i].Object = role
				set[i].Build = p.buildIRSARole()
			}
		case *awsiam.RolePolicyAttachment:
			set[i].After = roleReady
			if obj.Spec.ForProvider.PolicyArnRef != nil {
				policy := &awsiam.Policy{ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: ComponentPolicyName(p)}}
				set[i].After = append([]sequence.Condition{sequence.Ready(policy)}, roleReady...)
			}
		case *awseks.PodIdentityAssociation:
			set[i].After = roleReady
		}
	}
	return usagesAfterTheirUsers(set, nil)
}

// buildIRSARole renders the role once the cluster's OIDC provider has an
// ARN.
func (p ComponentIAMPlan) buildIRSARole() func(context.Context, sequence.Observed) (client.Object, string, error) {
	return func(ctx context.Context, seen sequence.Observed) (client.Object, string, error) {
		provider := &awsiam.OpenIDConnectProvider{ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: p.ClusterPrefix + "-oidc"}}
		found, err := seen.Get(ctx, provider)
		if err != nil {
			return nil, "", fmt.Errorf("read the cluster's IAM OIDC provider %s: %w", provider.Name, err)
		}
		arn := ""
		if found {
			arn = ptr.Deref(provider.Status.AtProvider.Arn, "")
		}
		if !strings.Contains(arn, oidcProviderARNMarker) {
			return nil, "the cluster's IAM OIDC provider exists (spec.aws.irsa)", nil
		}
		live := p
		live.OIDCProviderARN = arn
		return live.role(), "", nil
	}
}
