package network

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// object is the Network object events of row are about.
func object(row *model.Network) reconcile.Object {
	return reconcile.Object{Kind: reconcile.KindNetwork, Namespace: kube.TenantNamespace(row.TenantID), Name: row.Name}
}

// now is the clock; tests replace it.
var now = time.Now

// resourcesLeftPattern is the count in a deletion's message: it changes
// with every object that goes, which is not news.
var resourcesLeftPattern = regexp.MustCompile(`; [0-9]+ resources left`)

// storedStatus decodes the status recorded with the row; empty when there
// is none this build can read.
func storedStatus(row *model.Network) *cloudv1.NetworkStatus {
	st := &cloudv1.NetworkStatus{}
	if len(row.Status) > 0 {
		if err := Lenient.Unmarshal(row.Status, st); err != nil {
			return &cloudv1.NetworkStatus{}
		}
	}
	return st
}

// statusEvents says what happened between two recorded statuses of a
// network: the events of a pass. before is what the last pass recorded
// (empty for the first), after what this one found. A pass that found
// things as they were gives none.
func statusEvents(before, after *cloudv1.NetworkStatus) []reconcile.Note {
	out := reconcile.PassFailure(before.GetConditions(), after.GetConditions(), ConditionReconciled)
	out = append(out, reconcile.FreezeNotes(before.GetFreeze(), after.GetFreeze())...)
	return append(out, stateEvents(before, after)...)
}

// stateEvents are the events of the network's state: reaching it, and,
// while it stays FAILED or DELETING, a change of what it is about.
func stateEvents(before, after *cloudv1.NetworkStatus) []reconcile.Note {
	was, is := before.GetState(), after.GetState()
	imported := importEvents(before, after)
	switch is {
	case cloudv1.NetworkState_NETWORK_STATE_PROVISIONING:
		if was != is && len(imported) == 0 {
			return []reconcile.Note{reconcile.Normal(reconcile.ReasonProvisioning, "creating the network's resources")}
		}
	case cloudv1.NetworkState_NETWORK_STATE_READY:
		if was != is {
			return append(imported, reconcile.Normal(reconcile.ReasonReady,
				fmt.Sprintf("all %d resources are ready", len(after.GetManagedResources()))))
		}
	case cloudv1.NetworkState_NETWORK_STATE_UPDATING:
		if was != is && len(imported) == 0 {
			return []reconcile.Note{reconcile.Normal(reconcile.ReasonUpdating, "applying a change: "+after.GetMessage())}
		}
	case cloudv1.NetworkState_NETWORK_STATE_FAILED:
		if after.GetImport().GetPhase() == cloudv1.ImportPhase_IMPORT_PHASE_BLOCKED ||
			reconcile.PassFailed(after.GetConditions(), ConditionReconciled, after.GetMessage()) {
			break // ImportBlocked, or the failed pass (PassFailure), says it
		}
		if was != is || failing(before.GetMessage()) != failing(after.GetMessage()) {
			return []reconcile.Note{reconcile.Warning(reconcile.ReasonFailed, after.GetMessage())}
		}
	case cloudv1.NetworkState_NETWORK_STATE_DELETING:
		if was != is || deleting(before.GetMessage()) != deleting(after.GetMessage()) {
			return []reconcile.Note{reconcile.Normal(reconcile.ReasonDeleting, after.GetMessage())}
		}
	default:
	}
	return imported
}

// importEvents are the events of an import: discovered, blocked, taken
// over.
func importEvents(before, after *cloudv1.NetworkStatus) []reconcile.Note {
	was, is := before.GetImport().GetPhase(), after.GetImport().GetPhase()
	if was == is {
		return nil
	}
	switch is {
	case cloudv1.ImportPhase_IMPORT_PHASE_PLANNED:
		return []reconcile.Note{reconcile.Normal(reconcile.ReasonImportDiscovered,
			fmt.Sprintf("found the existing network's resources (%d); observed only until it is taken over",
				len(after.GetImport().GetResources())))}
	case cloudv1.ImportPhase_IMPORT_PHASE_BLOCKED:
		return []reconcile.Note{reconcile.Warning(reconcile.ReasonImportBlocked, "the import cannot be taken over: "+firstBlocking(after.GetImport()))}
	case cloudv1.ImportPhase_IMPORT_PHASE_CONVERGING:
		return []reconcile.Note{reconcile.Normal(reconcile.ReasonTakenOver, "the platform owns the network's resources now; applying them")}
	default:
		return nil
	}
}

// deleting is what a deletion's message says it is deleting, without the
// count of resources.
func deleting(message string) string { return resourcesLeftPattern.ReplaceAllString(message, "") }

// failing is what a failure's message is about: the part before the
// provider's own text ("<Kind> <name>"), which may differ on every try.
func failing(message string) string {
	if i := strings.Index(message, ": "); i >= 0 {
		return message[:i]
	}
	return message
}

// withEvents puts what happened in this pass into st (recent_events) and
// returns it: what the pass's steps said themselves (reconcile.AddNote: a
// repair), then what the status change means (statusEvents). The caller
// emits it once st is stored (emit).
func withEvents(ctx context.Context, row *model.Network, st *cloudv1.NetworkStatus) []reconcile.Note {
	before := storedStatus(row)
	notes := append(reconcile.Notes(ctx), statusEvents(before, st)...)
	st.RecentEvents = reconcile.Recent(before.GetRecentEvents(), notes, now())
	return notes
}

// emit reports notes as events of the network's object.
func (s *Reconciler) emit(ctx context.Context, row *model.Network, notes []reconcile.Note) {
	for _, n := range notes {
		s.deps.Emit(ctx, object(row), n.Type, n.Reason, n.Message)
	}
}
