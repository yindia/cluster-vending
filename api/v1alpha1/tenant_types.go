package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TenantFinalizer retains the namespace and refuses deletion while it is in use.
const TenantFinalizer = "cloud.cluster-vending.io/tenant"

// TenantSpec registers a tenant independently of its cloud accounts.
type TenantSpec struct {
	// displayName is an optional human-readable tenant name.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	DisplayName string `json:"displayName,omitempty"`
}

// TenantFreezeStatus observes the canonical ReleaseHold; it is not desired state.
type TenantFreezeStatus struct {
	Frozen bool `json:"frozen"`
	// +optional
	Scope string `json:"scope,omitempty"`
	// +optional
	Reason string `json:"reason,omitempty"`
	// +optional
	Until *metav1.Time `json:"until,omitempty"`
	// +optional
	Revision string `json:"revision,omitempty"`
}

// TenantStatus reports namespace readiness and the effective platform freeze.
type TenantStatus struct {
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	Freeze *TenantFreezeStatus `json:"freeze,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=cvtenant,categories=cluster-vending
// +kubebuilder:printcolumn:name="Display name",type=string,JSONPath=`.spec.displayName`
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.status.namespace`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Frozen",type=boolean,JSONPath=`.status.freeze.frozen`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9]([a-z0-9-]{0,54}[a-z0-9])?$')",message="tenant name must be a DNS label of at most 56 characters"

// Tenant is the cluster-scoped canonical tenant registration. Its namespace is
// retained on deletion and deliberately has no garbage-collection owner reference.
type Tenant struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`
	// +optional
	Spec TenantSpec `json:"spec,omitzero"`
	// +optional
	Status TenantStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// TenantList contains Tenant registrations.
type TenantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Tenant `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Tenant{}, &TenantList{})
		return nil
	})
}
