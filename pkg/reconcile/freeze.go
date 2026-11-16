package reconcile

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// FreezeName is the name of the release hold that is the platform freeze
// (AdminService.Freeze): one object, cluster-scoped, which the API server
// writes and the controllers read from their cache.
const FreezeName = "platform-freeze"

const freezePausedAnnotation = "cloud.cluster-vending.io/freeze-paused"

// ConditionFrozen is the condition of a network or cluster the platform
// freeze holds, or some of whose managed resources are paused: present
// and True while it is, absent otherwise.
const ConditionFrozen = "Frozen"

// FreezeOf is the platform freeze among holds at now as the resource with
// ids sees it (a cluster's ID; a network's ID): nil when the platform is
// not frozen (no freeze, or its until has passed); LetThrough when the
// freeze lets one of ids through.
func FreezeOf(holds []cloudv1alpha1.ReleaseHold, now time.Time, ids ...string) *model.Freeze {
	target := FreezeTarget{}
	if len(ids) > 0 {
		target.ClusterID, target.NetworkID = ids[0], ids[0]
	}
	if len(ids) > 1 {
		target.NetworkID = ids[1]
	}
	for index := range holds {
		if frozen := FreezeForTarget(&holds[index], now, target); frozen != nil {
			return frozen
		}
	}
	return nil
}

// Freeze is the platform freeze as the resource with ids sees it
// (FreezeOf), read from the platform source's holds. No source of holds
// (tests) is no freeze. Holds that cannot be read are an error: nothing
// is done blind.
func (d Deps) Freeze(ctx context.Context, ids ...string) (*model.Freeze, error) {
	if d.KubeReader != nil || d.Kube != nil {
		target := FreezeTarget{}
		if len(ids) > 0 {
			target.ClusterID, target.NetworkID = ids[0], ids[0]
		}
		if len(ids) > 1 {
			target.NetworkID = ids[1]
		}
		return d.FreezeFor(ctx, target)
	}
	src, ok := d.Platform.(HoldReader)
	if !ok {
		return nil, nil
	}
	holds, err := src.Holds(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the platform freeze: %w", err)
	}
	return FreezeOf(holds, time.Now(), ids...), nil
}

// FreezeProto is a freeze as a status carries it (status.freeze); nil
// when the platform is not frozen.
func FreezeProto(f *model.Freeze) *cloudv1.Freeze {
	if f == nil {
		return nil
	}
	out := &cloudv1.Freeze{Scope: f.Scope, By: f.By, Reason: f.Reason, LetThrough: f.LetThrough}
	if !f.Since.IsZero() {
		out.Since = timestamppb.New(f.Since)
	}
	if f.Until != nil {
		out.Until = timestamppb.New(*f.Until)
	}
	return out
}

// FrozenBy says who froze the platform, since when, why and until when:
// "frozen by alice since 2026-11-14T09:00:00Z (provider upgrade)".
func FrozenBy(f *model.Freeze) string {
	out := "frozen by " + f.By
	if !f.Since.IsZero() {
		out += " since " + f.Since.UTC().Format(time.RFC3339)
	}
	if f.Reason != "" {
		out += " (" + f.Reason + ")"
	}
	if f.Until != nil {
		out += " until " + f.Until.UTC().Format(time.RFC3339)
	}
	return out
}

// frozenNote is what a held resource's status adds to who froze.
const frozenNote = "the platform is frozen: nothing is created, changed or deleted until it is unfrozen " +
	"(admin unfreeze, or admin unfreeze --cluster/--network <id> for this one)"

func freezeScope(scope string) string {
	if scope == "" || scope == "platform" {
		return "the platform"
	}
	return scope
}

func freezeScopeNote(f *model.Freeze) string {
	if f.Scope == "" || f.Scope == "platform" {
		return frozenNote
	}
	return freezeScope(f.Scope) + " is frozen: nothing is created, changed or deleted until this scope is unfrozen (admin unfreeze --tenant/--network/--cluster <id>)"
}

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

// RowFreeze is how a status reports the freeze: the Frozen condition, and
// what to add to the status message. A held resource says who froze the
// platform: the platform does not act on it. One not held (let through,
// or the platform not frozen) whose resources still carry the providers'
// pause (set by hand, or not lifted yet) says how many. nil and "" when
// nothing is frozen or paused. A freeze is never a failure.
func RowFreeze(f *model.Freeze, views []*cloudv1.ManagedResource) (*cloudv1.Condition, string) {
	n := PausedCount(views)
	if !f.Holds() {
		if n == 0 {
			return nil, ""
		}
		return &cloudv1.Condition{Type: ConditionFrozen, Status: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE,
				Reason:  managed.ReasonPaused,
				Message: fmt.Sprintf("%d of %d resources are paused: their providers act on them once the pause is lifted", n, len(views))},
			fmt.Sprintf("; %d resources paused", n)
	}
	message := FrozenBy(f) + ": " + freezeScopeNote(f)
	if n > 0 {
		message += fmt.Sprintf("; %d of %d resources carry the providers' pause", n, len(views))
	}
	return &cloudv1.Condition{Type: ConditionFrozen, Status: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE,
		Reason: managed.ReasonPaused, Message: message}, "; " + FrozenBy(f) + ": " + freezeScopeNote(f)
}

// FreezeNotes are the Notes of the freeze changing for a resource
// (status.freeze before and after a pass): PlatformFrozen when it starts
// holding it, LetThrough when it lets it through, PlatformUnfrozen when it
// ends. A freeze that goes on says nothing.
func FreezeNotes(before, after *cloudv1.Freeze) []Note {
	held := func(f *cloudv1.Freeze) bool { return f != nil && !f.GetLetThrough() }
	through := func(f *cloudv1.Freeze) bool { return f != nil && f.GetLetThrough() }
	switch {
	case held(after) && !held(before):
		return []Note{Normal(ReasonPlatformFrozen, freezeScope(after.GetScope())+" is frozen by "+after.GetBy()+freezeReason(after)+
			": nothing of it changes until it is unfrozen or let through")}
	case through(after) && !through(before):
		return []Note{Normal(ReasonLetThrough, "freeze exception applied: the platform and providers may act on this resource again")}
	case after == nil && before != nil:
		return []Note{Normal(ReasonPlatformUnfrozen, freezeScope(before.GetScope())+" is unfrozen: what was asked for meanwhile is applied")}
	}
	return nil
}

func freezeReason(f *cloudv1.Freeze) string {
	if f.GetReason() == "" {
		return ""
	}
	return " (" + f.GetReason() + ")"
}

// PauseScope is what SetProvidersPaused covers: with no tenant, every
// tenant's managed resources (the platform freeze), those of the clusters
// and networks in Except left out; with a tenant, that tenant's, of one
// cluster (ClusterID) or one network (NetworkID) when set.
type PauseScope struct {
	TenantID, ClusterID, NetworkID string
	Except                         []string
	Only                           []string
	Targets                        map[string]FreezeTarget
	Select                         func(FreezeTarget) bool
}

// selector is the scope as list options.
func (s PauseScope) selector() []client.ListOption {
	if s.TenantID == "" {
		return []client.ListOption{client.HasLabels{crossplane.LabelTenantID}}
	}
	labels := client.MatchingLabels{crossplane.LabelTenantID: s.TenantID}
	switch {
	case s.ClusterID != "":
		labels[crossplane.LabelCluster] = s.ClusterID
	case s.NetworkID != "":
		labels[crossplane.LabelNetwork] = s.NetworkID
	}
	return []client.ListOption{client.InNamespace(kube.TenantNamespace(s.TenantID)), labels}
}

// excepted reports whether u belongs to a cluster or network the scope
// leaves out.
func (s PauseScope) excepted(u *unstructured.Unstructured) bool {
	labels := u.GetLabels()
	if s.Select != nil {
		target := FreezeTarget{TenantID: labels[crossplane.LabelTenantID], NetworkID: labels[crossplane.LabelNetwork], ClusterID: labels[crossplane.LabelCluster]}
		if known, ok := s.Targets[target.ClusterID]; ok {
			target = known
		} else if known, ok := s.Targets[target.NetworkID]; ok {
			target = known
		}
		if !s.Select(target) {
			return true
		}
	}
	if len(s.Only) > 0 && !slices.Contains(s.Only, labels[crossplane.LabelCluster]) && !slices.Contains(s.Only, labels[crossplane.LabelNetwork]) {
		return true
	}
	return slices.Contains(s.Except, labels[crossplane.LabelCluster]) && labels[crossplane.LabelCluster] != "" ||
		slices.Contains(s.Except, labels[crossplane.LabelNetwork]) && labels[crossplane.LabelNetwork] != ""
}

// SetProvidersPaused sets or removes the providers' pause
// (crossplane.io/paused) on every managed resource of scope and returns
// how many it changed: those in that state already are left, so the call
// can be repeated after a failure. Objects are read from the API server
// (or the cache) and patched one by one. A failed list is
// fault.Unavailable, a failed patch fault.Internal (saying how many were
// done).
func SetProvidersPaused(ctx context.Context, d Deps, scope PauseScope, paused bool) (int32, error) {
	kinds, err := pausedKinds(d)
	if err != nil {
		return 0, fault.New(fault.Internal, err.Error())
	}
	var count int32
	for _, gvk := range kinds {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		err = d.Reader().List(ctx, list, scope.selector()...)
		if meta.IsNoMatchError(err) {
			continue
		}
		if err != nil {
			return count, fault.New(fault.Unavailable, fmt.Sprintf("cannot list %s: %v (%d resources done; the call can be repeated)",
				gvk.Kind, err, count))
		}
		for i := range list.Items {
			u := &list.Items[i]
			if scope.excepted(u) || managed.IsPaused(u) == paused || (!paused && u.GetAnnotations()[freezePausedAnnotation] != "true") {
				continue
			}
			if err := patchProvidersPaused(ctx, d, u, paused); err != nil {
				return count, fault.New(fault.Internal,
					fmt.Sprintf("%s %s: %v (%d resources done; the call can be repeated)", gvk.Kind, u.GetName(), err, count))
			}
			count++
		}
	}
	if d.Logger != nil && count > 0 {
		d.Logger.InfoContext(ctx, "providers' pause set", "tenant_id", scope.TenantID, "cluster_id", scope.ClusterID,
			"network_id", scope.NetworkID, "paused", paused, "resources", count)
	}
	return count, nil
}

// pausedKinds are the kinds the freeze pauses: every managed resource kind
// of the AWS providers the platform renders, and provider-helm's Release
// (a platform component's chart). Usages are not managed resources; nor
// are ProviderConfigs.
func pausedKinds(d Deps) ([]schema.GroupVersionKind, error) {
	if d.ManagedResourceKinds != nil {
		out := slices.Clone(d.ManagedResourceKinds)
		if !slices.Contains(out, crossplaneaws.HelmReleaseGVK) {
			out = append(out, crossplaneaws.HelmReleaseGVK)
		}
		return out, nil
	}
	objects := crossplaneaws.ManagedObjects()
	out := make([]schema.GroupVersionKind, 0, len(objects)+1)
	for _, obj := range objects {
		gvk, err := apiutil.GVKForObject(obj, d.Kube.Scheme())
		if err != nil {
			return nil, err
		}
		if gvk.Kind != "Usage" {
			out = append(out, gvk)
		}
	}
	return append(out, crossplaneaws.HelmReleaseGVK), nil
}

func patchProvidersPaused(ctx context.Context, d Deps, u *unstructured.Unstructured, paused bool) error {
	before := client.MergeFrom(u.DeepCopy())
	annotations := u.GetAnnotations()
	if paused {
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[managed.PausedAnnotation] = "true"
		annotations[freezePausedAnnotation] = "true"
	} else {
		delete(annotations, managed.PausedAnnotation)
		delete(annotations, freezePausedAnnotation)
	}
	u.SetAnnotations(annotations)
	return client.IgnoreNotFound(d.Kube.Patch(ctx, u, before))
}
func WithoutFreezeMessage(message string, before *cloudv1.Freeze) string {
	if before == nil {
		return message
	}
	frozen := &model.Freeze{Scope: before.GetScope(), By: before.GetBy(), Reason: before.GetReason()}
	if before.GetSince() != nil {
		frozen.Since = before.GetSince().AsTime()
	}
	if before.GetUntil() != nil {
		until := before.GetUntil().AsTime()
		frozen.Until = &until
	}
	for _, text := range []string{freezeScopeNote(frozen), frozenNote} {
		note := FrozenBy(frozen) + ": " + text
		message = strings.ReplaceAll(message, "; "+note, "")
		message = strings.TrimPrefix(message, note)
	}
	message = strings.TrimPrefix(message, "; ")
	return strings.TrimSuffix(message, "; the platform is frozen: pending changes apply after admin unfreeze")
}
