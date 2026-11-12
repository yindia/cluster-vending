package aws_test

import (
	"slices"
	"testing"

	protectionv1beta1 "github.com/crossplane/crossplane/apis/v2/protection/v1beta1"
	awscwl "github.com/upbound/provider-aws/v2/apis/namespaced/cloudwatchlogs/v1beta1"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	awskms "github.com/upbound/provider-aws/v2/apis/namespaced/kms/v1beta1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

const (
	testKeyARN = "arn:aws:kms:us-east-1:444455556666:key/1234abcd-12ab-34cd-56ef-1234567890ab"
	testIssuer = "https://oidc.eks.us-east-1.amazonaws.com/id/ABCDEF"
	viewPolicy = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy"
)

// allOptions turns every optional feature on, with the live values they
// wait for.
func allOptions(p *crossplaneaws.ClusterPlan) {
	p.EndpointPublicAccess = ptr.To(true)
	p.AdditionalSecurityGroupIDs = []string{"sg-0aaa"}
	p.LogGroupRetentionDays = 90
	p.CreateEncryptionKey, p.EncryptionKeyARN = true, testKeyARN
	p.IRSA, p.OIDCIssuer = true, testIssuer
	p.ClusterSecurityGroupID = "sg-0c1u5ter"
	p.ClusterRoleName, p.NodeRoleName = "tf-cluster", "tf-node"
	p.AccessEntries = []crossplaneaws.AccessEntryPlan{
		{Principal: "arn:aws:iam::444455556666:role/dev", Policies: []crossplaneaws.AccessPolicyPlan{
			{PolicyARN: viewPolicy, Scope: "namespace", Namespaces: []string{"b", "a"}}}},
		{Principal: "arn:aws:iam::444455556666:role/nodes", Type: "EC2_LINUX"},
	}
	p.IdentityProvider = &crossplaneaws.IdentityProviderPlan{Name: "okta", IssuerURL: "https://example.okta.com", ClientID: "kubernetes",
		UsernameClaim: "email", GroupsClaim: "groups", GroupsPrefix: "oidc:", RequiredClaims: map[string]string{"hd": "example.com"}}
	p.NodePool.LaunchTemplate = &crossplaneaws.LaunchTemplatePlan{SecurityGroupIDs: []string{"sg-0bbb"},
		ImageID: "ami-0123456789abcdef0", UserData: "Zm9v"}
	p.NodePool.AMIType, p.NodePool.DiskSizeGB = "CUSTOM", 0
}

func byName(objects []client.Object) map[string]client.Object {
	out := map[string]client.Object{}
	for _, o := range objects {
		out[o.GetName()] = o
	}
	return out
}

func identityProviders(objects []client.Object) []*awseks.IdentityProviderConfig {
	var out []*awseks.IdentityProviderConfig
	for _, o := range objects {
		if idp, ok := o.(*awseks.IdentityProviderConfig); ok {
			out = append(out, idp)
		}
	}
	return out
}

func kinds(objects []client.Object) map[string]int {
	out := map[string]int{}
	for _, o := range objects {
		out[typeName(o)]++
	}
	return out
}

func typeName(o client.Object) string {
	switch o.(type) {
	case *awscwl.Group:
		return "Group"
	case *awskms.Key:
		return "Key"
	case *awskms.Alias:
		return "Alias"
	case *awsiam.OpenIDConnectProvider:
		return "OpenIDConnectProvider"
	case *awsec2.LaunchTemplate:
		return "LaunchTemplate"
	case *awseks.Cluster:
		return "Cluster"
	case *awseks.NodeGroup:
		return "NodeGroup"
	case *awseks.ClusterAuth:
		return "ClusterAuth"
	case *awseks.AccessEntry:
		return "AccessEntry"
	case *awseks.AccessPolicyAssociation:
		return "AccessPolicyAssociation"
	case *protectionv1beta1.Usage:
		return "Usage"
	default:
		return "other"
	}
}

func TestClusterOptionsRender(t *testing.T) {
	p := clusterPlan()
	allOptions(&p)
	objects := crossplaneaws.ClusterResources(p)
	o := byName(objects)

	// What must exist before the EKS cluster comes first.
	if _, ok := objects[0].(*awscwl.Group); !ok {
		t.Errorf("first object is %T, want the log group (before the Cluster)", objects[0])
	}
	// cloudwatchlogs Group: NameAsIdentifier, the log group name.
	group := o["prod-1a2b3c4d-logs"].(*awscwl.Group)
	if group.Annotations[crossplaneaws.ExternalNameAnnotation] != "/aws/eks/prod/cluster" || *group.Spec.ForProvider.RetentionInDays != 90 {
		t.Errorf("log group = %v %+v", group.Annotations, group.Spec.ForProvider)
	}
	// kms Key: IdentifierFromProvider (none set); Alias: "alias/<name>".
	key := o["prod-1a2b3c4d-secrets-key"].(*awskms.Key)
	if !*key.Spec.ForProvider.EnableKeyRotation || key.Annotations[crossplaneaws.ExternalNameAnnotation] != "" {
		t.Errorf("key = %v %+v", key.Annotations, key.Spec.ForProvider)
	}
	alias := o["prod-1a2b3c4d-secrets-alias"].(*awskms.Alias)
	if alias.Annotations[crossplaneaws.ExternalNameAnnotation] != "alias/prod-eks" || alias.Spec.ForProvider.TargetKeyIDRef.Name != key.Name {
		t.Errorf("alias = %v %+v", alias.Annotations, alias.Spec.ForProvider)
	}
	eks := o["prod-1a2b3c4d"].(*awseks.Cluster).Spec.ForProvider
	if enc := eks.EncryptionConfig; enc == nil || *enc.Provider.KeyArn != testKeyARN || *enc.Resources[0] != "secrets" {
		t.Errorf("encryption config = %+v", enc)
	}
	if len(eks.VPCConfig.SecurityGroupIds) != 1 || *eks.VPCConfig.SecurityGroupIds[0] != "sg-0aaa" {
		t.Errorf("security groups = %v", eks.VPCConfig.SecurityGroupIds)
	}
	// iam Role: NameAsIdentifier, the role's name.
	for name, want := range map[string]string{"prod-1a2b3c4d-cluster-role": "tf-cluster", "prod-1a2b3c4d-node-role": "tf-node"} {
		if got := o[name].(*awsiam.Role).Annotations[crossplaneaws.ExternalNameAnnotation]; got != want {
			t.Errorf("%s external name = %q, want %q", name, got, want)
		}
	}
	// iam OpenIDConnectProvider: IdentifierFromProvider (its ARN, set by
	// the provider).
	oidc := o["prod-1a2b3c4d-oidc"].(*awsiam.OpenIDConnectProvider)
	if *oidc.Spec.ForProvider.URL != testIssuer || *oidc.Spec.ForProvider.ClientIDList[0] != "sts.amazonaws.com" ||
		len(oidc.Spec.ForProvider.ThumbprintList) != 1 || oidc.Annotations[crossplaneaws.ExternalNameAnnotation] != "" {
		t.Errorf("oidc provider = %+v", oidc.Spec.ForProvider)
	}
	// ec2 LaunchTemplate: IdentifierFromProvider (lt-..., set by the provider).
	ng := o["prod-1a2b3c4d-default"].(*awseks.NodeGroup)
	lt := o["prod-1a2b3c4d-default-lt"].(*awsec2.LaunchTemplate).Spec.ForProvider
	if *lt.VPCSecurityGroupIds[0] != "sg-0c1u5ter" || *lt.VPCSecurityGroupIds[1] != "sg-0bbb" || *lt.ImageID != "ami-0123456789abcdef0" ||
		*lt.UserData != "Zm9v" || *lt.Name != "prod-1a2b3c4d-default-lt" {
		t.Errorf("launch template = %+v", lt)
	}
	if got := ng.Spec.ForProvider; *got.LaunchTemplate.Name != "prod-1a2b3c4d-default-lt" || *got.LaunchTemplate.Version != "1" ||
		got.Version != nil || *got.AMIType != "CUSTOM" || ng.Annotations[crossplaneaws.LaunchTemplateAnnotation] == "" {
		t.Errorf("node group = %+v %v, want the rendered template, no version (custom AMI)", got.LaunchTemplate, ng.Annotations)
	}
	// Access entries: named by principal and type; the association by
	// policy and scope.
	k := kinds(objects)
	if k["AccessEntry"] != 3 || k["AccessPolicyAssociation"] != 2 {
		t.Errorf("access entries %d, associations %d; want the admin's and the two entries, with one policy each for two", k["AccessEntry"], k["AccessPolicyAssociation"])
	}
	var scoped *awseks.AccessPolicyAssociation
	for _, obj := range objects {
		if a, ok := obj.(*awseks.AccessPolicyAssociation); ok && *a.Spec.ForProvider.PolicyArn == viewPolicy {
			scoped = a
		}
	}
	if scoped == nil || *scoped.Spec.ForProvider.AccessScope.Type != "namespace" || *scoped.Spec.ForProvider.AccessScope.Namespaces[0] != "a" {
		t.Fatalf("namespace-scoped association = %+v", scoped)
	}
	// eks IdentityProviderConfig: the association's name is the external
	// name; every setting is part of the object's name.
	idps := identityProviders(objects)
	if len(idps) != 1 {
		t.Fatalf("identity providers = %d, want 1", len(idps))
	}
	idp := idps[0]
	if got := idp.Spec.ForProvider; idp.Annotations[crossplaneaws.ExternalNameAnnotation] != "okta" ||
		*got.Oidc.IssuerURL != "https://example.okta.com" || *got.Oidc.ClientID != "kubernetes" || *got.Oidc.UsernameClaim != "email" ||
		got.Oidc.UsernamePrefix != nil || *got.Oidc.GroupsClaim != "groups" || *got.Oidc.GroupsPrefix != "oidc:" ||
		*got.Oidc.RequiredClaims["hd"] != "example.com" || got.ClusterNameRef.Name != "prod-1a2b3c4d" ||
		idp.Spec.ManagementPolicies != nil || *got.Tags[crossplaneaws.TagClusterName] != "prod" {
		t.Errorf("identity provider = %v %+v %+v", idp.Annotations, got, got.Oidc)
	}
	changed := p
	changed.IdentityProvider = ptr.To(*p.IdentityProvider)
	changed.IdentityProvider.GroupsPrefix = "sso:"
	if other := identityProviders(crossplaneaws.ClusterResources(changed)); len(other) != 1 || other[0].Name == idp.Name {
		t.Errorf("a changed identity provider renders %v, want one object with a new name (not %s)", other, idp.Name)
	}
	if again := identityProviders(crossplaneaws.ClusterResources(p)); again[0].Name != idp.Name {
		t.Errorf("the same identity provider renders %s, then %s", idp.Name, again[0].Name)
	}
	// Usages: the log group, the key and the OIDC provider outlive the EKS
	// cluster; the launch template its node group.
	held := map[string]string{}
	for _, obj := range objects {
		if u, ok := obj.(*protectionv1beta1.Usage); ok {
			held[u.Spec.Of.Kind+"/"+u.Spec.Of.ResourceRef.Name] = u.Spec.By.Kind
		}
	}
	for of, by := range map[string]string{"Group/prod-1a2b3c4d-logs": "Cluster", "Key/prod-1a2b3c4d-secrets-key": "Cluster",
		"OpenIDConnectProvider/prod-1a2b3c4d-oidc": "Cluster", "LaunchTemplate/prod-1a2b3c4d-default-lt": "NodeGroup"} {
		if held[of] != by {
			t.Errorf("usage of %s by %q, want by %s", of, held[of], by)
		}
	}
}

func TestClusterEndpointAccess(t *testing.T) {
	for _, tt := range []struct {
		name            string
		private, public *bool
		wantPrivate     bool
		wantPublic      bool
		wantCIDRs       int
	}{
		{"defaults", nil, nil, true, true, 1},
		{"private only", nil, ptr.To(false), true, false, 0},
		{"public only", ptr.To(false), ptr.To(true), false, true, 1},
	} {
		p := clusterPlan()
		p.EndpointPrivateAccess, p.EndpointPublicAccess = tt.private, tt.public
		vpc := byName(crossplaneaws.ClusterResources(p))["prod-1a2b3c4d"].(*awseks.Cluster).Spec.ForProvider.VPCConfig
		if *vpc.EndpointPrivateAccess != tt.wantPrivate || *vpc.EndpointPublicAccess != tt.wantPublic || len(vpc.PublicAccessCidrs) != tt.wantCIDRs {
			t.Errorf("%s: private %t public %t cidrs %d", tt.name, *vpc.EndpointPrivateAccess, *vpc.EndpointPublicAccess, len(vpc.PublicAccessCidrs))
		}
	}
}

// What an object needs and does not exist yet keeps it, and the Usages
// that refer to it, out of the render.
func TestClusterResourcesPending(t *testing.T) {
	for _, tt := range []struct {
		name    string
		change  func(*crossplaneaws.ClusterPlan)
		pending []string
	}{
		{"nothing", func(*crossplaneaws.ClusterPlan) {}, nil},
		{"held for the log group", func(p *crossplaneaws.ClusterPlan) { p.ClusterHeld = true },
			[]string{"Cluster", "ClusterAuth", "LaunchTemplate", "NodeGroup"}},
		{"key without an ARN yet", func(p *crossplaneaws.ClusterPlan) { p.EncryptionKeyARN = "" },
			[]string{"Cluster", "ClusterAuth", "LaunchTemplate", "NodeGroup"}},
		{"issuer unknown", func(p *crossplaneaws.ClusterPlan) { p.OIDCIssuer = "" }, []string{"OpenIDConnectProvider"}},
		{"cluster security group unknown", func(p *crossplaneaws.ClusterPlan) { p.ClusterSecurityGroupID = "" },
			[]string{"LaunchTemplate", "NodeGroup"}},
	} {
		p := clusterPlan()
		allOptions(&p)
		tt.change(&p)
		if got := p.Pending(); !slices.Equal(got, tt.pending) {
			t.Errorf("%s: pending %v, want %v", tt.name, got, tt.pending)
		}
		objects := crossplaneaws.ClusterResources(p)
		names := byName(objects)
		for _, obj := range objects {
			if slices.Contains(tt.pending, typeName(obj)) {
				t.Errorf("%s: %s %s rendered", tt.name, typeName(obj), obj.GetName())
			}
			if u, ok := obj.(*protectionv1beta1.Usage); ok && u.Spec.Of.Kind != "Subnet" &&
				(names[u.Spec.Of.ResourceRef.Name] == nil || names[u.Spec.By.ResourceRef.Name] == nil) {
				t.Errorf("%s: usage %s refers to an object that is not rendered", tt.name, u.Name)
			}
		}
	}
}

// A launch template EKS cannot swap in place gives the node group a new
// name (it is replaced); a new version of the same template does not.
func TestNodeGroupReplacementOnLaunchTemplate(t *testing.T) {
	base := clusterPlan().NodePool
	name := func(lt *crossplaneaws.LaunchTemplatePlan) string {
		n := base
		n.LaunchTemplate = lt
		return crossplaneaws.ReplacementNodeGroupName("prod", n)
	}
	none := name(nil)
	v1 := name(&crossplaneaws.LaunchTemplatePlan{ID: "lt-0aaa", Version: "1"})
	v2 := name(&crossplaneaws.LaunchTemplatePlan{ID: "lt-0aaa", Version: "2"})
	other := name(&crossplaneaws.LaunchTemplatePlan{ID: "lt-0bbb", Version: "1"})
	rendered := name(&crossplaneaws.LaunchTemplatePlan{SecurityGroupIDs: []string{"sg-1"}})
	renderedMore := name(&crossplaneaws.LaunchTemplatePlan{SecurityGroupIDs: []string{"sg-1", "sg-2"}})
	if v1 != v2 || none == v1 || v1 == other || rendered == renderedMore || rendered == none {
		t.Errorf("names: none %s, v1 %s, v2 %s, other %s, rendered %s / %s", none, v1, v2, other, rendered, renderedMore)
	}
	p := clusterPlan()
	p.NodePool.LaunchTemplate = &crossplaneaws.LaunchTemplatePlan{ID: "lt-0aaa", Version: "1"}
	ng := byName(crossplaneaws.ClusterResources(p))["prod-1a2b3c4d-default"].(*awseks.NodeGroup)
	if *ng.Spec.ForProvider.LaunchTemplate.ID != "lt-0aaa" || ng.Spec.ForProvider.LaunchTemplate.Name != nil ||
		*ng.Spec.ForProvider.LaunchTemplate.Version != "1" {
		t.Errorf("existing launch template = %+v", ng.Spec.ForProvider.LaunchTemplate)
	}
	same := p.NodePool
	same.LaunchTemplate = &crossplaneaws.LaunchTemplatePlan{ID: "lt-0aaa", Version: "2"}
	swapped := p.NodePool
	swapped.LaunchTemplate = nil
	if !crossplaneaws.SameNodeGroup(ng, same) || crossplaneaws.SameNodeGroup(ng, swapped) {
		t.Error("SameNodeGroup: a new version is the same node group, a removed template is not")
	}
}
