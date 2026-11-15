package reconcile

import (
	"context"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// Event types, as Kubernetes spells them.
const (
	EventNormal  = "Normal"
	EventWarning = "Warning"
)

// The reasons of the events the controller emits: what happened, as one
// word. An event is emitted when something changes, never on a pass that
// found things as they were. docs/events.md has the same table.
//
//	Object        Reason                      Type     When
//	Cluster       Provisioning                Normal   its resources are being created
//	Network       Provisioning                Normal   its resources are being created
//	Cluster       ControlPlaneActive          Normal   the EKS cluster is ACTIVE
//	Cluster       NodeGroupReady              Normal   the default node group is ready
//	Cluster       AddonsReady                 Normal   every add-on is ready
//	Cluster       Running                     Normal   every resource is ready and the generation is applied
//	Network       Ready                       Normal   every resource is ready and the generation is applied
//	both          Updating                    Normal   a new generation, or a renderer change, is being applied
//	both          Failed                      Warning  a resource failed, vanished, or the pass failed; the message says which
//	both          Repaired                    Normal   an object deleted from the management cluster was created again
//	both          Paused                      Normal   an operator paused its resources (admin pause): nothing changes in the cloud
//	both          Resumed                     Normal   the pause ended
//	both          Deleting                    Normal   deletion started, or waits for something else than before
//	both          Deleted                     Normal   everything is gone and the object is removed
//	both          ImportDiscovered            Normal   an import found the existing resources; it is observed, not owned
//	both          ImportBlocked               Warning  the import found something it cannot take over
//	both          TakenOver                   Normal   the platform owns the imported resources now
//	Cluster       UpgradeStarted              Normal   a Kubernetes upgrade began (control plane first)
//	Cluster       UpgradeNodes                Normal   the control plane runs the new version; the node groups, then the add-ons, follow
//	Cluster       UpgradeFinished             Normal   control plane, node groups and add-ons run the new version
//	Cluster       PlatformComponentInstalling Normal   a platform component's charts are being installed
//	Cluster       PlatformComponentReady      Normal   a platform component is installed
//	Cluster       PlatformComponentFailed     Warning  a platform component failed: chart, Helm state, last error
//	Cluster       PlatformComponentRemoved    Normal   a platform component is no longer installed
//	Cluster       StuckReleaseRecovered       Warning  a stuck Helm release's record was removed so it installs again
//	Cluster       RecoveryLimitReached        Warning  a stuck Helm release was recovered as often as allowed
//	Cluster       PlatformComponentsReady     Normal   every platform component is installed
//	Cluster       DeprecatedAPIs              Warning  the scan found API versions removed at the next Kubernetes version
//	Cluster       DeprecationScanFailed       Warning  the scan for deprecated API versions failed
//	Cluster       DeprecationScanClean        Normal   a scan that had findings, or failed, found nothing removed now
//	Cluster       KubernetesEnding            Warning  the cluster's Kubernetes minor is on no release channel any more: it is ending
//	CloudAccount  Bootstrapped                Normal   the tenant namespace and the account's ProviderConfig exist
//	CloudAccount  ProviderConfigFailed        Warning  the account's ProviderConfig or credentials copy could not be applied
//	CloudAccount  AccessVerified              Normal   the tenant role works in every region the account uses
//	CloudAccount  AccessDenied                Warning  a region or call of the access check was denied; the message says which
//	CloudAccount  Deleted                     Normal   the account's objects are gone
//	Release       ReleaseValid                Normal   a release passed its checks and can be offered
//	Release       ReleaseInvalid              Warning  a release fails a check; the message says which
//	Channel       ChannelOffers               Normal   a channel offers another release than before
//	Channel       ChannelCannotMove           Warning  a channel cannot offer the release it should
//	Channel       RolloutTier                 Normal   the rollout of the channel's minor moved to another tier
//	Channel       RolloutHalted               Warning  too many upgrades failed in a tier; nothing more starts
//	both          AutomaticUpgradeStarted     Normal   the rollout set the cluster's next minor (cluster and channel)
//	Cluster       AutomaticUpgradeSucceeded   Normal   an automatic upgrade finished: the cluster runs the new minor
//	Cluster       AutomaticUpgradeFailed      Warning  an automatic upgrade failed or did not finish in time
const (
	ReasonProvisioning                = "Provisioning"
	ReasonControlPlaneActive          = "ControlPlaneActive"
	ReasonNodeGroupReady              = "NodeGroupReady"
	ReasonAddonsReady                 = "AddonsReady"
	ReasonRunning                     = "Running"
	ReasonReady                       = "Ready"
	ReasonUpdating                    = "Updating"
	ReasonFailed                      = "Failed"
	ReasonRepaired                    = "Repaired"
	ReasonPaused                      = "Paused"
	ReasonResumed                     = "Resumed"
	ReasonDeleting                    = "Deleting"
	ReasonDeleted                     = "Deleted"
	ReasonTeardownStarted             = "TeardownStarted"
	ReasonTeardownFinished            = "TeardownFinished"
	ReasonTeardownTimedOut            = "TeardownTimedOut"
	ReasonTeardownSkipped             = "TeardownSkipped"
	ReasonImportDiscovered            = "ImportDiscovered"
	ReasonImportBlocked               = "ImportBlocked"
	ReasonTakenOver                   = "TakenOver"
	ReasonUpgradeStarted              = "UpgradeStarted"
	ReasonUpgradeNodes                = "UpgradeNodes"
	ReasonUpgradeFinished             = "UpgradeFinished"
	ReasonPlatformComponentInstalling = "PlatformComponentInstalling"
	ReasonPlatformComponentReady      = "PlatformComponentReady"
	ReasonPlatformComponentFailed     = "PlatformComponentFailed"
	ReasonPlatformComponentRemoved    = "PlatformComponentRemoved"
	ReasonStuckReleaseRecovered       = "StuckReleaseRecovered"
	ReasonRecoveryLimitReached        = "RecoveryLimitReached"
	ReasonPlatformComponentsReady     = "PlatformComponentsReady"
	ReasonTenantComponentsReady       = "TenantComponentsReady"
	ReasonComponentRetried            = "ComponentRetried"
	ReasonUpgradeRetried              = "UpgradeRetried"
	ReasonDeprecatedAPIs              = "DeprecatedAPIs"
	ReasonDeprecationScanFailed       = "DeprecationScanFailed"
	ReasonDeprecationScanClean        = "DeprecationScanClean"
	ReasonKubernetesEnding            = "KubernetesEnding"
	ReasonBootstrapped                = "Bootstrapped"
	ReasonProviderConfigFailed        = "ProviderConfigFailed"
	ReasonAccessVerified              = "AccessVerified"
	ReasonAccessDenied                = "AccessDenied"
	ReasonReleaseValid                = "ReleaseValid"
	ReasonReleaseInvalid              = "ReleaseInvalid"
	ReasonChannelOffers               = "ChannelOffers"
	ReasonChannelCannotMove           = "ChannelCannotMove"
	ReasonPromotionWaiting            = "PromotionWaiting"
	ReasonRolloutTier                 = "RolloutTier"
	ReasonRolloutHalted               = "RolloutHalted"
	ReasonAutomaticUpgradeStarted     = "AutomaticUpgradeStarted"
	ReasonAutomaticUpgradeSucceeded   = "AutomaticUpgradeSucceeded"
	ReasonAutomaticUpgradeFailed      = "AutomaticUpgradeFailed"
)

// Kinds of the objects events are about.
const (
	KindCloudAccount   = "CloudAccount"
	KindNetwork        = "Network"
	KindCluster        = "Cluster"
	KindRelease        = "Release"
	KindReleaseChannel = "ReleaseChannel"
)

// MaxRecentEvents is how many events a network's or a cluster's status
// keeps (status.recentEvents).
const MaxRecentEvents = 10

// Object names the object of the platform's kinds an event is about.
// Namespace is empty for a cluster-scoped kind.
type Object struct{ Kind, Namespace, Name string }

// Events is where what happens is reported as Kubernetes events. The
// controller gives the reconcilers its event recorder behind it; what
// only reads (the API handlers, tests) has none, and nothing is emitted.
type Events interface {
	// Event reports that reason happened to obj. eventType is EventNormal
	// or EventWarning.
	Event(ctx context.Context, obj Object, eventType, reason, message string)
}

// Emit reports an event; a no-op without Events.
func (d Deps) Emit(ctx context.Context, obj Object, eventType, reason, message string) {
	if d.Events != nil {
		d.Events.Event(ctx, obj, eventType, reason, message)
	}
}

// Note is something that happened in a pass, before it is an event: what
// a status change means (the reconcilers' diffs), or a step that says so
// itself (a repair).
type Note struct{ Type, Reason, Message string }

// Normal and Warning are a Note of that type.
func Normal(reason, message string) Note {
	return Note{Type: EventNormal, Reason: reason, Message: message}
}
func Warning(reason, message string) Note {
	return Note{Type: EventWarning, Reason: reason, Message: message}
}

type notesKey struct{}

// notes collects the Notes of one pass.
type notes struct {
	mu   sync.Mutex
	list []Note
}

// WithNotes returns a context a pass's steps can leave Notes in
// (AddNote); Notes reads them.
func WithNotes(ctx context.Context) context.Context {
	if _, ok := ctx.Value(notesKey{}).(*notes); ok {
		return ctx
	}
	return context.WithValue(ctx, notesKey{}, &notes{})
}

// AddNote leaves a Note for the pass that owns ctx; dropped when the
// context collects none.
func AddNote(ctx context.Context, n Note) {
	if c, ok := ctx.Value(notesKey{}).(*notes); ok {
		c.mu.Lock()
		c.list = append(c.list, n)
		c.mu.Unlock()
	}
}

// Notes are the Notes left in ctx so far, in order.
func Notes(ctx context.Context) []Note {
	c, ok := ctx.Value(notesKey{}).(*notes)
	if !ok {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Note(nil), c.list...)
}

// Recent adds what happened in a pass to the events a status keeps
// (oldest first): an event with the reason and message of the newest one
// kept is that event again (its time moves, its count grows), anything
// else is a new row, and only the last MaxRecentEvents stay. before is
// not changed.
func Recent(before []*cloudv1.Event, happened []Note, at time.Time) []*cloudv1.Event {
	out := make([]*cloudv1.Event, 0, len(before)+len(happened))
	for _, e := range before {
		out = append(out, &cloudv1.Event{Time: e.GetTime(), Type: e.GetType(), Reason: e.GetReason(),
			Message: e.GetMessage(), Count: e.GetCount()})
	}
	for _, n := range happened {
		if last := len(out) - 1; last >= 0 && out[last].GetReason() == n.Reason && out[last].GetMessage() == n.Message {
			out[last].Time, out[last].Count = timestamppb.New(at), max(out[last].GetCount(), 1)+1
			continue
		}
		out = append(out, &cloudv1.Event{Time: timestamppb.New(at), Type: EventTypeOf(n.Type), Reason: n.Reason,
			Message: n.Message, Count: 1})
	}
	if len(out) > MaxRecentEvents {
		out = out[len(out)-MaxRecentEvents:]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// EventTypeOf is the contract's spelling of a Kubernetes event type.
func EventTypeOf(eventType string) cloudv1.EventType {
	if eventType == EventWarning {
		return cloudv1.EventType_EVENT_TYPE_WARNING
	}
	return cloudv1.EventType_EVENT_TYPE_NORMAL
}

// Emitted is one event a RecordedEvents saw.
type Emitted struct {
	Object Object
	Note
}

// RecordedEvents is an Events that keeps what it is given: for tests.
type RecordedEvents struct {
	mu   sync.Mutex
	list []Emitted
}

// Event implements Events.
func (r *RecordedEvents) Event(_ context.Context, obj Object, eventType, reason, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.list = append(r.list, Emitted{Object: obj, Note: Note{Type: eventType, Reason: reason, Message: message}})
}

// All are the events seen so far, in order.
func (r *RecordedEvents) All() []Emitted {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Emitted(nil), r.list...)
}

// Reasons are the reasons of the events seen so far, in order.
func (r *RecordedEvents) Reasons() []string {
	all := r.All()
	out := make([]string, 0, len(all))
	for _, e := range all {
		out = append(out, e.Reason)
	}
	return out
}

// Reset forgets what was seen.
func (r *RecordedEvents) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.list = nil
}

// PassFailure is the Note of a pass that could not apply its owner: the
// condition kind (the reconcilers' ConditionReconciled) is False in after
// and was not, or said something else, in before. Nil otherwise.
func PassFailure(before, after []*cloudv1.Condition, kind string) []Note {
	find := func(conditions []*cloudv1.Condition) *cloudv1.Condition {
		for _, c := range conditions {
			if c.GetType() == kind && c.GetStatus() == cloudv1.ConditionStatus_CONDITION_STATUS_FALSE {
				return c
			}
		}
		return nil
	}
	is, was := find(after), find(before)
	if is == nil || (was != nil && was.GetMessage() == is.GetMessage()) {
		return nil
	}
	return []Note{Warning(ReasonFailed, "could not apply the change: "+is.GetMessage())}
}

// PassFailed reports whether the conditions say the last pass could not
// apply its owner (kind is False), with that message: the state such a
// status carries is FAILED for the same reason, which PassFailure reports.
func PassFailed(conditions []*cloudv1.Condition, kind, message string) bool {
	for _, c := range conditions {
		if c.GetType() == kind && c.GetStatus() == cloudv1.ConditionStatus_CONDITION_STATUS_FALSE && c.GetMessage() == message {
			return true
		}
	}
	return false
}

// PauseNotes are the Notes of a pause starting or ending: the Paused
// condition (PausedCondition) is in after and was not in before, or the
// other way round. A pause that goes on says nothing.
func PauseNotes(before, after []*cloudv1.Condition) []Note {
	find := func(conditions []*cloudv1.Condition) *cloudv1.Condition {
		for _, c := range conditions {
			if c.GetType() == ConditionPaused && c.GetStatus() == cloudv1.ConditionStatus_CONDITION_STATUS_TRUE {
				return c
			}
		}
		return nil
	}
	was, is := find(before), find(after)
	switch {
	case was == nil && is != nil:
		return []Note{Normal(ReasonPaused, is.GetMessage())}
	case was != nil && is == nil:
		return []Note{Normal(ReasonResumed, "the pause ended: the platform and the providers act on it again")}
	default:
		return nil
	}
}
