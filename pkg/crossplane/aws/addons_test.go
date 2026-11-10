package aws_test

import (
	"strings"
	"testing"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

func TestAddonsAndTheirSetup(t *testing.T) {
	p := clusterPlan()
	p.AdminPrincipals = nil
	p.Addons = []crossplaneaws.AddonPlan{
		{Name: "vpc-cni", Version: "v1.20.0-eksbuild.1", ConfigurationValues: `{"env":{"ENABLE_PREFIX_DELEGATION":"true"}}`},
		{Name: "coredns"},
		{Name: "aws-efs-csi-driver"},
		{Name: "aws-mountpoint-s3-csi-driver", S3BucketNames: []string{"data", "logs"}},
		{Name: "snapshot-controller"},
		{Name: "cert-manager"},
	}
	byName := map[string]any{}
	for _, o := range crossplaneaws.ClusterResources(p) {
		byName[o.GetName()] = o
	}

	cni := byName["prod-1a2b3c4d-addon-vpc-cni"].(*awseks.Addon)
	if *cni.Spec.ForProvider.AddonVersion != "v1.20.0-eksbuild.1" || !strings.Contains(*cni.Spec.ForProvider.ConfigurationValues, "PREFIX") {
		t.Errorf("vpc-cni = %+v", cni.Spec.ForProvider)
	}
	if byName["prod-1a2b3c4d-addon-coredns"].(*awseks.Addon).Spec.ForProvider.AddonVersion != nil {
		t.Error("coredns without a version must leave addonVersion to EKS")
	}

	// EFS: role, managed policy, pod identity for efs-csi-controller-sa.
	if _, ok := byName["prod-1a2b3c4d-efs-csi-role"].(*awsiam.Role); !ok {
		t.Error("no EFS CSI role")
	}
	efsPolicy := byName["prod-1a2b3c4d-efs-csi-policy"].(*awsiam.RolePolicyAttachment)
	if !strings.HasSuffix(*efsPolicy.Spec.ForProvider.PolicyArn, "/AmazonEFSCSIDriverPolicy") {
		t.Errorf("EFS policy = %s", *efsPolicy.Spec.ForProvider.PolicyArn)
	}
	efsID := byName["prod-1a2b3c4d-efs-csi-identity"].(*awseks.PodIdentityAssociation)
	if *efsID.Spec.ForProvider.ServiceAccount != "efs-csi-controller-sa" || efsID.Spec.ForProvider.RoleArnRef.Name != "prod-1a2b3c4d-efs-csi-role" {
		t.Errorf("EFS identity = %+v", efsID.Spec.ForProvider)
	}

	// Mountpoint: a written policy for the buckets, attached by reference.
	s3 := byName["prod-1a2b3c4d-s3-csi-s3"].(*awsiam.Policy)
	doc := *s3.Spec.ForProvider.Policy
	for _, want := range []string{`"arn:aws:s3:::data"`, `"arn:aws:s3:::logs/*"`, `"s3:ListBucket"`, `"s3:PutObject"`} {
		if !strings.Contains(doc, want) {
			t.Errorf("mountpoint policy %s lacks %s", doc, want)
		}
	}
	att := byName["prod-1a2b3c4d-s3-csi-policy"].(*awsiam.RolePolicyAttachment)
	if att.Spec.ForProvider.PolicyArnRef.Name != "prod-1a2b3c4d-s3-csi-s3" || att.Spec.ForProvider.RoleRef.Name != "prod-1a2b3c4d-s3-csi-role" {
		t.Errorf("mountpoint attachment = %+v", att.Spec.ForProvider)
	}

	// No setup for add-ons that make no AWS calls.
	for name := range byName {
		if strings.Contains(name, "snapshot") && !strings.HasSuffix(name, "addon-snapshot-controller") ||
			strings.Contains(name, "cert-manager") && !strings.HasSuffix(name, "addon-cert-manager") {
			t.Errorf("unexpected setup object %s", name)
		}
	}
	// EBS is not in the plan's add-ons: no EBS role.
	if _, ok := byName["prod-1a2b3c4d-ebs-csi-role"]; ok {
		t.Error("EBS role without the EBS add-on")
	}
}

func TestPartition(t *testing.T) {
	for region, want := range map[string]string{"us-east-1": "aws", "us-gov-west-1": "aws-us-gov", "cn-north-1": "aws-cn"} {
		if got := crossplaneaws.Partition(region); got != want {
			t.Errorf("Partition(%s) = %s, want %s", region, got, want)
		}
	}
}
