package aws_test

import (
	"slices"
	"testing"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	awscwl "github.com/upbound/provider-aws/v2/apis/namespaced/cloudwatchlogs/v1beta1"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	awskms "github.com/upbound/provider-aws/v2/apis/namespaced/kms/v1beta1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// External names per kind, as provider-upjet-aws v2.9.0 builds them
// (config/externalname.go; see crossplaneaws.Adopt).
func TestAdoptExternalNames(t *testing.T) {
	const dev, policy = "arn:aws:iam::444455556666:role/dev", viewPolicy
	p := clusterPlan()
	p.AdminPrincipals = nil
	p.AccessEntries = []crossplaneaws.AccessEntryPlan{{Principal: dev, Type: "STANDARD",
		Policies: []crossplaneaws.AccessPolicyPlan{{PolicyARN: policy, Scope: "cluster"}}}}
	ids := crossplaneaws.ClusterIdentities{ClusterName: "prod", NodeGroupName: "tf-ng",
		RoleNames:        map[string]string{"prod-1a2b3c4d-cluster-role": "tf-cluster", "prod-1a2b3c4d-node-role": "tf-node"},
		AttachedPolicies: map[string][]string{"tf-node": {"arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy"}},
		Addons:           []string{"coredns"},
		AccessEntries:    map[string][]crossplaneaws.AccessPolicyPlan{dev: {{PolicyARN: policy, Scope: "cluster"}}},
		PodIdentities:    map[string]string{"kube-system/ebs-csi-controller-sa": "a-0123"},
	}
	want := map[string]string{
		"*v1beta1.Cluster/prod-1a2b3c4d":                                 "prod",
		"*v1beta1.NodeGroup/prod-1a2b3c4d-default":                       "tf-ng",
		"*v1beta1.Role/prod-1a2b3c4d-cluster-role":                       "tf-cluster",
		"*v1beta1.Role/prod-1a2b3c4d-node-role":                          "tf-node",
		"*v1beta1.RolePolicyAttachment/prod-1a2b3c4d-node-cni":           "tf-node/arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy",
		"*v1beta1.Addon/prod-1a2b3c4d-addon-coredns":                     "prod:coredns",
		"*v1beta1.PodIdentityAssociation/prod-1a2b3c4d-ebs-csi-identity": "a-0123",
	}
	got := map[string]string{}
	var entry, association string
	for _, obj := range crossplaneaws.ClusterResources(p) {
		name, ok := crossplaneaws.Adopt(obj, ids)
		if !ok {
			continue
		}
		switch o := obj.(type) {
		case *awseks.AccessEntry:
			entry = name
		case *awseks.AccessPolicyAssociation:
			association = name
		case *awseks.Cluster:
			got["*v1beta1.Cluster/"+o.Name] = name
			if len(o.Spec.ManagementPolicies) != 2 || o.Spec.ManagementPolicies[0] != "Observe" || o.Spec.ManagementPolicies[1] != "LateInitialize" {
				t.Errorf("adopted cluster policies = %v", o.Spec.ManagementPolicies)
			}
		case *awseks.NodeGroup:
			got["*v1beta1.NodeGroup/"+o.Name] = name
		case *awsiam.Role:
			got["*v1beta1.Role/"+o.Name] = name
		case *awsiam.RolePolicyAttachment:
			got["*v1beta1.RolePolicyAttachment/"+o.Name] = name
		case *awseks.Addon:
			got["*v1beta1.Addon/"+o.Name] = name
		case *awseks.PodIdentityAssociation:
			got["*v1beta1.PodIdentityAssociation/"+o.Name] = name
		}
	}
	if len(got) != len(want) {
		t.Errorf("adopted %v, want %v (objects the cloud does not have are not adopted)", got, want)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s external name = %q, want %q", k, got[k], w)
		}
	}
	if entry != "prod:"+dev || association != "prod#"+dev+"#"+policy {
		t.Errorf("access entry %q, association %q", entry, association)
	}
	// A policy with another scope is not the one the object describes.
	ids.AccessEntries[dev][0].Scope = "namespace"
	for _, obj := range crossplaneaws.ClusterResources(p) {
		if _, isAssociation := obj.(*awseks.AccessPolicyAssociation); isAssociation {
			if _, ok := crossplaneaws.Adopt(obj, ids); ok {
				t.Error("an association with another scope was adopted")
			}
		}
	}

	// The external names found are part of every later render.
	p.ExternalNames = map[string]string{"prod-1a2b3c4d-default": "tf-ng"}
	if ng := byName(crossplaneaws.ClusterResources(p))["prod-1a2b3c4d-default"]; ng.GetAnnotations()[crossplaneaws.ExternalNameAnnotation] != "tf-ng" {
		t.Errorf("node group annotations = %v", ng.GetAnnotations())
	}
}

// A security group something else uses too is never deleted with the
// cluster, and neither are its rules.
func TestSharedSecurityGroupIsRetained(t *testing.T) {
	objects := crossplaneaws.CarriedOver(clusterPlan(), crossplaneaws.CarriedOverResources{
		SecurityGroups: []crossplaneaws.SecurityGroupPlan{{ID: "sg-1", Name: "shared", Shared: true,
			Rules: []crossplaneaws.SecurityGroupRulePlan{{ID: "sgr-1", Protocol: "-1", CIDRIPv4: "10.0.0.0/8"}}}}})
	if len(objects) != 2 {
		t.Fatalf("%d objects, want the group and its rule", len(objects))
	}
	for _, obj := range objects {
		if obj.GetAnnotations()[crossplaneaws.RetainAnnotation] != "true" {
			t.Errorf("%T of a shared group is not retained: %v", obj, obj.GetAnnotations())
		}
	}
}

func TestCarriedOverClusterResources(t *testing.T) {
	objects := crossplaneaws.CarriedOver(clusterPlan(), crossplaneaws.CarriedOverResources{
		RoleNames:         map[string]string{"cluster-role": "tf-cluster", "node-role": "tf-node"},
		ExtraPolicies:     map[string][]string{"node-role": {"arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"}},
		OIDCProviderARN:   "arn:aws:iam::444455556666:oidc-provider/oidc.eks.us-east-1.amazonaws.com/id/ABC",
		OIDCIssuer:        testIssuer,
		LogGroupName:      "/aws/eks/prod/cluster",
		EncryptionKeyARN:  testKeyARN,
		IdentityProviders: []crossplaneaws.IdentityProviderPlan{{Name: "okta", IssuerURL: "https://example.okta.com", ClientID: "k8s"}},
		Addons:            []crossplaneaws.AddonPlan{{Name: "adot", Version: "v1"}},
		PodIdentities:     []crossplaneaws.PodIdentityPlan{{ID: "a-9", Namespace: "apps", ServiceAccount: "api"}},
		SecurityGroups: []crossplaneaws.SecurityGroupPlan{{ID: "sg-1", Name: "tf-cluster", Description: "control plane", VPCID: "vpc-1",
			Rules: []crossplaneaws.SecurityGroupRulePlan{
				{ID: "sgr-1", Protocol: "tcp", FromPort: ptr.To(443), ToPort: ptr.To(443), ReferencedGroupID: "sg-nodes"},
				{ID: "sgr-2", Egress: true, Protocol: "-1", CIDRIPv4: "0.0.0.0/0"}}}},
	})
	want := []string{
		"tf-node/arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore",
		"arn:aws:iam::444455556666:oidc-provider/oidc.eks.us-east-1.amazonaws.com/id/ABC",
		"/aws/eks/prod/cluster", "1234abcd-12ab-34cd-56ef-1234567890ab", "okta", "prod:adot",
		"sg-1", "sgr-1", "sgr-2", "a-9",
	}
	if len(objects) != len(want) {
		t.Fatalf("%d objects, want %d", len(objects), len(want))
	}
	for i, obj := range objects {
		a := obj.GetAnnotations()
		if a[crossplaneaws.ExternalNameAnnotation] != want[i] || a[crossplane.CarriedOverAnnotation] != "true" {
			t.Errorf("object %d (%T) annotations = %v, want external name %s, carried over", i, obj, a, want[i])
		}
		_, isKey := obj.(*awskms.Key)
		_, isAttachment := obj.(*awsiam.RolePolicyAttachment)
		if (a[crossplaneaws.RetainAnnotation] == "true") != (isKey || isAttachment) {
			t.Errorf("%T retained = %q: only the KMS key and the roles' policies are never deleted here", obj, a[crossplaneaws.RetainAnnotation])
		}
	}
	for _, obj := range objects {
		var policies []string
		switch o := obj.(type) {
		case *awscwl.Group:
			if o.Spec.ForProvider.RetentionInDays != nil {
				t.Errorf("log group without retention rendered one: %v", *o.Spec.ForProvider.RetentionInDays)
			}
		case *awsec2.SecurityGroupIngressRule:
			fp := o.Spec.ForProvider
			if *fp.SecurityGroupID != "sg-1" || *fp.IPProtocol != "tcp" || *fp.FromPort != 443 || *fp.ReferencedSecurityGroupID != "sg-nodes" || fp.CidrIPv4 != nil {
				t.Errorf("ingress rule = %+v", fp)
			}
		case *awsec2.SecurityGroupEgressRule:
			if fp := o.Spec.ForProvider; fp.FromPort != nil || *fp.IPProtocol != "-1" || *fp.CidrIPv4 != "0.0.0.0/0" {
				t.Errorf("egress rule = %+v", fp)
			}
		case *awsec2.SecurityGroup:
			if fp := o.Spec.ForProvider; *fp.Name != "tf-cluster" || *fp.VPCID != "vpc-1" || *fp.Description != "control plane" {
				t.Errorf("security group = %+v", fp)
			}
		case *awsiam.OpenIDConnectProvider:
			for _, p := range o.Spec.ManagementPolicies {
				policies = append(policies, string(p))
			}
			if len(policies) != 2 || o.Spec.ForProvider.ThumbprintList != nil {
				t.Errorf("OIDC provider policies %v, thumbprints %v", policies, o.Spec.ForProvider.ThumbprintList)
			}
		}
	}
}

// The cluster and node roles of an imported cluster may be other clusters'
// too: once taken over they and their policy attachments are owned without
// Delete, like the KMS key, so deleting the cluster leaves them in IAM.
// The roles the platform creates itself are deleted with the cluster.
func TestAdoptedRolesAreRetained(t *testing.T) {
	iam := []string{"cluster-role", "cluster-policy", "node-role", "node-worker", "node-cni", "node-ecr"}
	adopted := clusterPlan()
	adopted.ClusterRoleName, adopted.NodeRoleName = "tf-cluster", "tf-node"
	for name, tc := range map[string]struct {
		p    crossplaneaws.ClusterPlan
		keep bool
	}{"adopted": {adopted, true}, "created": {clusterPlan(), false}} {
		objects := byName(crossplaneaws.ClusterResources(tc.p))
		for _, suffix := range iam {
			obj := objects["prod-1a2b3c4d-"+suffix]
			if obj == nil {
				t.Fatalf("%s: no %s", name, suffix)
			}
			if got := obj.GetAnnotations()[crossplaneaws.RetainAnnotation] == "true"; got != tc.keep {
				t.Errorf("%s: %s retained = %v, want %v", name, suffix, got, tc.keep)
			}
			a, ok := obj.(*awsiam.RolePolicyAttachment)
			if ok && tc.keep && !slices.Equal(a.Spec.ManagementPolicies, crossplaneaws.PolicyNoDelete) {
				t.Errorf("%s: %s policies = %v, want without Delete", name, suffix, a.Spec.ManagementPolicies)
			}
		}
	}
	// The other policies the import found on the roles too.
	for _, obj := range crossplaneaws.CarriedOver(adopted, crossplaneaws.CarriedOverResources{
		RoleNames:     map[string]string{"cluster-role": "tf-cluster", "node-role": "tf-node"},
		ExtraPolicies: map[string][]string{"node-role": {"arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"}},
	}) {
		if obj.GetAnnotations()[crossplaneaws.RetainAnnotation] != "true" {
			t.Errorf("carried-over %s is not retained", obj.GetName())
		}
	}
}

// A cluster imported before the carried-over policy attachments were
// retained has them stored without RetainAnnotation. Owned at takeover,
// they are still kept: every one is an extra policy on a role the import
// adopted, which other clusters may use too.
func TestOwnCarriedOverKeepsAttachmentsStoredBeforeTheyWereRetained(t *testing.T) {
	objects := crossplaneaws.CarriedOver(clusterPlan(), crossplaneaws.CarriedOverResources{
		RoleNames:     map[string]string{"cluster-role": "tf-cluster", "node-role": "tf-node"},
		ExtraPolicies: map[string][]string{"node-role": {"arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"}},
		LogGroupName:  "/aws/eks/prod/cluster",
	})
	for _, obj := range objects {
		a := obj.GetAnnotations()
		delete(a, crossplaneaws.RetainAnnotation) // as stored before the fix
		obj.SetAnnotations(a)

		crossplaneaws.OwnCarriedOver(obj)

		got := policies(t, obj)
		_, attachment := obj.(*awsiam.RolePolicyAttachment)
		want := crossplaneaws.PolicyAll
		if attachment {
			want = crossplaneaws.PolicyNoDelete
		}
		if !slices.Equal(got, want) {
			t.Errorf("%T %s: policies %v, want %v", obj, obj.GetName(), got, want)
		}
		if attachment && obj.GetAnnotations()[crossplaneaws.RetainAnnotation] != "true" {
			t.Errorf("attachment %s is not marked retained: %v", obj.GetName(), obj.GetAnnotations())
		}
	}
}

func policies(t *testing.T, obj client.Object) xpv2.ManagementPolicies {
	t.Helper()
	m, ok := obj.(interface {
		GetManagementPolicies() xpv2.ManagementPolicies
	})
	if !ok {
		t.Fatalf("%T is not a managed resource", obj)
	}
	return m.GetManagementPolicies()
}
