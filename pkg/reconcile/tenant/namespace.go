// Package tenant owns the Kubernetes namespace lifecycle of a tenant. It has no
// dependency on the cloudstore or on account/provider reconciliation.
package tenant

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/kube"
)

const (
	LabelTenant           = "cloud.cluster-vending.io/tenant"
	AnnotationDisplayName = "cloud.cluster-vending.io/display-name"
	AnnotationCreatedAt   = "cloud.cluster-vending.io/created-at"
	// AnnotationRegistration marks a namespace managed by the Tenant CR lifecycle.
	// Its presence prevents legacy migration from resurrecting a deleted CR.
	AnnotationRegistration = "cloud.cluster-vending.io/tenant-registration"
	// AnnotationRetainedTenant proves that an unregistered namespace was retained
	// by this platform on deletion. It permits safe re-registration of that tenant
	// without adopting an arbitrary unmarked namespace.
	AnnotationRetainedTenant = "cloud.cluster-vending.io/retained-tenant"
)

var ErrNamespaceCollision = errors.New("tenant namespace is not owned by this tenant")

// ValidID matches the API scope and leaves room for the tenant- prefix.
func ValidID(id string) bool {
	return id != "" && len(id) <= 56 && len(validation.IsDNS1123Label(id)) == 0
}

// EnsureNamespace creates the namespace only for a Tenant registration. Existing
// namespaces need an explicit matching ownership label or retained-tenant marker.
// It never sets owner references, since deleting a Tenant must not cascade.
func EnsureNamespace(ctx context.Context, c client.Client, obj *cloudv1alpha1.Tenant) (*corev1.Namespace, error) {
	if !ValidID(obj.Name) || obj.Namespace != "" {
		return nil, fmt.Errorf("invalid cluster-scoped tenant %q", obj.Name)
	}
	if !obj.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("tenant %s is deleting", obj.Name)
	}
	var result *corev1.Namespace
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		ns := &corev1.Namespace{}
		err := c.Get(ctx, client.ObjectKey{Name: kube.TenantNamespace(obj.Name)}, ns)
		if apierrors.IsNotFound(err) {
			ns.Name = kube.TenantNamespace(obj.Name)
			setMetadata(ns, obj)
			if err = c.Create(ctx, ns); apierrors.IsAlreadyExists(err) {
				return apierrors.NewConflict(corev1.Resource("namespaces"), ns.Name, err)
			}
			result = ns
			return err
		}
		if err != nil {
			return err
		}
		owner := ns.Labels[LabelTenant]
		retained := ns.Annotations[AnnotationRetainedTenant]
		if (owner != obj.Name && (owner != "" || retained != obj.Name)) || len(ns.OwnerReferences) != 0 || !ns.DeletionTimestamp.IsZero() || ns.Status.Phase == corev1.NamespaceTerminating {
			return fmt.Errorf("%w: namespace %s", ErrNamespaceCollision, ns.Name)
		}
		before := ns.DeepCopy()
		setMetadata(ns, obj)
		if reflect.DeepEqual(before.Labels, ns.Labels) && reflect.DeepEqual(before.Annotations, ns.Annotations) {
			result = ns
			return nil
		}
		if err := c.Patch(ctx, ns, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		result = ns
		return nil
	})
	return result, err
}

// MigrateLegacy creates a canonical registration only for the pre-existing,
// explicitly labelled tenant namespace. It never changes the namespace.
func MigrateLegacy(ctx context.Context, c client.Client, id string) (*cloudv1alpha1.Tenant, error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("invalid tenant ID %q", id)
	}
	ns := &corev1.Namespace{}
	if err := c.Get(ctx, client.ObjectKey{Name: kube.TenantNamespace(id)}, ns); err != nil {
		return nil, err
	}
	if ns.Labels[LabelTenant] != id || ns.Annotations[AnnotationRetainedTenant] != "" || ns.Annotations[AnnotationRegistration] != "" || len(ns.OwnerReferences) != 0 || !ns.DeletionTimestamp.IsZero() || ns.Status.Phase == corev1.NamespaceTerminating {
		return nil, fmt.Errorf("%w: namespace %s is not an active legacy tenant", ErrNamespaceCollision, ns.Name)
	}
	created := ns.Annotations[AnnotationCreatedAt]
	if created == "" && !ns.CreationTimestamp.IsZero() {
		created = ns.CreationTimestamp.UTC().Format(time.RFC3339Nano)
	}
	obj := &cloudv1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: id, Annotations: map[string]string{AnnotationCreatedAt: created}}, Spec: cloudv1alpha1.TenantSpec{DisplayName: ns.Annotations[AnnotationDisplayName]}}
	if err := c.Create(ctx, obj); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, err
		}
		if err := c.Get(ctx, client.ObjectKey{Name: id}, obj); err != nil {
			return nil, err
		}
	}
	return obj, nil
}

func setMetadata(ns *corev1.Namespace, obj *cloudv1alpha1.Tenant) {
	if ns.Labels == nil {
		ns.Labels = map[string]string{}
	}
	if ns.Annotations == nil {
		ns.Annotations = map[string]string{}
	}
	ns.Labels[LabelTenant] = obj.Name
	ns.Annotations[AnnotationRegistration] = string(obj.UID)
	if ns.Annotations[AnnotationRegistration] == "" {
		ns.Annotations[AnnotationRegistration] = obj.Name
	}
	ns.Annotations[AnnotationDisplayName] = obj.Spec.DisplayName
	if ns.Annotations[AnnotationDisplayName] == "" {
		ns.Annotations[AnnotationDisplayName] = obj.Name
	}
	if ns.Annotations[AnnotationCreatedAt] == "" {
		created := obj.Annotations[AnnotationCreatedAt]
		if created == "" && !obj.CreationTimestamp.IsZero() {
			created = obj.CreationTimestamp.UTC().Format(time.RFC3339Nano)
		}
		if created != "" {
			ns.Annotations[AnnotationCreatedAt] = created
		}
	}
	delete(ns.Annotations, AnnotationRetainedTenant)
}

// RetainNamespace unregisters a known-owned namespace without deleting its
// contents. A marker permits a later registration of this same tenant only.
func RetainNamespace(ctx context.Context, c client.Client, id string) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		ns := &corev1.Namespace{}
		if err := c.Get(ctx, client.ObjectKey{Name: kube.TenantNamespace(id)}, ns); err != nil {
			return client.IgnoreNotFound(err)
		}
		if ns.Labels[LabelTenant] == "" && ns.Annotations[AnnotationRetainedTenant] == id {
			return nil
		}
		if ns.Labels[LabelTenant] != id || len(ns.OwnerReferences) != 0 {
			return fmt.Errorf("%w: namespace %s", ErrNamespaceCollision, ns.Name)
		}
		before := ns.DeepCopy()
		delete(ns.Labels, LabelTenant)
		if ns.Annotations == nil {
			ns.Annotations = map[string]string{}
		}
		ns.Annotations[AnnotationRetainedTenant] = id
		delete(ns.Annotations, AnnotationDisplayName)
		delete(ns.Annotations, AnnotationCreatedAt)
		return c.Patch(ctx, ns, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	})
}

// NamespaceFor documents the deterministic namespace name for callers.
func NamespaceFor(id string) string { return kube.TenantNamespace(id) }

// Timestamp returns creation metadata consistently for API-created and YAML CRs.
func Timestamp(obj *cloudv1alpha1.Tenant) time.Time {
	if value, err := time.Parse(time.RFC3339Nano, obj.Annotations[AnnotationCreatedAt]); err == nil {
		return value
	}
	return obj.CreationTimestamp.Time
}

// Condition is shared by synchronous creation and controller status reporting.
func Condition(obj *cloudv1alpha1.Tenant, status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{Type: "Ready", Status: status, ObservedGeneration: obj.Generation, LastTransitionTime: metav1.Now(), Reason: reason, Message: message}
}
