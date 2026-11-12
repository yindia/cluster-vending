package model

// Component is a cloud.v1 Component: one catalog component enabled on one
// cluster. A cluster has a component at most once, and cannot be deleted
// while it has any; the store enforces both.
type Component struct {
	Base
	TenantID string
	// ClusterID is the clusters row the component is installed into.
	ClusterID string
	// Component is the ComponentDefinition name, e.g. "karpenter".
	Component string
	// Version pins a version; empty: the release's.
	Version string
	// Values are the tenant's overrides: a JSON object, nil for none.
	Values JSON
	// State is the cloud.v1 ComponentPhase name the API set, e.g.
	// "COMPONENT_PHASE_DELETING" after DisableComponent; the controller's
	// view is in Status.
	State string
	// Generation increments on every change of version or values;
	// ObservedGeneration is the generation the stored status reflects.
	Generation         int64
	ObservedGeneration int64
	// ETag changes on every write; updates must present the current one.
	ETag string
	// Status is the protojson cloud.v1 ComponentStatus.
	Status          JSON
	CreateRequestID *string
	// DeleteForce: DisableComponent was called with force.
	DeleteForce bool
}
