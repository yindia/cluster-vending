package subtenant

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"connectrpc.com/connect/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
)

// Name claims are tenant-local but keyed by the workload cluster incarnation.
// Never reclaim by age: an ownerless claim may precede a persisted CR or remote
// writes. The same request can recover it; other requests must fail closed.
func nameClaimKey(namespace, clusterUID, name string) client.ObjectKey {
	digest := sha256.Sum256([]byte(clusterUID + "\x00" + name))
	return client.ObjectKey{Namespace: namespace, Name: "subtenant-name-" + hex.EncodeToString(digest[:20])}
}

func (s *Service) reserveName(ctx context.Context, namespace string, tenant *cloud.Tenant, parent *cloud.Cluster, name, id, hash string) error {
	if name == "" { // Legacy unnamed allocations have no new naming claim.
		return nil
	}
	key := nameClaimKey(namespace, string(parent.UID), name)
	desired := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: namespace,
		Labels: map[string]string{cloudstore.LabelTenant: tenant.Name}},
		Data: map[string]string{"name": name, "clusterUID": string(parent.UID), "tenantUID": string(tenant.UID), "allocationID": id, "requestHash": hash}}
	if err := s.deps.Kube.Create(ctx, desired); err == nil {
		return nil
	} else if !apierrors.IsAlreadyExists(err) {
		return kubeError(err)
	}
	existing := &corev1.ConfigMap{}
	// Use the live client, never a cached list/get, for claim decisions.
	if err := s.deps.Kube.Get(ctx, key, existing); err != nil {
		return kubeError(err)
	}
	if !existing.DeletionTimestamp.IsZero() || existing.Labels[cloudstore.LabelTenant] != tenant.Name ||
		existing.Data["name"] != name || existing.Data["clusterUID"] != string(parent.UID) || existing.Data["tenantUID"] != string(tenant.UID) ||
		existing.Data["allocationID"] != id || existing.Data["requestHash"] != hash {
		return connect.NewError(connect.CodeAlreadyExists, "subtenant name is already reserved in this workload cluster")
	}
	if len(existing.OwnerReferences) > 0 {
		owner := &cloud.SubTenant{}
		err := s.deps.Kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: id}, owner)
		if err != nil && !apierrors.IsNotFound(err) {
			return kubeError(err)
		}
		ref := existing.OwnerReferences[0]
		if err != nil || len(existing.OwnerReferences) != 1 || ref.APIVersion != cloud.SchemeGroupVersion.String() || ref.Kind != "SubTenant" || ref.Name != id || ref.UID != owner.UID || !owner.DeletionTimestamp.IsZero() {
			return connect.NewError(connect.CodeAlreadyExists, "subtenant name claim belongs to a terminating or previous allocation; wait for verified cleanup")
		}
	}
	return nil
}

// Binding makes Kubernetes GC release the claim only after the allocation CR
// actually disappears, including its require-empty finalizer. A crash before
// binding is recoverable by the exact create retry; no timer releases the claim.
func (s *Service) bindNameClaim(ctx context.Context, obj *cloud.SubTenant, hash string) error {
	if obj.Spec.Name == "" {
		return nil
	}
	if obj.UID == "" {
		return connect.NewError(connect.CodeUnavailable, "allocation has no UID yet; retry the same create request to bind its name claim")
	}
	key := nameClaimKey(obj.Namespace, obj.Spec.ClusterRef.UID, obj.Spec.Name)
	return kubeError(retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		claim := &corev1.ConfigMap{}
		if err := s.deps.Kube.Get(ctx, key, claim); err != nil {
			return err
		}
		if !claim.DeletionTimestamp.IsZero() || claim.Data["name"] != obj.Spec.Name || claim.Data["clusterUID"] != obj.Spec.ClusterRef.UID ||
			claim.Data["tenantUID"] != obj.Spec.TenantRef.UID || claim.Data["allocationID"] != obj.Name || claim.Data["requestHash"] != hash {
			return connect.NewError(connect.CodeAlreadyExists, "allocation name claim no longer matches this request")
		}
		owner := metav1.OwnerReference{APIVersion: cloud.SchemeGroupVersion.String(), Kind: "SubTenant", Name: obj.Name, UID: obj.UID}
		if len(claim.OwnerReferences) > 0 {
			if len(claim.OwnerReferences) == 1 && claim.OwnerReferences[0].APIVersion == owner.APIVersion && claim.OwnerReferences[0].Kind == owner.Kind && claim.OwnerReferences[0].Name == owner.Name && claim.OwnerReferences[0].UID == owner.UID {
				return nil
			}
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Sprintf("name claim for %s has a conflicting owner; manual investigation is required", obj.Spec.Name))
		}
		before := claim.DeepCopy()
		claim.OwnerReferences = []metav1.OwnerReference{owner}
		return s.deps.Kube.Patch(ctx, claim, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	}))
}
