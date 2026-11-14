package reconcile

import (
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// ConditionPaused is the condition of a network or cluster some of whose
// managed resources are paused (AdminService.PauseReconcile): present and
// True while any is, absent otherwise.
const ConditionPaused = "Paused"

// PausedCount is how many of views are paused: they carry
// crossplane.io/paused, so their providers do not act on them.
func PausedCount(views []*cloudv1.ManagedResource) int {
	n := 0
	for _, v := range views {
		if v.GetPaused() {
			n++
		}
	}
	return n
}

// PausedCondition is the Paused condition for views, or nil when none of
// them is paused. A pause is the operator's (a provider upgrade): it is
// reported, and it is never a failure.
func PausedCondition(views []*cloudv1.ManagedResource) *cloudv1.Condition {
	n := PausedCount(views)
	if n == 0 {
		return nil
	}
	return &cloudv1.Condition{Type: ConditionPaused, Status: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE,
		Reason:  managed.ReasonPaused,
		Message: fmt.Sprintf("%d of %d resources are paused: nothing changes in the cloud until they are resumed (admin resume)", n, len(views))}
}

// PausedNote is what a status message says about paused resources: ""
// when none is, else "; N resources paused" to append.
func PausedNote(views []*cloudv1.ManagedResource) string {
	if n := PausedCount(views); n > 0 {
		return fmt.Sprintf("; %d resources paused", n)
	}
	return ""
}

// PauseProto is a row's pause as its status carries it (status.pause);
// nil for a row that is not paused.
func PauseProto(p *model.Pause) *cloudv1.Pause {
	if p == nil {
		return nil
	}
	out := &cloudv1.Pause{By: p.By, Reason: p.Reason}
	if !p.Since.IsZero() {
		out.Since = timestamppb.New(p.Since)
	}
	return out
}

// PausedBy says who paused a row, since when and why: "paused by alice
// since 2026-11-14T09:00:00Z (provider upgrade)". "" for a row that is
// not paused.
func PausedBy(p *model.Pause) string {
	if p == nil {
		return ""
	}
	out := "paused by " + p.By
	if !p.Since.IsZero() {
		out += " since " + p.Since.UTC().Format(time.RFC3339)
	}
	if p.Reason != "" {
		out += " (" + p.Reason + ")"
	}
	return out
}

// pausedRowNote is what a paused row's status adds to who paused it.
const pausedRowNote = "nothing is created, changed or deleted until it is resumed (admin resume)"

// RowPause is how a status reports a pause: the Paused condition, and
// what to add to the status message. A paused row (pause, the operator's
// pause recorded on the account, network or cluster) says who paused it:
// the platform itself does not act on it. A row that is not paused but
// has paused resources (their crossplane.io/paused annotation, e.g. its
// network's subnets) says how many: PausedCondition and PausedNote. nil
// and "" when nothing is paused. A pause is never a failure.
func RowPause(pause *model.Pause, views []*cloudv1.ManagedResource) (*cloudv1.Condition, string) {
	if pause == nil {
		return PausedCondition(views), PausedNote(views)
	}
	message := PausedBy(pause) + ": " + pausedRowNote
	if n := PausedCount(views); n > 0 {
		message += fmt.Sprintf("; %d of %d resources carry the providers' pause", n, len(views))
	}
	return &cloudv1.Condition{Type: ConditionPaused, Status: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE,
		Reason: managed.ReasonPaused, Message: message}, "; " + PausedBy(pause) + ": " + pausedRowNote
}
