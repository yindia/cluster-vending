package network

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/fault"
)

// OwnAll switches every recorded object of a taken-over network to full
// management. It is safe to repeat.
func (s *Reconciler) OwnAll(ctx context.Context, row *model.Network) error {
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerNetwork, row.ID)
	if err != nil {
		return err
	}
	for _, r := range records {
		if err := s.own(ctx, r); err != nil {
			s.logger().ErrorContext(ctx, "take over resource", "network_id", row.ID, "object", r.Name, "error", err)
			return fault.New(fault.Internal, fmt.Sprintf("could not take over %s %s", r.Kind, r.Name))
		}
	}
	return nil
}

// own switches one recorded object to full management (without Delete for
// one marked crossplaneaws.RetainAnnotation) and adds the ELB role tags the
// platform's subnets carry.
func (s *Reconciler) own(ctx context.Context, r model.ManagedResource) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.FromAPIVersionAndKind(r.APIVersion, r.Kind))
	// From the API server: a read-modify-write needs the current object.
	if err := s.deps.Reader().Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: r.Name}, u); err != nil {
		return err
	}
	policies := []string{"*"}
	if u.GetAnnotations()[crossplaneaws.RetainAnnotation] == "true" {
		// Shared with resources outside the network: owned, never deleted.
		policies = policies[:0]
		for _, action := range crossplaneaws.PolicyNoDelete {
			policies = append(policies, string(action))
		}
	}
	if err := unstructured.SetNestedStringSlice(u.Object, policies, "spec", "managementPolicies"); err != nil {
		return err
	}
	if r.Kind == "Subnet" {
		tag := crossplaneaws.TagInternalELB
		if u.GetLabels()[crossplane.LabelSubnetRole] == string(crossplaneaws.SubnetPublic) {
			tag = crossplaneaws.TagELB
		}
		if err := unstructured.SetNestedField(u.Object, "1", "spec", "forProvider", "tags", tag); err != nil {
			return err
		}
	}
	return s.deps.Kube.Update(ctx, u)
}
