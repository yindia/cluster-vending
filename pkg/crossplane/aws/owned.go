package aws

import (
	"slices"
	"strings"

	awscwl "github.com/upbound/provider-aws/v2/apis/namespaced/cloudwatchlogs/v1beta1"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
)

// LabelTagsAnnotation, on an object of a taken-over network, lists the tag
// keys that are the network's labels (comma-separated, sorted): the tags
// the platform set. Every other tag came with the import.
const LabelTagsAnnotation = "cluster-vending.io/label-tags"

// Own turns an imported object (it only observes) into the object as the
// platform owns it after a takeover: full management, or everything but
// Delete for one whose cloud resource others use too (RetainAnnotation).
// Nothing else changes, so applying it over the imported object changes
// the management policies and nothing in the cloud. It reports whether
// obj is a managed resource.
func Own(obj client.Object) bool {
	m, ok := obj.(managed)
	if !ok {
		return false
	}
	if obj.GetAnnotations()[RetainAnnotation] == "true" {
		m.SetManagementPolicies(slices.Clone(PolicyNoDelete))
		return true
	}
	m.SetManagementPolicies(slices.Clone(PolicyAll))
	return true
}

// OwnedNetworkResources renders n as the managed resources of a network
// that was taken over: ImportedNetworkResources, each object owned (Own),
// with what a takeover adds and nothing else:
//
//   - subnets carry the load balancer role tag of the platform's subnets
//     (TagELB on public ones, TagInternalELB on the others);
//   - labels (the network's labels) are tags of every object that takes
//     tags, over a tag of the same key the import had. LabelTagsAnnotation
//     lists their keys. An object shared with other networks
//     (RetainAnnotation) is not the network's to tag.
//
// The render is complete: applied again with other labels, a label that
// is gone is no tag any more (a tag of that key the import had is back),
// and the tags the import had are as they were.
func OwnedNetworkResources(n ImportedNetwork, labels map[string]string) []client.Object {
	objects := ImportedNetworkResources(n)
	for _, obj := range objects {
		Own(obj)
		if s, ok := obj.(*awsec2.Subnet); ok {
			tag := TagInternalELB
			if s.Labels[crossplane.LabelSubnetRole] == string(SubnetPublic) {
				tag = TagELB
			}
			setTags(obj, map[string]string{tag: "1"})
		}
		if len(labels) == 0 || obj.GetAnnotations()[RetainAnnotation] == "true" || !setTags(obj, labels) {
			continue
		}
		keys := make([]string, 0, len(labels))
		for k := range labels {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		annotations := obj.GetAnnotations()
		annotations[LabelTagsAnnotation] = strings.Join(keys, ",")
		obj.SetAnnotations(annotations)
	}
	return objects
}

// setTags adds tags to the tags of obj and reports whether obj is of a
// kind of a network's resources that takes tags.
func setTags(obj client.Object, tags map[string]string) bool {
	have := tagsOf(obj)
	if have == nil {
		return false
	}
	if *have == nil {
		*have = make(map[string]*string, len(tags))
	}
	for k, v := range tags {
		(*have)[k] = ptr.To(v)
	}
	return true
}

// tagsOf is the tags field of a network's managed resource; nil for a
// kind without tags (routes, associations, network ACL rules).
func tagsOf(obj client.Object) *map[string]*string {
	switch o := obj.(type) {
	case *awsec2.VPC:
		return &o.Spec.ForProvider.Tags
	case *awsec2.InternetGateway:
		return &o.Spec.ForProvider.Tags
	case *awsec2.Subnet:
		return &o.Spec.ForProvider.Tags
	case *awsec2.EIP:
		return &o.Spec.ForProvider.Tags
	case *awsec2.NATGateway:
		return &o.Spec.ForProvider.Tags
	case *awsec2.RouteTable:
		return &o.Spec.ForProvider.Tags
	case *awsec2.FlowLog:
		return &o.Spec.ForProvider.Tags
	case *awsec2.NetworkACL:
		return &o.Spec.ForProvider.Tags
	case *awsec2.DefaultNetworkACL:
		return &o.Spec.ForProvider.Tags
	case *awscwl.Group:
		return &o.Spec.ForProvider.Tags
	case *awsiam.Role:
		return &o.Spec.ForProvider.Tags
	default:
		return nil
	}
}
