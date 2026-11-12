package aws_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

func clusterPlan() crossplaneaws.ClusterPlan {
	return crossplaneaws.ClusterPlan{
		Namespace: "tenant-acme", ProviderConfigName: "aws-1", TenantID: "acme",
		ClusterID: "c-1", ClusterName: "prod", Prefix: "prod-1a2b3c4d", Region: "us-east-1",
		KubernetesVersion: "1.31", SubnetIDs: []string{"subnet-a", "subnet-b"},
		PublicAccessCIDRs: []string{"0.0.0.0/0"}, AuthenticationMode: "API", Tags: map[string]string{"team": "x"},
		NodePool: crossplaneaws.NodePoolPlan{InstanceTypes: []string{"m6i.large"}, CapacityType: "ON_DEMAND",
			AMIType: "AL2023_x86_64_STANDARD", MinSize: 1, MaxSize: 3, Desired: 2,
			Taints: []crossplaneaws.TaintPlan{{Key: "gpu", Value: "true", Effect: "NO_SCHEDULE"}}},
		AdminPrincipals: []string{"arn:aws:iam::444455556666:role/ops"},
	}
}

func TestClusterResources(t *testing.T) {
	objects := crossplaneaws.ClusterResources(clusterPlan())
	// 3 roles + 5 attachments, cluster, node group, 5 add-ons, pod identity,
	// cluster auth, the helm ProviderConfig, access entry + policy
	// association, 12 Usages.
	if len(objects) != 32 {
		t.Fatalf("got %d objects, want 32", len(objects))
	}
	byName := map[string]any{}
	for _, o := range objects {
		byName[o.GetName()] = o
		if o.GetLabels()["cluster-vending.io/cluster"] != "c-1" || o.GetNamespace() != "tenant-acme" {
			t.Errorf("%s: labels %v namespace %q", o.GetName(), o.GetLabels(), o.GetNamespace())
		}
	}
	eks, ok := byName["prod-1a2b3c4d"].(*awseks.Cluster)
	if !ok {
		t.Fatal("no EKS cluster object named after the prefix")
	}
	if got := eks.GetAnnotations()[crossplaneaws.ExternalNameAnnotation]; got != "prod" {
		t.Errorf("EKS cluster external name = %q, want the client's name %q", got, "prod")
	}
	fp := eks.Spec.ForProvider
	if *fp.Version != "1.31" || len(fp.VPCConfig.SubnetIds) != 2 || !*fp.VPCConfig.EndpointPublicAccess ||
		!*fp.AccessConfig.BootstrapClusterCreatorAdminPermissions || fp.RoleArnRef.Name != "prod-1a2b3c4d-cluster-role" {
		t.Errorf("eks cluster = %+v", fp)
	}
	auth, ok := byName["prod-1a2b3c4d-auth"].(*awseks.ClusterAuth)
	if !ok || auth.Spec.WriteConnectionSecretToReference.Name != "prod-1a2b3c4d-kubeconfig" ||
		auth.Spec.ForProvider.ClusterNameRef.Name != "prod-1a2b3c4d" {
		t.Errorf("cluster auth = %+v", auth)
	}
	ng := byName["prod-1a2b3c4d-default"].(*awseks.NodeGroup)
	if *ng.Spec.ForProvider.ScalingConfig.DesiredSize != 2 ||
		*ng.Spec.ForProvider.ScalingConfig.MaxSize != 3 || *ng.Spec.ForProvider.Taint[0].Effect != "NO_SCHEDULE" {
		t.Errorf("node group = %+v", ng.Spec.ForProvider.ScalingConfig)
	}
	role := byName["prod-1a2b3c4d-ebs-csi-role"].(*awsiam.Role)
	if *role.Spec.ForProvider.Tags["cluster-vending.io/cluster"] != "c-1" {
		t.Errorf("role tags = %v", role.Spec.ForProvider.Tags)
	}
	if _, ok := byName["prod-1a2b3c4d-addon-aws-ebs-csi-driver"].(*awseks.Addon); !ok {
		t.Error("missing the EBS CSI add-on")
	}
	if _, ok := byName["prod-1a2b3c4d-admin-1-policy"].(*awseks.AccessPolicyAssociation); !ok {
		t.Error("missing the admin access policy association")
	}
}

// The options added since (endpoint access, log group, encryption, access
// entries, IRSA, security groups, launch template, role names) default to
// what a cluster rendered before they existed: this is the sha256 of that
// render, taken before they were added. A change here needs a
// RenderVersion bump (render version 2 renamed the EKS cluster's Usages;
// 3 added the helm ProviderConfig and its Usage).
func TestClusterResourcesDefaultRenderUnchanged(t *testing.T) {
	raw, err := json.Marshal(crossplaneaws.ClusterResources(clusterPlan()))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != "56af08b3ed3caed9e16e5bd15541c071eafb0e1b1a7b4151c221ab427d46b0d8" {
		t.Errorf("the default render changed (sha256 %s, %d bytes)", got, len(raw))
	}
}
