package network

import (
	"context"
	"errors"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// now is the clock; tests replace it.
var now = time.Now

// ConditionSpecEditIgnored: see reconcile.ConditionSpecEditIgnored.
const ConditionSpecEditIgnored = reconcile.ConditionSpecEditIgnored

// editIgnoredMessage is what the status of an imported network says after
// a change made on its object was put back.
const editIgnoredMessage = "spec changes are ignored while imported: take the network over first"

// readOnlyImport reports whether row is an imported network that was not
// taken over and has its plan: its spec, display name and labels are what
// the import left, and nobody may change them.
func readOnlyImport(row *model.Network) bool {
	return (row.State == StateImported || row.State == StateBlocked) && !Discovering(row) && StoredImport(row) != nil
}

// sameSpec reports whether two stored specs say the same.
func sameSpec(a, b model.JSON) bool {
	x, y := &cloudv1.NetworkSpec{}, &cloudv1.NetworkSpec{}
	if Lenient.Unmarshal(a, x) != nil || Lenient.Unmarshal(b, y) != nil {
		return string(a) == string(b)
	}
	return proto.Equal(x, y)
}

// RestoreDiscovered puts what the import left back onto an imported
// network that was not taken over (its spec, display name and labels),
// and reports whether anything had been changed: the API never changes
// them while the network is imported, so a difference is an edit of the
// object made behind its back. TakeOverNetwork calls it before it stores
// the takeover: the plan that is applied is the one that was reviewed.
// False, and nothing changed, for a network that keeps none (imported
// before it was kept).
func RestoreDiscovered(row *model.Network) (bool, error) {
	if !readOnlyImport(row) {
		return false, nil
	}
	d, ok, err := reconcile.DiscoveredOf(row.Input)
	if err != nil || !ok {
		return false, err
	}
	return d.Restore(&row.Resource, sameSpec), nil
}

// keepDiscovered keeps an imported network as its import left it until
// it is taken over (cluster.keepDiscovered, for a network): the first
// pass after the discovery remembers the row as it reads back, with the
// network's stored input; every later pass puts back what was changed on
// the object behind the API's back, and the status and the log say so.
func (s *Reconciler) keepDiscovered(ctx context.Context, row *model.Network) error {
	if !readOnlyImport(row) {
		return nil
	}
	d, ok, err := reconcile.DiscoveredOf(row.Input)
	if err != nil {
		return fault.New(fault.Internal, err.Error())
	}
	before := *row
	switch {
	case !ok:
		fresh, err := s.deps.Store.Networks().Get(ctx, row.TenantID, row.ID)
		if err != nil {
			return err
		}
		*row = *fresh
		before = *row
		d = reconcile.Snapshot(&row.Resource)
	case !d.Restore(&row.Resource, sameSpec):
		return nil
	default:
		d = d.Ignored(now())
	}
	input, err := reconcile.WithDiscovered(row.Input, d)
	if err != nil {
		*row = before
		return fault.New(fault.Internal, err.Error())
	}
	row.Input = input
	if err := s.deps.Store.Networks().Update(ctx, row); err != nil {
		*row = before
		if errors.Is(err, repository.ErrConflict) || errors.Is(err, repository.ErrNotFound) {
			return nil
		}
		return err
	}
	if ok {
		s.logger().WarnContext(ctx, "a spec change of an imported network was ignored: its object was edited behind the API; "+
			"what the import left is back", "tenant_id", row.TenantID, "network_id", row.ID, "name", row.Name)
	}
	return nil
}

// withEditIgnored reports an ignored edit in st: the condition, and a
// note after the message.
func withEditIgnored(st *cloudv1.NetworkStatus, row *model.Network) {
	if !readOnlyImport(row) {
		return
	}
	d, ok, err := reconcile.DiscoveredOf(row.Input)
	if err != nil || !ok || d.EditIgnoredAt == "" {
		return
	}
	st.Conditions = append(st.Conditions, &cloudv1.Condition{Type: ConditionSpecEditIgnored,
		Status: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Imported", Message: editIgnoredMessage})
	st.Message += "; " + editIgnoredMessage
}
