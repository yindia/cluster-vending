package network

import (
	"context"

	"connectrpc.com/connect/v2"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// taggable are the kinds of a network's resources that take tags.
var taggable = map[string]bool{
	"VPC": true, "InternetGateway": true, "Subnet": true, "EIP": true, "NATGateway": true, "RouteTable": true,
	"FlowLog": true, "Group": true, "Role": true, "NetworkACL": true, "DefaultNetworkACL": true,
}

// retag sets an imported (taken-over) network's labels as tags on the
// objects it has: the labels are added or changed, labels removed since
// before are removed, and every other tag (the ones the resources were
// imported with) is left alone. The network is not rendered again.
func (s *NetworkService) retag(ctx context.Context, row *model.Network, before model.Labels) error {
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerNetwork, row.ID)
	if err != nil {
		return service.ToConnect(err)
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
			return connect.NewError(connect.CodeUnavailable, "cannot read "+r.Kind+" "+r.Name+" from the management cluster")
		}
		tags, _, _ := unstructured.NestedStringMap(u.Object, "spec", "forProvider", "tags")
		if tags == nil {
			tags = map[string]string{}
		}
		for k := range before {
			if _, kept := row.Labels[k]; !kept {
				delete(tags, k)
			}
		}
		for k, v := range row.Labels {
			tags[k] = v
		}
		if err := unstructured.SetNestedStringMap(u.Object, tags, "spec", "forProvider", "tags"); err != nil {
			return connect.NewError(connect.CodeInternal, err.Error())
		}
		if err := s.deps.Kube.Update(ctx, u); err != nil {
			s.logger().ErrorContext(ctx, "tag imported network resource", "network_id", row.ID, "object", r.Name, "error", err)
			return connect.NewError(connect.CodeInternal, "could not tag "+r.Kind+" "+r.Name)
		}
	}
	return s.markApplied(ctx, row)
}
