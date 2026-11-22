package naas

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
)

const QualificationNamespace = "naas-system"
const QualificationKey = "qualification.json"

// QualificationBinding identifies the exact configuration exercised by a
// trusted operator. Values are reconstructed from fresh reads by the verifier,
// not accepted from SubTenant status, annotations, or tenant-facing requests.
type QualificationBinding struct {
	AllocationUID     string            `json:"allocationUID"`
	TenantUID         string            `json:"tenantUID"`
	ClusterID         string            `json:"clusterID"`
	ClusterUID        string            `json:"clusterUID"`
	RemoteTenantUID   string            `json:"remoteTenantUID"`
	NamespaceUIDs     map[string]string `json:"namespaceUIDs"`
	BootstrapRevision string            `json:"bootstrapRevision"`
	SpecHash          string            `json:"specHash"`
	OIDCRevision      string            `json:"oidcRevision"`
	AssociationUID    string            `json:"associationUID"`
}

type AccessQualification struct {
	Version   string               `json:"version"`
	Binding   QualificationBinding `json:"binding"`
	IssuedAt  time.Time            `json:"issuedAt"`
	ExpiresAt time.Time            `json:"expiresAt"`
	Evidence  string               `json:"evidence"`
	Checks    map[string]bool      `json:"checks"`
}

// RequiredQualificationChecks are assertions made only by a trusted runtime
// qualification operator. Static template matching never manufactures them.
func RequiredQualificationChecks() []string {
	return []string{"namespace-rbac", "cross-namespace-rbac", "admission-guards", "ambient-mtls-hbone", "cross-subtenant-traffic", "dns-udp-tcp", "egress-default-deny", "restricted-pss", "shared-quota", "group-delegation-approved"}
}

func QualificationName(allocationUID string) string {
	digest := sha256.Sum256([]byte(allocationUID))
	return "naas-qualified-" + hex.EncodeToString(digest[:16])
}

func BindAccessQualification(sub *cloud.SubTenant, parent *cloud.Cluster, remote *unstructured.Unstructured, inventory []cloud.SubTenantNamespaceStatus, revision string, access *ResolvedAccess, associationUID string) (QualificationBinding, error) {
	binding := QualificationBinding{AllocationUID: string(sub.UID), TenantUID: sub.Spec.TenantRef.UID,
		ClusterID: sub.Spec.ClusterRef.ID, ClusterUID: string(parent.UID),
		RemoteTenantUID: string(remote.GetUID()), NamespaceUIDs: map[string]string{}, BootstrapRevision: revision,
		SpecHash: remote.GetAnnotations()[SpecAnnotation], OIDCRevision: access.OIDCRevision, AssociationUID: associationUID}
	if binding.AllocationUID == "" || binding.ClusterUID == "" || binding.RemoteTenantUID == "" || binding.TenantUID == "" ||
		binding.AssociationUID == "" || binding.SpecHash == "" || binding.OIDCRevision == "" || binding.BootstrapRevision == "" {
		return binding, fmt.Errorf("qualification requires complete current identity and revision bindings")
	}
	for _, key := range sub.Spec.NamespaceKeys {
		name := remote.GetName() + "-" + string(key)
		for _, namespace := range inventory {
			if namespace.Name == name && namespace.Key == key && namespace.UID != "" {
				binding.NamespaceUIDs[name] = namespace.UID
			}
		}
	}
	if len(binding.NamespaceUIDs) != len(sub.Spec.NamespaceKeys) {
		return binding, fmt.Errorf("qualification requires every allocated namespace's current UID")
	}
	return binding, nil
}

// VerifyAccessQualification reads only the protected workload namespace. Fixed
// SubTenant roles never grant permissions in this namespace. A platform admin
// or narrowly authorized qualifier writes this record after actual tests.
// Expiry revocation is reconciliation-bound, not a Kubernetes RBAC hard TTL:
// the expiry is returned so the caller looks again no later than then.
func VerifyAccessQualification(ctx context.Context, workload client.Reader, expected QualificationBinding, now time.Time) (time.Time, error) {
	if expected.AllocationUID == "" {
		return time.Time{}, fmt.Errorf("qualification allocation identity is missing")
	}
	namespace := &corev1.Namespace{}
	if err := workload.Get(ctx, client.ObjectKey{Name: QualificationNamespace}, namespace); err != nil {
		return time.Time{}, fmt.Errorf("protected qualification namespace is unavailable: %w", err)
	}
	if namespace.UID == "" || !namespace.DeletionTimestamp.IsZero() || namespace.Labels["capsule.clastix.io/tenant"] != "" || namespace.Labels[LabelSubTenantID] != "" {
		return time.Time{}, fmt.Errorf("qualification namespace must be an active platform namespace outside Capsule tenant ownership")
	}
	for _, owner := range namespace.OwnerReferences {
		if owner.Kind == "Tenant" && owner.APIVersion == TenantGVK.GroupVersion().String() {
			return time.Time{}, fmt.Errorf("qualification namespace must not be owned by a Capsule Tenant")
		}
	}
	cm := &corev1.ConfigMap{}
	if err := workload.Get(ctx, client.ObjectKey{Namespace: QualificationNamespace, Name: QualificationName(expected.AllocationUID)}, cm); err != nil {
		return time.Time{}, fmt.Errorf("read protected runtime qualification: %w", err)
	}
	raw := cm.Data[QualificationKey]
	if len(raw) == 0 || len(raw) > 64*1024 || !cm.DeletionTimestamp.IsZero() {
		return time.Time{}, fmt.Errorf("runtime qualification is missing, oversized, or being revoked")
	}
	var qualification AccessQualification
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&qualification); err != nil {
		return time.Time{}, fmt.Errorf("runtime qualification is invalid: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return time.Time{}, fmt.Errorf("runtime qualification has trailing content")
	}
	if qualification.Version != "v1" || !reflect.DeepEqual(qualification.Binding, expected) {
		return time.Time{}, fmt.Errorf("runtime qualification does not match current identities, intent, templates and OIDC")
	}
	if qualification.IssuedAt.IsZero() || qualification.IssuedAt.After(now.Add(time.Minute)) || !qualification.ExpiresAt.After(now) ||
		!qualification.ExpiresAt.After(qualification.IssuedAt) || qualification.ExpiresAt.Sub(qualification.IssuedAt) > time.Hour {
		return time.Time{}, fmt.Errorf("runtime qualification is expired or has an invalid validity period (maximum one hour)")
	}
	if strings.TrimSpace(qualification.Evidence) == "" || len(qualification.Evidence) > 2048 {
		return time.Time{}, fmt.Errorf("runtime qualification requires a bounded evidence reference")
	}
	checks := RequiredQualificationChecks()
	if len(qualification.Checks) != len(checks) {
		return time.Time{}, fmt.Errorf("runtime qualification requires the complete approved test suite")
	}
	for _, check := range checks {
		if !qualification.Checks[check] {
			return time.Time{}, fmt.Errorf("runtime qualification check %s has not passed", check)
		}
	}
	return qualification.ExpiresAt, nil
}
