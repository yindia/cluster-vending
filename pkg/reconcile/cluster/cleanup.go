package cluster

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// helmRelease is provider-helm's namespaced Release: charts installed into
// a tenant cluster through its kubeconfig Secret.
var helmRelease = schema.GroupVersionKind{Group: "helm.m.crossplane.io", Version: "v1beta1", Kind: "Release"}

// forceRelease is a forced deletion's extra step (the cluster's API may be
// unreachable): Helm Releases into the cluster are taken out of the way,
// managementPolicies without Delete so removing them does not try to
// uninstall, then removed. Idempotent; a forced deletion runs it on every
// step.
func (s *Reconciler) forceRelease(ctx context.Context, row *model.Cluster) error {
	releases := &unstructured.UnstructuredList{}
	releases.SetGroupVersionKind(helmRelease.GroupVersion().WithKind(helmRelease.Kind + "List"))
	err := s.deps.Reader().List(ctx, releases, client.InNamespace(kube.TenantNamespace(row.TenantID)),
		client.MatchingLabels{crossplane.LabelCluster: row.ID})
	if meta.IsNoMatchError(err) {
		return nil // provider-helm is not installed: no Releases
	}
	if err != nil {
		return fmt.Errorf("list helm releases: %w", err)
	}
	var errs []error
	for i := range releases.Items {
		r := &releases.Items[i]
		patch := client.MergeFrom(r.DeepCopy())
		if err := unstructured.SetNestedStringSlice(r.Object, []string{"Observe", "Create", "Update", "LateInitialize"},
			"spec", "managementPolicies"); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := client.IgnoreNotFound(s.deps.Kube.Patch(ctx, r, patch)); err != nil {
			errs = append(errs, fmt.Errorf("release helm release %s: %w", r.GetName(), err))
			continue
		}
		errs = append(errs, client.IgnoreNotFound(s.deps.Kube.Delete(ctx, r)))
	}
	return errors.Join(errs...)
}
