// Package v1alpha1 holds the platform's own custom resources: what the
// repositories (pkg/database/repository) store, as objects in the
// management cluster instead of rows in a database. One kind per table:
// Tenant, CloudAccount, Network, Cluster, NodePool and ManagedResource,
// plus Claim, which makes a value unique the way a unique index does.
//
// Every object lives in one platform namespace and is named after its row
// ID. Its spec is the row: the scalar columns as typed fields, the
// protojson documents (spec, input, status of the cloud.v1 contract) as
// opaque JSON, so the contract evolves without a CRD change. There is no
// status subresource: a write replaces the whole row at once, as an
// UPDATE does.
//
// +kubebuilder:object:generate=true
// +groupName=platform.cluster-vending.io
package v1alpha1
