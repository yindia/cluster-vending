package reconcile

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/fault"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// SetProvidersPaused sets or removes the providers' pause
// (crossplane.io/paused) on every managed resource of target (the
// tenant's, or one network's or cluster's) and returns how many are in
// that state now. Objects are read from the API server and patched one by
// one. A tenant's resume leaves the annotation on the resources of a
// network or cluster that is still paused on its own. The annotation is
// the admin's: the platform's applies do not own it, so they neither set
// nor remove it. A failed list is fault.Unavailable, a failed patch
// fault.Internal (saying how many were done: the call can be repeated).
func SetProvidersPaused(ctx context.Context, d Deps, tenantID string, target model.PauseTarget, paused bool) (int32, error) {
	selector := client.MatchingLabels{crossplane.LabelTenantID: tenantID}
	switch {
	case target.ClusterID != "":
		selector[crossplane.LabelCluster] = target.ClusterID
	case target.NetworkID != "":
		selector[crossplane.LabelNetwork] = target.NetworkID
	}
	keep := stillPaused{d: d, tenantID: tenantID, rows: map[string]bool{}, check: !paused && target.Scope() == model.PauseTenant}
	kinds, err := pausedKinds(d)
	if err != nil {
		return 0, fault.New(fault.Internal, err.Error())
	}
	var count int32
	for _, gvk := range kinds {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		err = d.Reader().List(ctx, list, client.InNamespace(kube.TenantNamespace(tenantID)), selector)
		if meta.IsNoMatchError(err) {
			continue
		}
		if err != nil {
			return count, fault.New(fault.Unavailable, fmt.Sprintf("cannot list %s: %v", gvk.Kind, err))
		}
		for i := range list.Items {
			stays, err := keep.holds(ctx, &list.Items[i])
			if err != nil {
				return count, fault.New(fault.Unavailable, err.Error())
			}
			if stays {
				continue
			}
			if err := patchProvidersPaused(ctx, d, &list.Items[i], paused); err != nil {
				return count, fault.New(fault.Internal,
					fmt.Sprintf("%s %s: %v (%d resources done; the call can be repeated)", gvk.Kind, list.Items[i].GetName(), err, count))
			}
			count++
		}
	}
	if d.Logger != nil {
		d.Logger.InfoContext(ctx, "reconciliation paused state set", "tenant_id", tenantID, "cluster_id", target.ClusterID,
			"network_id", target.NetworkID, "paused", paused, "resources", count)
	}
	return count, nil
}

// LiftLapsedPause writes the pause of one cluster or network again
// without its scopes whose until has passed (Tenants.DropLapsedPause) and,
// when no pause is left (stillPaused false), resumes the providers on its
// managed resources: a pause asked with an until ends by itself.
func LiftLapsedPause(ctx context.Context, d Deps, tenantID string, target model.PauseTarget, stillPaused bool) error {
	if err := d.Store.Tenants().DropLapsedPause(ctx, tenantID, target); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("drop the pause that ended: %w", err)
	}
	if stillPaused {
		return nil
	}
	if _, err := SetProvidersPaused(ctx, d, tenantID, target, false); err != nil {
		return fmt.Errorf("resume the providers after the pause ended: %w", err)
	}
	return nil
}

// pausedKinds are the kinds a pause covers: every managed resource kind of
// the AWS providers the platform renders, and provider-helm's Release (a
// platform component's chart). Usages are not managed resources; nor are
// ProviderConfigs.
func pausedKinds(d Deps) ([]schema.GroupVersionKind, error) {
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
	_, has := u.GetAnnotations()[managed.PausedAnnotation]
	if has == paused {
		return nil
	}
	before := client.MergeFrom(u.DeepCopy())
	annotations := u.GetAnnotations()
	if paused {
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[managed.PausedAnnotation] = "true"
	} else {
		delete(annotations, managed.PausedAnnotation)
	}
	u.SetAnnotations(annotations)
	return client.IgnoreNotFound(d.Kube.Patch(ctx, u, before))
}

// stillPaused tells a tenant's resume which managed resources keep the
// providers' pause: those of a network or cluster that is still paused on
// its own (its row's pause, read once per row). With check false it keeps
// none.
type stillPaused struct {
	d        Deps
	tenantID string
	rows     map[string]bool
	check    bool
}

// holds reports whether u belongs to a network or cluster still paused.
func (k stillPaused) holds(ctx context.Context, u *unstructured.Unstructured) (bool, error) {
	if !k.check {
		return false, nil
	}
	labels := u.GetLabels()
	if id := labels[crossplane.LabelCluster]; id != "" {
		return k.row("cluster/"+id, func() (*model.Pause, error) {
			c, err := k.d.Store.Clusters().Get(ctx, k.tenantID, id)
			if err != nil {
				return nil, err
			}
			return c.Paused, nil
		})
	}
	if id := labels[crossplane.LabelNetwork]; id != "" {
		return k.row("network/"+id, func() (*model.Pause, error) {
			n, err := k.d.Store.Networks().Get(ctx, k.tenantID, id)
			if err != nil {
				return nil, err
			}
			return n.Paused, nil
		})
	}
	return false, nil
}

func (k stillPaused) row(key string, read func() (*model.Pause, error)) (bool, error) {
	if paused, ok := k.rows[key]; ok {
		return paused, nil
	}
	pause, err := read()
	if errors.Is(err, repository.ErrNotFound) {
		pause, err = nil, nil // gone: nothing holds its resources
	}
	if err != nil {
		return false, fmt.Errorf("cannot read %s: %w", key, err)
	}
	k.rows[key] = pause != nil
	return pause != nil, nil
}
