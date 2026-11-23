package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// AWSAccount identifies an AWS account and how the platform reaches it.
type AWSAccount struct {
	// accountID is the 12-digit AWS account ID. Immutable.
	// +required
	// +kubebuilder:validation:Pattern=`^[0-9]{12}$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="accountID is immutable"
	AccountID string `json:"accountID"`
	// role is the name (no path) of the IAM role the platform assumes in
	// the account, e.g. "cluster-vending-provisioner". The tenant creates
	// it, trusting the platform's management role. Empty until the account
	// is bootstrapped.
	// +optional
	// +kubebuilder:validation:Pattern=`^[\w+=,.@-]{1,64}$`
	Role string `json:"role,omitempty"`
	// readerRole is the ARN of the account's read-only role (the reader
	// stack's), which the live reader's identity assumes (PROP-02 7.1).
	// Empty: arn:aws:iam::<accountID>:role/cluster-vending-reader, the
	// stack's default RoleName. An account without the role gets no live
	// reads; nothing else depends on it.
	// +optional
	// +kubebuilder:validation:Pattern=`^arn:aws[a-z-]*:iam::[0-9]{12}:role/[\w+=,.@/-]{1,512}$`
	ReaderRole string `json:"readerRole,omitempty"`
}

// CloudAccountSpec is a cloud account of the tenant, as the platform
// should reach it. Exactly one provider is set.
// +kubebuilder:validation:XValidation:rule="has(self.aws)",message="exactly one provider must be set: aws"
type CloudAccountSpec struct {
	// accountName is the account's human-readable name (AWS account name).
	// Informational only; not unique.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	AccountName string `json:"accountName,omitempty"`
	// aws is set for an AWS account.
	// +optional
	AWS *AWSAccount `json:"aws,omitempty"`
}

// CloudAccountPhase is the coarse state of an account.
// +kubebuilder:validation:Enum=Bootstrapping;Ready;Deleting;Failed
type CloudAccountPhase string

// Account phases.
const (
	// CloudAccountPhaseBootstrapping: the account's provider credential is
	// being set up.
	CloudAccountPhaseBootstrapping CloudAccountPhase = "Bootstrapping"
	// CloudAccountPhaseReady: networks and clusters can be created in it.
	CloudAccountPhaseReady CloudAccountPhase = "Ready"
	// CloudAccountPhaseDeleting: deletion was requested; it waits until the
	// account has no networks left.
	CloudAccountPhaseDeleting CloudAccountPhase = "Deleting"
	// CloudAccountPhaseFailed: see the conditions.
	CloudAccountPhaseFailed CloudAccountPhase = "Failed"
)

// CloudAccountStatus is the observed state of a CloudAccount.
type CloudAccountStatus struct {
	// observedGeneration is the generation the status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// phase is the coarse state.
	// +optional
	Phase CloudAccountPhase `json:"phase,omitempty"`
	// conditions: Ready and Synced.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// providerConfigName is the name of the account's provider credential
	// (AWS: the ProviderConfig) in this namespace: generated on the first
	// bootstrap, then fixed. The Crossplane objects of every network and
	// cluster of the account reference it.
	// +optional
	ProviderConfigName string `json:"providerConfigName,omitempty"`
	// identity is what the provider credential assumes in the account (AWS:
	// the role ARN).
	// +optional
	Identity string `json:"identity,omitempty"`
	// networks counts the Networks the account owns. The account cannot be
	// deleted while it is not zero.
	// +optional
	Networks int32 `json:"networks,omitempty"`
	// inventory lists the objects created for the account itself (its
	// ProviderConfig), in creation order.
	// +listType=atomic
	// +optional
	Inventory []InventoryEntry `json:"inventory,omitempty"`
	// access is the controller's last check that it can reach and act in
	// the account: the tenant role in every region the account's networks
	// use, and the calls the platform needs (docs/architecture.md#aws-access-and-accounts).
	// +optional
	Access *AccountAccess `json:"access,omitempty"`
}

// AccountAccess is what an access check of the account found.
type AccountAccess struct {
	// verified: every region and check passed when last checked.
	Verified bool `json:"verified"`
	// checkedAt is the last check that gave a verdict.
	// +optional
	CheckedAt *metav1.Time `json:"checkedAt,omitempty"`
	// attemptedAt is the last check tried; later than checkedAt when it
	// failed for a transient reason.
	// +optional
	AttemptedAt *metav1.Time `json:"attemptedAt,omitempty"`
	// stale: the last attempt failed transiently (throttling, the
	// network); verified and the results are from checkedAt.
	// +optional
	Stale bool `json:"stale,omitempty"`
	// principalARN is who the calls were made as.
	// +optional
	PrincipalARN string `json:"principalARN,omitempty"`
	// reason and message say what failed first.
	// +optional
	Reason string `json:"reason,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// regions: whether the tenant role could be used in each region.
	// +optional
	// +kubebuilder:validation:MaxItems=40
	Regions []AccessResult `json:"regions,omitempty"`
	// checks: each call of the check, per region.
	// +optional
	// +kubebuilder:validation:MaxItems=200
	Checks []AccessResult `json:"checks,omitempty"`
}

// AccessResult is one region's or call's outcome; action is empty for a
// region.
type AccessResult struct {
	// +optional
	Action string `json:"action,omitempty"`
	Region string `json:"region"`
	OK     bool   `json:"ok"`
	// +optional
	Reason string `json:"reason,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=cvacct;cvaccount,categories=cluster-vending
// +kubebuilder:printcolumn:name="Account",type=string,JSONPath=`.spec.aws.accountID`
// +kubebuilder:printcolumn:name="Networks",type=integer,JSONPath=`.status.networks`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// CloudAccount is a cloud account of a tenant (the tenant is the
// namespace). It owns the Networks created in it, and through them their
// Clusters: one account has many networks and clusters. It is not deleted
// while it owns a Network (CloudAccountFinalizer).
type CloudAccount struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of CloudAccount
	// +required
	Spec CloudAccountSpec `json:"spec"`

	// status defines the observed state of CloudAccount
	// +optional
	Status CloudAccountStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// CloudAccountList contains a list of CloudAccount
type CloudAccountList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []CloudAccount `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &CloudAccount{}, &CloudAccountList{})
		return nil
	})
}
