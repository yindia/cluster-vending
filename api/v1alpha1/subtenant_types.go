package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// SubTenantFinalizer protects remote tenant data during require-empty deletion.
const SubTenantFinalizer = "cloud.cluster-vending.io/subtenant"

// SubTenantTenantReference binds an existing management Tenant registration.
type SubTenantTenantReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=56
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]{0,54}[a-z0-9])?$`
	Name string `json:"name"`
	// uid prevents adopting a replacement registration with the same name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	UID string `json:"uid"`
}

// SubTenantClusterReference binds a canonical cluster UUID and object incarnation.
type SubTenantClusterReference struct {
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`
	ID string `json:"id"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	UID string `json:"uid"`
}

// SubTenantNamespaceKey is a logical key, not a caller-selected remote namespace.
// Generated namespaces use the UUID-derived tenant prefix followed by this key.
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=27
// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]{0,25}[a-z0-9])?$`
type SubTenantNamespaceKey string

// SubTenantQuota is a SubTenant-wide budget. The API must parse each hard value as
// a nonnegative Kubernetes Quantity and require integer values for object counts.
type SubTenantQuota struct {
	// namespaceCount bounds namespace allocation, including future append requests.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	NamespaceCount int32 `json:"namespaceCount"`
	// hard permits only the approved quota dimensions; values are Quantity strings.
	// +kubebuilder:validation:MinProperties=1
	// +kubebuilder:validation:MaxProperties=12
	// +kubebuilder:validation:XValidation:rule="self.all(k, k in ['requests.cpu', 'requests.memory', 'limits.cpu', 'limits.memory', 'requests.ephemeral-storage', 'limits.ephemeral-storage', 'requests.storage', 'persistentvolumeclaims', 'pods', 'services', 'secrets', 'configmaps'])",message="unsupported quota resource"
	// +kubebuilder:validation:XValidation:rule="self.all(k, size(self[k]) > 0 && size(self[k]) <= 64)",message="quota quantities must contain 1 to 64 characters"
	// +kubebuilder:validation:XValidation:rule="self.all(k, isQuantity(self[k]) && quantity(self[k]).compareTo(quantity('0')) >= 0)",message="quota values must be valid nonnegative Kubernetes quantities"
	// +kubebuilder:validation:XValidation:rule="self.all(k, !(k in ['persistentvolumeclaims', 'pods', 'services', 'secrets', 'configmaps']) || (isQuantity(self[k]) && quantity(self[k]).isInteger()))",message="object-count quotas must be integral quantities"
	Hard map[string]string `json:"hard"`
}

// SubTenantAccessBinding refers to an identity approved for this tenant. It is
// never a raw Kubernetes subject, OIDC group, role name or bearer credential.
type SubTenantAccessBinding struct {
	// identityRef must be resolved and authorized by a trusted platform resolver.
	// A missing resolver or unknown reference must fail closed.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`
	IdentityRef string `json:"identityRef"`
	// +kubebuilder:validation:Enum=workload-editor
	RoleProfile string `json:"roleProfile"`
}

// SubTenantAccess requests approved identity bindings; omission grants no access.
type SubTenantAccess struct {
	// +optional
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=identityRef
	Bindings []SubTenantAccessBinding `json:"bindings,omitempty"`
}

// SubTenantSpec is the desired state stored only in the owning tenant namespace.
// Canonical references are resolved by the service, not trusted from RPC input.
// +kubebuilder:validation:XValidation:rule="self.tenantRef == oldSelf.tenantRef",message="tenantRef is immutable"
// +kubebuilder:validation:XValidation:rule="self.clusterRef == oldSelf.clusterRef",message="clusterRef is immutable"
// +kubebuilder:validation:XValidation:rule="oldSelf.namespaceKeys.all(k, k in self.namespaceKeys)",message="namespaceKeys are append-only; namespace removal requires a separate safe lifecycle"
// +kubebuilder:validation:XValidation:rule="self.quota.namespaceCount >= size(self.namespaceKeys)",message="namespaceCount must cover every requested namespace key"
type SubTenantSpec struct {
	TenantRef  SubTenantTenantReference  `json:"tenantRef"`
	ClusterRef SubTenantClusterReference `json:"clusterRef"`
	// +optional
	// +kubebuilder:validation:MaxLength=128
	DisplayName string `json:"displayName,omitempty"`
	// namespaceKeys are append-only. Remote names are assigned by the platform.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +listType=set
	NamespaceKeys []SubTenantNamespaceKey `json:"namespaceKeys"`
	Quota         SubTenantQuota          `json:"quota"`
	// +kubebuilder:validation:Enum=restricted-ambient-v1
	PolicyProfile string `json:"policyProfile"`
	// +optional
	Access *SubTenantAccess `json:"access,omitempty"`
}

// SubTenantState is an asynchronously observed lifecycle state.
// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;Degraded;Failed;Deleting
type SubTenantState string

const (
	SubTenantStatePending      SubTenantState = "Pending"
	SubTenantStateProvisioning SubTenantState = "Provisioning"
	SubTenantStateReady        SubTenantState = "Ready"
	SubTenantStateDegraded     SubTenantState = "Degraded"
	SubTenantStateFailed       SubTenantState = "Failed"
	SubTenantStateDeleting     SubTenantState = "Deleting"
)

// SubTenantNamespaceStatus is an observation, never permission to create directly.
// Readiness requires current baseline qualification; GTR Ready alone is insufficient.
type SubTenantNamespaceStatus struct {
	Key SubTenantNamespaceKey `json:"key"`
	// +optional
	Name string `json:"name,omitempty"`
	// +optional
	UID string `json:"uid,omitempty"`
	// +optional
	TemplateRevision string `json:"templateRevision,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// SubTenantStatus is written by the observer/controller only. The per-subtenant
// remote writer creates only the Capsule Tenant; shared GTRs own its children.
type SubTenantStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	State SubTenantState `json:"state,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	RemoteCapsuleTenant string `json:"remoteCapsuleTenant,omitempty"`
	// +optional
	RemoteCapsuleTenantUID string `json:"remoteCapsuleTenantUID,omitempty"`
	// +optional
	TemplateRevision string `json:"templateRevision,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=key
	Namespaces []SubTenantNamespaceStatus `json:"namespaces,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxItems=64
	DeletionBlockers []string `json:"deletionBlockers,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cvst,categories=cluster-vending
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterRef.name`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SubTenant records one namespace-as-a-service allocation in tenant-<tenantID>.
// Kubernetes is its source of truth; no database or arbitrary policy payload exists.
type SubTenant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`
	Spec              SubTenantSpec `json:"spec"`
	// +optional
	Status SubTenantStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SubTenantList contains SubTenant records.
type SubTenantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SubTenant `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &SubTenant{}, &SubTenantList{})
		return nil
	})
}
