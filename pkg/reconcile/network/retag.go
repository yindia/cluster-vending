package network

import (
	"context"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/fault"
)

// taggable are the kinds of a network's resources that take tags.
var taggable = map[string]bool{
	"VPC": true, "InternetGateway": true, "Subnet": true, "EIP": true, "NATGateway": true, "RouteTable": true,
	"FlowLog": true, "Group": true, "Role": true, "NetworkACL": true, "DefaultNetworkACL": true,
}

// labelTagsAnnotation, on an object of an imported network, lists the tag
// keys that are the network's labels (comma-separated): what retag set and
// may remove again. Every other tag came with the import and is not the
// platform's to touch.
const labelTagsAnnotation = "cluster-vending.io/label-tags"

// Retag sets an imported (taken-over) network's labels as tags on the
// objects it has, and marks the generation applied once all are done. The
// labels are added or changed, tags that were labels before and are not
// any more (labelTagsAnnotation) are removed, and every other tag is left
// alone. Objects shared with other networks (a log group or role other
// VPCs use: crossplaneaws.RetainAnnotation) are not the network's to tag.
// The network is not rendered again. It is safe to repeat: the loop calls
// it again after a failure, until every object carries the labels.
func (s *Reconciler) Retag(ctx context.Context, row *model.Network) error {
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerNetwork, row.ID)
	if err != nil {
		return err
	}
	for _, r := range records {
		if !taggable[r.Kind] {
			continue
		}
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(schema.FromAPIVersionAndKind(r.APIVersion, r.Kind))
		// From the API server: a read-modify-write needs the current object.
		if err := s.deps.Reader().Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: r.Name}, u); err != nil {
			if client.IgnoreNotFound(err) == nil {
				continue
			}
			return fault.New(fault.Unavailable, "cannot read "+r.Kind+" "+r.Name+" from the management cluster")
		}
		if u.GetAnnotations()[crossplaneaws.RetainAnnotation] == "true" || !setLabelTags(u, row.Labels) {
			continue
		}
		if err := s.deps.Kube.Update(ctx, u); err != nil {
			s.logger().ErrorContext(ctx, "tag imported network resource", "network_id", row.ID, "object", r.Name, "error", err)
			return fault.New(fault.Internal, "could not tag "+r.Kind+" "+r.Name)
		}
	}
	return s.MarkApplied(ctx, row)
}

// setLabelTags makes labels the label tags of u and reports whether u
// changed.
func setLabelTags(u *unstructured.Unstructured, labels map[string]string) bool {
	tags, _, _ := unstructured.NestedStringMap(u.Object, "spec", "forProvider", "tags")
	if tags == nil {
		tags = map[string]string{}
	}
	annotations := u.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	changed := false
	for _, k := range strings.Split(annotations[labelTagsAnnotation], ",") {
		if _, kept := labels[k]; !kept && k != "" {
			if _, has := tags[k]; has {
				delete(tags, k)
				changed = true
			}
		}
	}
	keys := make([]string, 0, len(labels))
	for k, v := range labels {
		keys = append(keys, k)
		if tags[k] != v {
			tags[k] = v
			changed = true
		}
	}
	slices.Sort(keys)
	if want := strings.Join(keys, ","); annotations[labelTagsAnnotation] != want {
		annotations[labelTagsAnnotation] = want
		changed = true
	}
	if !changed {
		return false
	}
	u.SetAnnotations(annotations)
	_ = unstructured.SetNestedStringMap(u.Object, tags, "spec", "forProvider", "tags") // a string map: cannot fail
	return true
}
