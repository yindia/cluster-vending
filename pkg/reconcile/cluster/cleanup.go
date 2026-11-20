package cluster

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// helmRelease is provider-helm's namespaced Release: charts installed into
// a tenant cluster through its kubeconfig Secret.
var helmRelease = schema.GroupVersionKind{Group: "helm.m.crossplane.io", Version: "v1beta1", Kind: "Release"}

// noDeletePolicies are the management policies a Release is let go with:
// everything but Delete, so removing it does not uninstall the chart.
var noDeletePolicies = []string{"Observe", "Create", "Update", "LateInitialize"}

// forceRelease is a forced deletion's extra step (the cluster's API may be
// unreachable): Helm Releases into the cluster are taken out of the way,
// managementPolicies without Delete so removing them does not try to
// uninstall, then removed. Idempotent; it runs on every step of a deletion
// that lets its Releases go.
func (s *Reconciler) forceRelease(ctx context.Context, row *model.Cluster) error {
	_, err := s.letGoReleases(ctx, row)
	return err
}

// letGoReleases is forceRelease; it reports how many Releases it took out of
// the way for the first time.
func (s *Reconciler) letGoReleases(ctx context.Context, row *model.Cluster) (int, error) {
	releases := &unstructured.UnstructuredList{}
	releases.SetGroupVersionKind(helmRelease.GroupVersion().WithKind(helmRelease.Kind + "List"))
	err := s.deps.Reader().List(ctx, releases, client.InNamespace(kube.TenantNamespace(row.TenantID)),
		client.MatchingLabels{crossplane.LabelCluster: row.ID})
	if meta.IsNoMatchError(err) {
		return 0, nil // provider-helm is not installed: no Releases
	}
	if err != nil {
		return 0, fmt.Errorf("list helm releases: %w", err)
	}
	held, err := s.usageHolders(ctx, kube.TenantNamespace(row.TenantID), row.ID)
	if err != nil {
		return 0, err
	}
	var (
		errs    []error
		changed int
	)
	for i := range releases.Items {
		r := &releases.Items[i]
		have, _, _ := unstructured.NestedStringSlice(r.Object, "spec", "managementPolicies")
		if !slices.Equal(have, noDeletePolicies) {
			patch := client.MergeFrom(r.DeepCopy())
			if err := unstructured.SetNestedStringSlice(r.Object, slices.Clone(noDeletePolicies), "spec", "managementPolicies"); err != nil {
				errs = append(errs, err)
				continue
			}
			if err := client.IgnoreNotFound(s.deps.Kube.Patch(ctx, r, patch)); err != nil {
				errs = append(errs, fmt.Errorf("release helm release %s: %w", r.GetName(), err))
				continue
			}
			changed++
		}
		// Never a delete the Usage webhook would refuse: a Release that
		// something still uses goes once its users are gone.
		if held.by(helmRelease.Kind, r.GetName()) != "" {
			continue
		}
		if r.GetDeletionTimestamp() != nil {
			continue
		}
		if err := s.deps.Kube.Delete(ctx, r); err != nil && !managed.IsUsageRefusal(err) {
			errs = append(errs, client.IgnoreNotFound(err))
		}
	}
	return changed, errors.Join(errs...)
}
