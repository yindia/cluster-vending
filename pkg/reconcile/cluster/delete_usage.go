package cluster

import (
	"context"
	"fmt"
	"strings"
	"time"

	protectionv1beta1 "github.com/crossplane/crossplane/apis/v2/protection/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/helmrel"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// usageHolders are the objects of a cluster that a Usage keeps: what the
// Crossplane Usage webhook refuses to delete, and the object that uses it
// (its "by") for as long as that one exists. It is the order the webhook
// enforces, requirements and sequences alike, so the platform sends no
// delete the webhook would refuse.
type usageHolders map[string][]string // "Kind/name" -> what uses it, as it is called

// by is what holds the object, as a name for a message; "" when nothing
// does.
func (h usageHolders) by(kind, name string) string {
	if users := h[viewKey(kind, name)]; len(users) > 0 {
		return strings.Join(users, ", ")
	}
	return ""
}

// usageHolders reads the cluster's Usages and, for each, whether its user
// still exists. A user is named by its component label when it has one.
func (s *Reconciler) usageHolders(ctx context.Context, namespace, clusterID string) (usageHolders, error) {
	usages := &protectionv1beta1.UsageList{}
	err := s.deps.Reader().List(ctx, usages, client.InNamespace(namespace), client.MatchingLabels{crossplane.LabelCluster: clusterID})
	if meta.IsNoMatchError(err) {
		return usageHolders{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list the cluster's Usages: %w", err)
	}
	out := usageHolders{}
	for i := range usages.Items {
		u := &usages.Items[i]
		if u.Spec.By == nil || u.Spec.By.ResourceRef == nil || u.Spec.Of.ResourceRef == nil {
			continue
		}
		gv, err := schema.ParseGroupVersion(u.Spec.By.APIVersion)
		if err != nil {
			continue
		}
		by := &unstructured.Unstructured{}
		by.SetGroupVersionKind(gv.WithKind(u.Spec.By.Kind))
		err = s.deps.Reader().Get(ctx, client.ObjectKey{Namespace: u.Namespace, Name: u.Spec.By.ResourceRef.Name}, by)
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			continue // its user is gone: it holds nothing
		}
		if err != nil {
			return nil, fmt.Errorf("read %s %s, which uses %s %s: %w", u.Spec.By.Kind, u.Spec.By.ResourceRef.Name,
				u.Spec.Of.Kind, u.Spec.Of.ResourceRef.Name, err)
		}
		name := u.Spec.By.ResourceRef.Name
		if component := by.GetLabels()[crossplane.LabelComponent]; component != "" {
			name = component
		}
		key := viewKey(u.Spec.Of.Kind, u.Spec.Of.ResourceRef.Name)
		out[key] = append(out[key], name)
	}
	return out, nil
}

// liftPlatformPauses removes the platform's own Helm backoff pause and its
// give-up mark from every Release of the cluster (only where the platform's
// marker is on it): nothing of the platform's reliability machinery may hold
// an uninstall, and which Release a pass reaches must not matter. A Release
// paused by someone else is left; prepareReleases reports it when its
// component's turn comes.
func (s *Reconciler) liftPlatformPauses(ctx context.Context, row *model.Cluster) error {
	releases := &unstructured.UnstructuredList{}
	releases.SetGroupVersionKind(helmRelease.GroupVersion().WithKind(helmRelease.Kind + "List"))
	err := s.deps.Reader().List(ctx, releases, client.InNamespace(kube.TenantNamespace(row.TenantID)),
		client.MatchingLabels{crossplane.LabelCluster: row.ID})
	if meta.IsNoMatchError(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list helm releases: %w", err)
	}
	for i := range releases.Items {
		r := &releases.Items[i]
		annotations := r.GetAnnotations()
		if annotations[helmrel.BackoffAnnotation] == "" && annotations[helmrel.GaveUpAnnotation] == "" {
			continue
		}
		s.setBackoff(ctx, row, r.GetNamespace(), r.GetName(), time.Time{}, false)
	}
	return nil
}
