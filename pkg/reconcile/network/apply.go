package network

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/fault"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// Apply makes the network's managed resources match plan, in order
// (crossplaneaws.NetworkSet), and marks the generation applied once the
// whole set is: while NAT gateways wait for the internet gateway,
// Reconcile applies the network again.
func (s *Reconciler) Apply(ctx context.Context, row *model.Network, a *model.CloudAccount, plan crossplaneaws.NetworkPlan) error {
	set, err := crossplaneaws.NetworkSet(plan)
	if err != nil {
		return fault.New(fault.Invalid, "spec.layout: "+err.Error())
	}
	res, err := s.ApplySet(ctx, row, a, set)
	if err != nil || !res.Complete() {
		return err // held back: not applied in full yet
	}
	return s.MarkApplied(ctx, row)
}

// ApplySet applies set as the network's objects (sequence.Engine: what
// the network has is recorded, what waits is held back, what the set
// dropped is removed). The caller marks the row applied once the result
// is complete.
func (s *Reconciler) ApplySet(ctx context.Context, row *model.Network, a *model.CloudAccount, set sequence.Set) (sequence.Result, error) {
	if a.Namespace == "" || a.ProviderConfigName == "" {
		return sequence.Result{}, fault.New(fault.FailedPrecondition,
			fmt.Sprintf("account %s is not fully bootstrapped; run BootstrapTenant again", a.ID))
	}
	owner := reconcile.Owner{TenantID: row.TenantID, CloudAccountID: a.ID, Kind: model.OwnerNetwork, ID: row.ID}
	res, err := s.deps.Sequence().Apply(ctx, set, s.deps.Inventory(owner))
	switch {
	case err == nil:
		return res, nil
	case meta.IsNoMatchError(err):
		return res, fault.New(fault.FailedPrecondition,
			"a Crossplane AWS provider the network needs (ec2, iam, cloudwatchlogs) is not installed in the management cluster")
	default:
		s.logger().ErrorContext(ctx, "apply network resources", "tenant_id", row.TenantID, "network_id", row.ID, "error", err)
		return res, fault.New(fault.Internal, "could not create the network's resources")
	}
}

// MarkApplied records that every object of the row's generation was
// applied without error, with this build's renderer.
func (s *Reconciler) MarkApplied(ctx context.Context, row *model.Network) error {
	if row.AppliedGeneration == row.Generation && row.RenderVersion == crossplaneaws.RenderVersion {
		return nil
	}
	before, version := row.AppliedGeneration, row.RenderVersion
	row.AppliedGeneration, row.RenderVersion = row.Generation, crossplaneaws.RenderVersion
	err := s.deps.Store.Networks().Update(ctx, row)
	if errors.Is(err, repository.ErrConflict) {
		// A call changed the row while it was applied: it is behind again
		// (calls only store), and the next pass applies it as it is now.
		row.AppliedGeneration, row.RenderVersion = before, version
		return nil
	}
	if err != nil {
		return fault.New(fault.Internal, "could not record the applied generation: "+err.Error())
	}
	return nil
}

// liveObjects counts the network's objects the API server still has (by
// label, uncached).
func (s *Reconciler) liveObjects(ctx context.Context, row *model.Network) (int, error) {
	n := 0
	for _, list := range crossplaneaws.NetworkLists() {
		err := s.deps.Reader().List(ctx, list, client.InNamespace(kube.TenantNamespace(row.TenantID)),
			client.MatchingLabels{crossplane.LabelNetwork: row.ID})
		if meta.IsNoMatchError(err) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("list %T: %w", list, err)
		}
		n += meta.LenList(list)
	}
	return n, nil
}

// Finalize moves a DELETING network's deletion on (sequence.Engine.Delete
// with crossplaneaws.NetworkDeleteOrder: the next
// tier once the previous one is gone) and removes its row once all its
// resources are gone.
func (s *Reconciler) Finalize(ctx context.Context, row *model.Network) error {
	if row.State != StateDeleting {
		return nil
	}
	o, err := s.observe(ctx, row.TenantID, row.ID)
	if err != nil {
		return fault.New(fault.Unavailable, "cannot read the network's resources from the management cluster")
	}
	// records also records live objects that were never recorded (by
	// label), so every resource of the network is deleted in order.
	records, err := s.records(ctx, row, o.objects)
	if err != nil {
		return err
	}
	owner := reconcile.Owner{TenantID: row.TenantID, CloudAccountID: row.CloudAccountID, Kind: model.OwnerNetwork, ID: row.ID}
	deletion, err := s.deps.Sequence().Delete(ctx, s.deps.Inventory(owner), crossplaneaws.NetworkDeleteOrder)
	var unreadable *managed.ReadError
	switch {
	case errors.As(err, &unreadable):
		return fault.New(fault.Unavailable,
			fmt.Sprintf("cannot read %s %s from the management cluster", unreadable.Ref.Kind, unreadable.Ref.Name))
	case err != nil:
		s.logger().ErrorContext(ctx, "delete network resources", "network_id", row.ID, "error", err)
		return fault.New(fault.Internal, "could not delete the network's resources")
	}
	if err := s.deps.MarkGone(ctx, records, deletion.Views); err != nil {
		return fault.New(fault.Internal, err.Error())
	}
	if !deletion.Done() || o.total > 0 {
		return nil
	}
	// The cache says everything is gone: confirm with the API server before
	// forgetting the network (a cache that lost its objects must not make
	// the platform forget cloud resources).
	if live, err := s.liveObjects(ctx, row); err != nil || live > 0 {
		if err != nil {
			return fault.New(fault.Unavailable, "cannot confirm the network's resources are gone: "+err.Error())
		}
		return nil
	}
	// Records first: a failure after it is retried on the next pass (the
	// row is still there), never leaving records without a network.
	if err := s.deps.Store.ManagedResources().DeleteByOwner(ctx, model.OwnerNetwork, row.ID); err != nil {
		return fault.New(fault.Internal, "could not forget the deleted network's resources: "+err.Error())
	}
	if err := s.deps.Store.Networks().Delete(ctx, row.TenantID, row.ID); err != nil && !errors.Is(err, repository.ErrNotFound) {
		return fault.New(fault.Internal, "could not remove the deleted network: "+err.Error())
	}
	return nil
}
