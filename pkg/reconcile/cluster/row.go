package cluster

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	corecluster "github.com/yindia/cluster-vending/pkg/cluster"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// supported rejects what this version cannot build.
func supported(spec *cloudv1.ClusterSpec) error {
	switch {
	case spec.GetAws() == nil:
		return fault.New(fault.Unimplemented, "only aws clusters are supported")
	case spec.GetExistingCluster() != nil:
		return fault.New(fault.Unimplemented,
			"spec.existing_cluster is not implemented; import a cluster with ImportCluster and TakeOverCluster")
	}
	return nil
}

// NewRow checks a create request (the contract's rules, what this version
// can build, the labels, the network and its subnets, the name among the
// platform's own clusters) and returns the row CreateCluster stores for
// it, not stored yet: the cluster as sent (input) and with its defaults
// (spec), PROVISIONING, its generation neither applied nor resolved yet.
// The ID is chosen here, it names the resources. The API handler and the
// controller (for a Cluster object that was created without the API) both
// build their row here.
//
// It does not look at the cloud. Whether EKS already has a cluster of the
// name, and which Kubernetes and add-on versions EKS offers, is found out
// by Reconcile before anything is rendered (resolve).
func (s *Reconciler) NewRow(ctx context.Context, req *cloudv1.CreateClusterRequest) (*model.Cluster, error) {
	if err := reconcile.Validate(req); err != nil {
		return nil, err
	}
	in := req.GetCluster()
	if err := supported(in.GetSpec()); err != nil {
		return nil, err
	}
	if err := reconcile.CheckAWSLabels(in.GetLabels()); err != nil {
		return nil, err
	}
	t, err := s.Target(ctx, req.GetTenantId(), in.GetSpec())
	if err != nil {
		return nil, err
	}
	if err := s.NameTaken(ctx, req.GetTenantId(), in.GetName(), in.GetSpec().GetRegion(), t.Account); err != nil {
		return nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fault.New(fault.Internal, err.Error())
	}
	spec, err := corecluster.ResolveSpec(in.GetSpec(), t.Zones)
	if err != nil {
		return nil, fault.New(fault.Invalid, err.Error())
	}
	input, err1 := Encode(in)
	raw, err2 := Encode(spec)
	if err := errors.Join(err1, err2); err != nil {
		return nil, fault.New(fault.Internal, err.Error())
	}
	row := &model.Cluster{
		Resource: model.Resource{Base: model.Base{ID: id.String()}, DisplayName: in.GetDisplayName(),
			Labels: in.GetLabels(), State: StateProvisioning, Spec: raw},
		TenantID: req.GetTenantId(), Name: in.GetName(), Input: input, NetworkID: t.Network.ID,
		Provider: model.ProviderAWS, Region: spec.GetRegion(),
	}
	if rid := req.GetRequestId(); rid != "" {
		row.CreateRequestID = &rid
	}
	return row, nil
}

// NameTaken fails with an AlreadyExists fault when a cluster of the
// tenant already has the EKS name in the account and region. It reads the
// store only: clusters made outside the platform are found by Reconcile
// (resolve).
func (s *Reconciler) NameTaken(ctx context.Context, tenantID, name, region string, account *model.CloudAccount) error {
	rows, err := s.deps.Store.Clusters().ListByName(ctx, tenantID, name)
	if err != nil {
		return storeError(err)
	}
	for _, c := range rows {
		if c.Region != region {
			continue
		}
		n, err := s.deps.Store.Networks().Get(ctx, tenantID, c.NetworkID)
		if errors.Is(err, repository.ErrNotFound) {
			continue
		}
		if err != nil {
			return storeError(err)
		}
		if n.CloudAccountID == account.ID {
			return fault.New(fault.AlreadyExists, fmt.Sprintf(
				"cluster %s (%s) already uses the EKS name %q in account %s, %s; choose another name",
				c.ID, c.State, name, account.AccountID, region))
		}
	}
	return nil
}
