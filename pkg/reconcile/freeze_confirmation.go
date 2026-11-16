package reconcile

import (
	"context"
	"fmt"
	"math"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
)

type PauseCounts struct{ Total, Paused, Owned int32 }

func ProviderPauseCounts(ctx context.Context, d Deps, scope PauseScope) (PauseCounts, error) {
	var counts PauseCounts
	kinds, err := pausedKinds(d)
	if err != nil {
		return counts, err
	}
	for _, kind := range kinds {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(kind.GroupVersion().WithKind(kind.Kind + "List"))
		if err := d.Reader().List(ctx, list, scope.selector()...); err != nil {
			if meta.IsNoMatchError(err) {
				continue
			}
			return counts, fmt.Errorf("read %s pause status: %w", kind.Kind, err)
		}
		for index := range list.Items {
			resource := &list.Items[index]
			if scope.excepted(resource) {
				continue
			}
			counts.Total++
			if managed.IsPaused(resource) {
				counts.Paused++
				if resource.GetAnnotations()[freezePausedAnnotation] == "true" {
					counts.Owned++
				}
			}
		}
	}
	return counts, nil
}

func ConfirmFreeze(ctx context.Context, d Deps, now time.Time) (time.Duration, error) {
	release := d.FreezeBarrier.Drain()
	defer release()
	hold := &cloudv1alpha1.ReleaseHold{}
	if err := d.Reader().Get(ctx, client.ObjectKey{Name: FreezeName}, hold); err != nil {
		if !apierrors.IsNotFound(err) {
			return 0, err
		}
		_, err = SetProvidersPaused(ctx, d, PauseScope{}, false)
		return 0, err
	}

	// Publish the drain barrier independently of provider pause progress.
	// A selected scope can confirm from this acknowledgement and live pause
	// counts even if an unrelated scope encounters a pause failure.
	if hold.Status.DrainedRevision != hold.Spec.Revision || hold.Status.DrainedGeneration != hold.Generation {
		before := hold.DeepCopy()
		hold.Status.DrainedRevision = hold.Spec.Revision
		hold.Status.DrainedGeneration = hold.Generation
		if err := d.Kube.Status().Patch(ctx, hold, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return 0, err
		}
	}
	targets, err := d.FreezeTargets(ctx)
	if err != nil {
		return 0, err
	}
	scope := HeldFreezeScope(hold, targets, now)
	changed, err := SetProvidersPaused(ctx, d, scope, true)
	if err != nil {
		return 0, err
	}
	thawed := PauseScope{Targets: targets, Select: func(target FreezeTarget) bool { return !FreezeForTarget(hold, now, target).Holds() }}
	if _, err := SetProvidersPaused(ctx, d, thawed, false); err != nil {
		return 0, err
	}
	counts, err := ProviderPauseCounts(ctx, d, scope)
	if err != nil {
		return 0, err
	}
	active := EffectiveFreezeActive(hold, targets, now) || counts.Total > 0
	before := hold.DeepCopy()
	hold.Status.FreezePhase = "Unfrozen"
	if active {
		hold.Status.FreezePhase = "Freezing"
		if counts.Total == counts.Paused {
			hold.Status.FreezePhase = "Frozen"
		}
	}
	hold.Status.FreezeRevision = hold.Spec.Revision
	hold.Status.ObservedGeneration = hold.Generation
	hold.Status.Resources, hold.Status.PausedResources = counts.Total, counts.Paused
	if changed > 0 && active {
		hold.Status.NewlyPausedResources = changed
	} else if before.Status.FreezeRevision != hold.Spec.Revision {
		hold.Status.NewlyPausedResources = 0
	}
	clusters := &cloudv1alpha1.ClusterList{}
	if err := d.Reader().List(ctx, clusters); err != nil {
		return 0, err
	}
	heldClusters := 0
	for _, cluster := range clusters.Items {
		if FreezeForTarget(hold, now, targets[cluster.Labels["cloud.cluster-vending.io/id"]]).Holds() {
			heldClusters++
		}
	}
	hold.Status.Clusters = int32(min(heldClusters, math.MaxInt32))
	if !equality.Semantic.DeepEqual(before.Status, hold.Status) {
		if err := d.Kube.Status().Patch(ctx, hold, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return 0, err
		}
	}
	var next time.Duration
	consider := func(until *metav1.Time) {
		if until != nil && until.After(now) {
			wait := max(until.Sub(now), time.Second)
			if next == 0 || wait < next {
				next = wait
			}
		}
	}
	consider(hold.Spec.Until)
	for _, rule := range hold.Spec.Rules {
		consider(rule.Until)
	}
	return next, nil
}
