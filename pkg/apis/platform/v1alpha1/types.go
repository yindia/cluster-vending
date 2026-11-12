package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Row holds what every API resource (network, cluster, node pool) stores:
// lifecycle state, optimistic concurrency and the protojson documents.
type Row struct {
	// +optional
	DisplayName string `json:"displayName,omitempty"`
	// Labels are the resource's API labels (not the object's).
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
	// State is the cloud.v1 *State enum name.
	State string `json:"state"`
	// Generation increments on every spec change of the resource (not the
	// object's metadata.generation, which counts every write).
	Generation int64 `json:"generation"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// AppliedGeneration is the generation whose objects were all applied.
	// +optional
	AppliedGeneration int64 `json:"appliedGeneration,omitempty"`
	// RenderVersion is the renderer version the objects were applied with.
	// +optional
	RenderVersion int64 `json:"renderVersion,omitempty"`
	// ETag changes on every update of the row; updates must present it.
	ETag string `json:"etag"`
	// Spec and Status are the protojson cloud.v1 *Spec / *Status.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +optional
	Spec *apiextensionsv1.JSON `json:"spec,omitempty"`
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +optional
	Status *apiextensionsv1.JSON `json:"status,omitempty"`
	// +optional
	CreatedAt metav1.MicroTime `json:"createdAt,omitempty"`
	// +optional
	UpdatedAt metav1.MicroTime `json:"updatedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced

// Tenant is a customer of the platform; its name is the tenant ID.
type Tenant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              TenantSpec `json:"spec,omitempty"`
}

// TenantSpec is a tenant row.
type TenantSpec struct {
	// +optional
	DisplayName string `json:"displayName,omitempty"`
	// +optional
	CreatedAt metav1.MicroTime `json:"createdAt,omitempty"`
	// +optional
	UpdatedAt metav1.MicroTime `json:"updatedAt,omitempty"`
}

// +kubebuilder:object:root=true

// TenantList is a list of tenants.
type TenantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Tenant `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Tenant",type=string,JSONPath=`.spec.tenantID`
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.provider`
// +kubebuilder:printcolumn:name="Account",type=string,JSONPath=`.spec.accountID`

// CloudAccount is a cloud account a tenant bootstrapped.
type CloudAccount struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              CloudAccountSpec `json:"spec,omitempty"`
}

// CloudAccountSpec is a cloud account row.
type CloudAccountSpec struct {
	TenantID string `json:"tenantID"`
	// +kubebuilder:validation:Enum=aws;gcp;azure
	Provider  string `json:"provider"`
	AccountID string `json:"accountID"`
	// +optional
	ProviderConfigName string `json:"providerConfigName,omitempty"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// +optional
	Identity string `json:"identity,omitempty"`
	// +optional
	DirectoryID string `json:"directoryID,omitempty"`
	// +optional
	AccountName string `json:"accountName,omitempty"`
	// +optional
	CreatedAt metav1.MicroTime `json:"createdAt,omitempty"`
	// +optional
	UpdatedAt metav1.MicroTime `json:"updatedAt,omitempty"`
}

// +kubebuilder:object:root=true

// CloudAccountList is a list of cloud accounts.
type CloudAccountList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CloudAccount `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Tenant",type=string,JSONPath=`.spec.tenantID`
// +kubebuilder:printcolumn:name="Name",type=string,JSONPath=`.spec.name`
// +kubebuilder:printcolumn:name="Region",type=string,JSONPath=`.spec.region`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.spec.state`

// Network is a cloud.v1 Network.
type Network struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              NetworkSpec `json:"spec,omitempty"`
}

// NetworkSpec is a network row.
type NetworkSpec struct {
	Row      `json:",inline"`
	TenantID string `json:"tenantID"`
	// Name is cloud.v1 Network.name: chosen by the client, not unique.
	Name string `json:"name"`
	// Input is the network as the client sent it (protojson).
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +optional
	Input          *apiextensionsv1.JSON `json:"input,omitempty"`
	CloudAccountID string                `json:"cloudAccountID"`
	// +kubebuilder:validation:Enum=aws;gcp;azure
	Provider string `json:"provider"`
	Region   string `json:"region"`
	// ExternalID is the cloud network an imported network stands for.
	// +optional
	ExternalID *string `json:"externalID,omitempty"`
	// +optional
	CreateRequestID *string `json:"createRequestID,omitempty"`
}

// +kubebuilder:object:root=true

// NetworkList is a list of networks.
type NetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Network `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Tenant",type=string,JSONPath=`.spec.tenantID`
// +kubebuilder:printcolumn:name="Name",type=string,JSONPath=`.spec.name`
// +kubebuilder:printcolumn:name="Region",type=string,JSONPath=`.spec.region`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.spec.state`

// Cluster is a cloud.v1 Cluster.
type Cluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ClusterSpec `json:"spec,omitempty"`
}

// ClusterSpec is a cluster row.
type ClusterSpec struct {
	Row      `json:",inline"`
	TenantID string `json:"tenantID"`
	// Name is cloud.v1 Cluster.name: unique per account and region.
	Name string `json:"name"`
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +optional
	Input     *apiextensionsv1.JSON `json:"input,omitempty"`
	NetworkID string                `json:"networkID"`
	// +optional
	CloudAccountID string `json:"cloudAccountID,omitempty"`
	// +kubebuilder:validation:Enum=aws;gcp;azure
	Provider string `json:"provider"`
	Region   string `json:"region"`
	// +optional
	Imported bool `json:"imported,omitempty"`
	// Adoption is what an imported cluster keeps of what it was imported
	// from (JSON crossplaneaws.Adoption).
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +optional
	Adoption *apiextensionsv1.JSON `json:"adoption,omitempty"`
	// +optional
	CreateRequestID *string `json:"createRequestID,omitempty"`
	// +optional
	DeleteForce bool `json:"deleteForce,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterList is a list of clusters.
type ClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Cluster `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterID`
// +kubebuilder:printcolumn:name="Name",type=string,JSONPath=`.spec.name`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.spec.state`

// NodePool is a cloud.v1 NodePool.
type NodePool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              NodePoolSpec `json:"spec,omitempty"`
}

// NodePoolSpec is a node pool row.
type NodePoolSpec struct {
	Row       `json:",inline"`
	ClusterID string `json:"clusterID"`
	// Name is cloud.v1 NodePool.id, unique per cluster.
	Name string `json:"name"`
	// +optional
	CarriedOver bool `json:"carriedOver,omitempty"`
	// +optional
	CreateRequestID *string `json:"createRequestID,omitempty"`
}

// +kubebuilder:object:root=true

// NodePoolList is a list of node pools.
type NodePoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodePool `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.spec.ownerKind`
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.kind`
// +kubebuilder:printcolumn:name="Object",type=string,JSONPath=`.spec.name`
// +kubebuilder:printcolumn:name="External",type=string,JSONPath=`.spec.externalName`

// ManagedResource records one object the platform created in the
// management cluster for an account, network, cluster or node pool.
type ManagedResource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ManagedResourceSpec `json:"spec,omitempty"`
}

// ManagedResourceSpec is a managed resource row.
type ManagedResourceSpec struct {
	TenantID       string `json:"tenantID"`
	CloudAccountID string `json:"cloudAccountID"`
	// +kubebuilder:validation:Enum=cloud_account;network;cluster;node_pool
	OwnerKind string `json:"ownerKind"`
	OwnerID   string `json:"ownerID"`
	// APIVersion, Kind, Namespace and Name identify the recorded object.
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	// +optional
	ExternalName string `json:"externalName,omitempty"`
	// +optional
	CarriedOver bool `json:"carriedOver,omitempty"`
	// +optional
	GoneAt *metav1.MicroTime `json:"goneAt,omitempty"`
	// Order is the position of the record among its owner's: records list
	// in the order they were made.
	Order int64 `json:"order"`
	// +optional
	CreatedAt metav1.MicroTime `json:"createdAt,omitempty"`
	// +optional
	UpdatedAt metav1.MicroTime `json:"updatedAt,omitempty"`
}

// +kubebuilder:object:root=true

// ManagedResourceList is a list of managed resource records.
type ManagedResourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ManagedResource `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Key",type=string,JSONPath=`.spec.key`
// +kubebuilder:printcolumn:name="Holder",type=string,JSONPath=`.spec.holder`

// Claim makes a value unique, the way a unique index does: its name is
// derived from the value (Key), and creating an object is atomic, so of
// two rows claiming one value exactly one gets it. It is deleted with the
// row that holds it.
type Claim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ClaimSpec `json:"spec,omitempty"`
}

// ClaimSpec is what a claim reserves and for whom.
type ClaimSpec struct {
	// Key is the claimed value, readable: "cluster-name/<account>/<region>/<name>".
	Key string `json:"key"`
	// HolderKind and Holder name the row (kind and object name) holding it.
	HolderKind string `json:"holderKind"`
	Holder     string `json:"holder"`
	// +optional
	CreatedAt metav1.MicroTime `json:"createdAt,omitempty"`
}

// +kubebuilder:object:root=true

// ClaimList is a list of claims.
type ClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Claim `json:"items"`
}
