package aws

import (
	"encoding/json"
	"fmt"
	"strings"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DefaultAddons are the EKS add-ons every cluster gets, in install order.
var DefaultAddons = []string{"vpc-cni", "coredns", "kube-proxy", "eks-pod-identity-agent", "aws-ebs-csi-driver"}

// Add-on names with platform setup.
const (
	AddonEBSCSI       = "aws-ebs-csi-driver"
	AddonEFSCSI       = "aws-efs-csi-driver"
	AddonFSxCSI       = "aws-fsx-csi-driver"
	AddonMountpointS3 = "aws-mountpoint-s3-csi-driver"
)

// AddonPlan is one add-on resolved to concrete values.
type AddonPlan struct {
	Name string
	// Version is the addonVersion; empty takes the EKS default.
	Version             string
	ConfigurationValues string
	// S3BucketNames: aws-mountpoint-s3-csi-driver's buckets (its policy).
	S3BucketNames []string
}

// addonSetup is what the platform creates for an add-on that calls AWS: a
// role for its service account (EKS Pod Identity) with policies. Add-ons
// without an entry need nothing (coredns, kube-proxy, the pod identity
// agent, snapshot-controller) or have their policy on the node role
// (vpc-cni). From
// https://docs.aws.amazon.com/eks/latest/userguide/workloads-add-ons-available-eks.html.
type addonSetup struct {
	// short names the IAM objects, "<prefix>-<short>-role": at most 8
	// characters so the role name stays within IAM's 64.
	short                     string
	namespace, serviceAccount string
	managedPolicies           []string
	// s3Policy: write a policy for the add-on's S3 buckets.
	s3Policy bool
}

var addonSetups = map[string]addonSetup{
	AddonEBSCSI: {short: "ebs-csi", namespace: "kube-system", serviceAccount: "ebs-csi-controller-sa",
		managedPolicies: []string{policyEBSCSI}},
	AddonEFSCSI: {short: "efs-csi", namespace: "kube-system", serviceAccount: "efs-csi-controller-sa",
		managedPolicies: []string{"arn:aws:iam::aws:policy/service-role/AmazonEFSCSIDriverPolicy"}},
	AddonFSxCSI: {short: "fsx-csi", namespace: "kube-system", serviceAccount: "fsx-csi-controller-sa",
		managedPolicies: []string{"arn:aws:iam::aws:policy/AmazonFSxFullAccess"}},
	AddonMountpointS3: {short: "s3-csi", namespace: "kube-system", serviceAccount: "s3-csi-driver-sa", s3Policy: true},
}

// DefaultAddonPlans are the default add-ons with no pinned versions.
func DefaultAddonPlans() []AddonPlan {
	out := make([]AddonPlan, 0, len(DefaultAddons))
	for _, name := range DefaultAddons {
		out = append(out, AddonPlan{Name: name})
	}
	return out
}

func (b clusterBuilder) addons() []client.Object {
	plans := b.p.Addons
	if len(plans) == 0 {
		plans = DefaultAddonPlans()
	}
	out := make([]client.Object, 0, len(plans)*4)
	for _, a := range plans {
		out = append(out, b.addon(a))
	}
	for _, a := range plans {
		if setup, ok := addonSetups[a.Name]; ok {
			out = append(out, b.addonSetup(a, setup)...)
		}
	}
	return out
}

func (b clusterBuilder) addon(a AddonPlan) *awseks.Addon {
	name := b.name("addon-" + a.Name)
	params := awseks.AddonParameters{
		Region: ptr.To(b.p.Region), ClusterNameRef: b.ref(b.p.Prefix), AddonName: ptr.To(a.Name),
		ResolveConflictsOnCreate: ptr.To("OVERWRITE"), ResolveConflictsOnUpdate: ptr.To("OVERWRITE"),
		Tags: b.tags(name),
	}
	if a.Version != "" {
		params.AddonVersion = ptr.To(a.Version)
	}
	if a.ConfigurationValues != "" {
		params.ConfigurationValues = ptr.To(a.ConfigurationValues)
	}
	return &awseks.Addon{ObjectMeta: b.meta(name), Spec: awseks.AddonSpec{ManagedResourceSpec: b.spec(), ForProvider: params}}
}

// addonSetup is the add-on's role "<prefix>-<short>-role", its policies
// (managed, or written: "<prefix>-<short>-s3"), their attachments and the
// pod identity association of its service account.
func (b clusterBuilder) addonSetup(a AddonPlan, s addonSetup) []client.Object {
	role := s.short + "-role"
	out := []client.Object{b.role(role, trustPodIdentity)}
	for i, policy := range s.managedPolicies {
		suffix := s.short + "-policy"
		if i > 0 {
			suffix = fmt.Sprintf("%s-%d", suffix, i+1)
		}
		out = append(out, b.attach(suffix, role, policy))
	}
	if s.s3Policy {
		policy := b.name(s.short + "-s3")
		out = append(out, b.s3Policy(policy, a.S3BucketNames), &awsiam.RolePolicyAttachment{
			ObjectMeta: b.meta(b.name(s.short + "-policy")),
			Spec: awsiam.RolePolicyAttachmentSpec{ManagedResourceSpec: b.spec(), ForProvider: awsiam.RolePolicyAttachmentParameters{
				PolicyArnRef: b.ref(policy), RoleRef: b.ref(b.name(role)),
			}},
		})
	}
	name := b.name(s.short + "-identity")
	return append(out, &awseks.PodIdentityAssociation{
		ObjectMeta: b.meta(name),
		Spec: awseks.PodIdentityAssociationSpec{ManagedResourceSpec: b.spec(), ForProvider: awseks.PodIdentityAssociationParameters_2{
			Region: ptr.To(b.p.Region), ClusterNameRef: b.ref(b.p.Prefix), RoleArnRef: b.ref(b.name(role)),
			Namespace: ptr.To(s.namespace), ServiceAccount: ptr.To(s.serviceAccount), Tags: b.tags(name),
		}},
	})
}

// s3Policy is Mountpoint's recommended policy for the buckets: list them,
// read, write and delete their objects
// (https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md#iam-permissions).
func (b clusterBuilder) s3Policy(name string, buckets []string) *awsiam.Policy {
	partition := Partition(b.p.Region)
	bucketARNs := make([]string, 0, len(buckets))
	objectARNs := make([]string, 0, len(buckets))
	for _, bucket := range buckets {
		arn := "arn:" + partition + ":s3:::" + bucket
		bucketARNs = append(bucketARNs, arn)
		objectARNs = append(objectARNs, arn+"/*")
	}
	doc, _ := json.Marshal(map[string]any{ // maps and strings: cannot fail
		"Version": "2012-10-17",
		"Statement": []map[string]any{
			{"Sid": "MountpointBucketAccess", "Effect": "Allow", "Action": []string{"s3:ListBucket"}, "Resource": bucketARNs},
			{"Sid": "MountpointObjectAccess", "Effect": "Allow",
				"Action":   []string{"s3:GetObject", "s3:PutObject", "s3:AbortMultipartUpload", "s3:DeleteObject"},
				"Resource": objectARNs},
		},
	})
	return &awsiam.Policy{
		ObjectMeta: b.meta(name),
		Spec: awsiam.PolicySpec{ManagedResourceSpec: b.spec(), ForProvider: awsiam.PolicyParameters{
			Policy: ptr.To(string(doc)), Description: ptr.To("Mountpoint for Amazon S3 CSI driver of EKS cluster " + b.p.ClusterName),
			Tags: b.tags(name),
		}},
	}
}

// Partition is the AWS partition of a region: aws, aws-us-gov or aws-cn.
func Partition(region string) string {
	switch {
	case strings.HasPrefix(region, "us-gov-"):
		return "aws-us-gov"
	case strings.HasPrefix(region, "cn-"):
		return "aws-cn"
	default:
		return "aws"
	}
}
