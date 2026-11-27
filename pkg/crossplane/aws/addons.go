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

// AddonPlan is one add-on resolved to concrete values.
type AddonPlan struct {
	Name string
	// Version is the addonVersion; empty takes the EKS default.
	Version             string
	ConfigurationValues string
	// S3BucketNames: the buckets of an add-on whose setup has the
	// s3-bucket-policy hook (aws-mountpoint-s3-csi-driver): its policy.
	S3BucketNames []string
}

// addonSetup is what the platform creates for an add-on that calls AWS
// (setup in addons.yaml): a role for its service account (EKS Pod
// Identity) with policies. Add-ons without one need nothing (coredns,
// kube-proxy, the pod identity agent, snapshot-controller) or have their
// policy on the node role (vpc-cni).
type addonSetup struct {
	// short names the IAM objects, "<prefix>-<short>-role": at most 8
	// characters so the role name stays within IAM's 64.
	short                     string
	namespace, serviceAccount string
	managedPolicies           []string
	// hook names platform code adding objects (addonHooks).
	hook string
	// protectIAM: Usages keep the role, its managed policy attachments and
	// the pod identity association until the add-on is gone.
	protectIAM bool
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
		if setup, ok := addonCatalog.setups[a.Name]; ok {
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
	if values := b.addonConfiguration(a); values != "" {
		params.ConfigurationValues = ptr.To(values)
	}
	return &awseks.Addon{ObjectMeta: b.meta(name), Spec: awseks.AddonSpec{ManagedResourceSpec: b.spec(), ForProvider: params}}
}

// addonSetup is the add-on's role "<prefix>-<short>-role", its managed
// policies' attachments, its hook's objects and the pod identity
// association of its service account.
func (b clusterBuilder) addonSetup(a AddonPlan, s addonSetup) []client.Object {
	role := s.short + "-role"
	out := []client.Object{b.role(role, trustPodIdentity)}
	for i, policy := range s.managedPolicies {
		out = append(out, b.attach(s.attachmentSuffix(i), role, policy))
	}
	if s.hook != "" {
		out = append(out, addonHooks[s.hook].objects(b, a, s, role)...)
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

// attachmentSuffix names the attachment of the i-th managed policy:
// "<short>-policy", then "<short>-policy-2" and on.
func (s addonSetup) attachmentSuffix(i int) string {
	suffix := s.short + "-policy"
	if i > 0 {
		suffix = fmt.Sprintf("%s-%d", suffix, i+1)
	}
	return suffix
}

// s3BucketPolicyObjects is the s3-bucket-policy hook: the policy
// "<prefix>-<short>-s3" for the add-on's buckets and its attachment
// "<prefix>-<short>-policy" to the role.
func (b clusterBuilder) s3BucketPolicyObjects(a AddonPlan, s addonSetup, role string) []client.Object {
	policy := b.name(s.short + "-s3")
	return []client.Object{b.s3Policy(policy, a.S3BucketNames), &awsiam.RolePolicyAttachment{
		ObjectMeta: b.meta(b.name(s.short + "-policy")),
		Spec: awsiam.RolePolicyAttachmentSpec{ManagedResourceSpec: b.spec(), ForProvider: awsiam.RolePolicyAttachmentParameters{
			PolicyArnRef: b.ref(policy), RoleRef: b.ref(b.name(role)),
		}},
	}}
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
