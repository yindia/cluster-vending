package network

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	aws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/fault"
	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// RenderResources is shared by preview and reconciliation. Imported networks
// always render from discovery, never as a fresh network with a new cloud ID.
func (s *Reconciler) RenderResources(ctx context.Context, row *model.Network, spec *v1.NetworkSpec) ([]client.Object, error) {
	if StoredImport(row) != nil {
		resources, found, err := importedResources(row.Input)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fault.New(fault.FailedPrecondition, "the imported network has no recorded discovery; cannot safely plan its update")
		}
		return aws.OwnedNetworkResources(*resources, row.Labels), nil
	}
	account, layout, err := s.Plan(ctx, row.TenantID, spec)
	if err != nil {
		return nil, err
	}
	return aws.NetworkResources(RenderPlan(row, spec, account, layout))
}
