package network

import (
	"context"
	"errors"
	"log/slog"
	"sort"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// View is the API view of a stored network: spec, observed status, the
// clusters using it.
//
// A failed attempt to apply the network is part of it (the failure the
// last pass recorded with the row): nothing else says so while there are
// no resources to observe.
func (s *Reconciler) View(ctx context.Context, row *model.Network) (*cloudv1.Network, error) {
	return s.view(ctx, row, storedFailure(row))
}

// view is View with the failure to report: the recorded one for a read,
// this pass's for the reconciler.
func (s *Reconciler) view(ctx context.Context, row *model.Network, failure *cloudv1.Condition) (*cloudv1.Network, error) {
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerNetwork, row.ID)
	if err != nil {
		return nil, err
	}
	n, err := s.viewOf(ctx, row, records)
	if err != nil {
		return nil, err
	}
	withFailure(n.GetStatus(), failure)
	n.GetStatus().CodeChange = reconcile.CodeOf(ctx, storedStatus(row).GetCodeChange())
	// What the controller reported last stays with the status: a read
	// returns it, and a pass adds to it (withEvents).
	n.GetStatus().RecentEvents = storedStatus(row).GetRecentEvents()
	return n, nil
}

// viewOf is view with the network's records at hand. It only reads.
func (s *Reconciler) viewOf(ctx context.Context, row *model.Network, records []model.ManagedResource) (*cloudv1.Network, error) {
	spec, err := SpecOf(row)
	if err != nil {
		return nil, fault.New(fault.Internal, err.Error())
	}
	o, err := s.observe(ctx, row.TenantID, row.ID)
	if err != nil {
		s.logger().ErrorContext(ctx, "observe network", "network_id", row.ID, "error", err)
		return nil, fault.New(fault.Unavailable, "cannot read the network's resources from the management cluster")
	}
	views, err := s.deps.Observe(ctx, records)
	if err != nil {
		return nil, err
	}
	if row.State == StateProvisioning || row.State == StateUpdating {
		reconcile.SettleMissing(records, views)
	}
	clusters, err := s.deps.Store.Clusters().ListByNetwork(ctx, row.ID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(clusters))
	for _, c := range clusters {
		ids = append(ids, c.Name)
	}
	return ToProto(row, spec, status(row, views, o.resources, ids)), nil
}

func (s *Reconciler) logger() *slog.Logger {
	if s.deps.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.deps.Logger
}

// records returns the network's recorded objects, first recording any live
// object of the network (by label) that has no record yet: networks created
// before recording existed, or a record write that failed. The view so
// always covers every resource Crossplane has for the network.
func (s *Reconciler) records(ctx context.Context, row *model.Network, live []client.Object) ([]model.ManagedResource, error) {
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerNetwork, row.ID)
	if err != nil {
		return nil, err
	}
	known := make(map[string]bool, len(records))
	for _, r := range records {
		known[r.Kind+"/"+r.Name] = true
	}
	var missing []client.Object
	for _, obj := range live {
		gvk, err := apiutil.GVKForObject(obj, s.deps.Kube.Scheme())
		if err != nil {
			return nil, fault.New(fault.Internal, err.Error())
		}
		if !known[gvk.Kind+"/"+obj.GetName()] {
			missing = append(missing, obj)
		}
	}
	if len(missing) == 0 {
		return records, nil
	}
	sortByCreation(s.deps.Kube.Scheme(), missing)
	owner := reconcile.Owner{TenantID: row.TenantID, CloudAccountID: row.CloudAccountID, Kind: model.OwnerNetwork, ID: row.ID}
	if err := s.deps.Record(ctx, owner, missing); err != nil {
		return nil, err
	}
	s.logger().InfoContext(ctx, "recorded network resources found in the cluster", "network_id", row.ID, "count", len(missing))
	records, err = s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerNetwork, row.ID)
	if err != nil {
		return nil, err
	}
	return records, nil
}

// sortByCreation orders objects as NetworkResources creates them: VPC,
// internet gateway, subnets, EIPs, NAT gateways, route tables, routes,
// associations; by name within a kind.
func sortByCreation(scheme *runtime.Scheme, objects []client.Object) {
	rank := map[string]int{}
	kinds := crossplaneaws.NetworkObjects() // deletion order: reversed creation order
	for i, o := range kinds {
		if gvk, err := apiutil.GVKForObject(o, scheme); err == nil {
			rank[gvk.Kind] = len(kinds) - i
		}
	}
	kindOf := func(o client.Object) string {
		gvk, _ := apiutil.GVKForObject(o, scheme)
		return gvk.Kind
	}
	sort.SliceStable(objects, func(i, j int) bool {
		ri, rj := rank[kindOf(objects[i])], rank[kindOf(objects[j])]
		if ri != rj {
			return ri < rj
		}
		return objects[i].GetName() < objects[j].GetName()
	})
}

// remember records a network's status as observed now, for GetNetwork and
// ListNetworks. It writes the status column only: the etag does not
// change. A network that is gone has nothing to record, and one that a
// call changed since row was read keeps what that call stored.
//
// What changed since the status recorded before is what happened in this
// pass: it is kept with the status (recent_events) and, once the status
// is stored, emitted as events of the network's object. A pass that found
// things as they were records and emits nothing.
func (s *Reconciler) remember(ctx context.Context, row *model.Network, st *cloudv1.NetworkStatus) error {
	notes := withEvents(ctx, row, st)
	raw, err := Encode(st)
	if err != nil {
		return err
	}
	if string(raw) == string(row.Status) {
		return nil
	}
	err = s.deps.Store.Networks().SetStatus(ctx, row.TenantID, row.ID, row.ETag, raw)
	if errors.Is(err, repository.ErrNotFound) || errors.Is(err, repository.ErrConflict) {
		return nil // gone, or changed since it was read: the next pass records it
	}
	if err == nil {
		row.Status = raw
		s.emit(ctx, row, notes)
	}
	return err
}

// settle records what the loop observed of a network that is not being
// deleted: its status, and READY as its stored state once every resource
// is ready and its generation is applied, so the loop leaves it alone
// until it changes. failed is what went wrong applying it in this pass.
func (s *Reconciler) settle(ctx context.Context, row *model.Network, failed error) error {
	n, err := s.view(ctx, row, FailureCondition(failed))
	if err != nil {
		return err
	}
	st := n.GetStatus()
	converging := row.State == StateProvisioning || row.State == StateUpdating
	if !converging || st.GetState() != cloudv1.NetworkState_NETWORK_STATE_READY || row.AppliedGeneration < row.Generation {
		return s.remember(ctx, row, st)
	}
	// READY and its status in one write: once READY the loop does not come
	// back, so the recorded status must not be left behind.
	notes := withEvents(ctx, row, st)
	raw, err := Encode(st)
	if err != nil {
		return err
	}
	before, status := row.State, row.Status
	row.State, row.Status = StateReady, raw
	if err := s.deps.Store.Networks().Record(ctx, row); err != nil {
		row.State, row.Status = before, status
		if errors.Is(err, repository.ErrConflict) {
			return nil // an API call changed the row; the next pass sees it
		}
		return err
	}
	s.emit(ctx, row, notes)
	return nil
}
