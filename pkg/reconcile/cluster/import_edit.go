package cluster

import (
	"context"
	"errors"

	"google.golang.org/protobuf/proto"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// ConditionSpecEditIgnored: see reconcile.ConditionSpecEditIgnored.
const ConditionSpecEditIgnored = reconcile.ConditionSpecEditIgnored

// editIgnoredMessage is what the status of an imported cluster says after
// a change of its spec made on its object was put back.
const editIgnoredMessage = "spec changes are ignored while imported: take the cluster over first"

// readOnlyImport reports whether row is an imported cluster that was not
// taken over and has its plan: its spec is what the import discovered,
// and nobody may change it.
func readOnlyImport(row *model.Cluster) bool {
	return row.Imported && (row.State == StateImported || row.State == StateBlocked) && !discovering(row)
}

// sameSpec reports whether two stored specs say the same.
func sameSpec(a, b model.JSON) bool {
	x, y := &cloudv1.ClusterSpec{}, &cloudv1.ClusterSpec{}
	if Lenient.Unmarshal(a, x) != nil || Lenient.Unmarshal(b, y) != nil {
		return string(a) == string(b)
	}
	return proto.Equal(x, y)
}

// RestoreDiscovered puts what the import discovered back onto an imported
// cluster that was not taken over (its spec, display name and labels),
// and reports whether anything had been changed. The API never changes
// them while the cluster is imported, so a difference is an edit of the
// object made behind its back. TakeOverCluster calls it before it stores
// the takeover: the plan that is applied is the one that was reviewed.
// False, and nothing changed, for a cluster that keeps no discovered spec
// (imported before it was kept).
func RestoreDiscovered(row *model.Cluster) (bool, error) {
	if !readOnlyImport(row) {
		return false, nil
	}
	d, ok, err := reconcile.DiscoveredOf(row.Adoption)
	if err != nil || !ok {
		return false, err
	}
	return d.Restore(&row.Resource, sameSpec), nil
}

// keepDiscovered keeps an imported cluster as its import left it until it
// is taken over. On the first pass after the discovery it remembers the
// row as it reads back (reconcile.Discovered, with the cluster's
// adoption). On every later pass it compares: a spec, display name or
// labels that differ were changed on the object, behind the API's back,
// and are put back; the status then says so (ConditionSpecEditIgnored)
// and the controller's log does, once per edit. A row a call changed
// meanwhile is left for the next pass.
func (s *Reconciler) keepDiscovered(ctx context.Context, row *model.Cluster) error {
	if !readOnlyImport(row) {
		return nil
	}
	d, ok, err := reconcile.DiscoveredOf(row.Adoption)
	if err != nil {
		return fault.New(fault.Internal, err.Error())
	}
	before := *row
	switch {
	case !ok:
		// As the store reads it back, not as the discovery wrote it: the
		// comparison of later passes is against what a read returns.
		fresh, err := s.deps.Store.Clusters().Get(ctx, row.TenantID, row.ID)
		if err != nil {
			return storeError(err)
		}
		*row = *fresh
		before = *row
		d = reconcile.Snapshot(&row.Resource)
	case !d.Restore(&row.Resource, sameSpec):
		return nil
	default:
		d = d.Ignored(now())
	}
	adoption, err := reconcile.WithDiscovered(row.Adoption, d)
	if err != nil {
		*row = before
		return fault.New(fault.Internal, err.Error())
	}
	row.Adoption = adoption
	if err := s.deps.Store.Clusters().Update(ctx, row); err != nil {
		*row = before
		if errors.Is(err, repository.ErrConflict) || errors.Is(err, repository.ErrNotFound) {
			return nil
		}
		return storeError(err)
	}
	if ok {
		s.logger().WarnContext(ctx, "a spec change of an imported cluster was ignored: its object was edited behind the API; "+
			"the discovered spec is back", "tenant_id", row.TenantID, "cluster_id", row.ID, "name", row.Name)
	}
	return nil
}

// editIgnored reports whether a change of the imported cluster's spec was
// put back since its import.
func editIgnored(row *model.Cluster) bool {
	if !readOnlyImport(row) {
		return false
	}
	d, ok, err := reconcile.DiscoveredOf(row.Adoption)
	return err == nil && ok && d.EditIgnoredAt != ""
}

// withEditIgnored reports an ignored edit in st: the condition, and a
// note after the message.
func withEditIgnored(st *cloudv1.ClusterStatus, row *model.Cluster) {
	if !editIgnored(row) {
		return
	}
	st.Conditions = append(st.Conditions, &cloudv1.Condition{Type: ConditionSpecEditIgnored,
		Status: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Imported", Message: editIgnoredMessage})
	st.Message += "; " + editIgnoredMessage
}
