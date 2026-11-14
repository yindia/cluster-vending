package controller

// The controller's access to the component kinds (docs/design/component-usage.md).
// The component reconciler is not written yet; these markers give the
// controller's role what it will need: the catalog read, and created where a
// shipped definition is missing (pkg/catalog; release-admin
// writes it) with its status, and the tenants' Components with their status
// and finalizer.

// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=componentdefinitions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=componentdefinitions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=componentdefinitions/finalizers,verbs=update
// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=components,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=components/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=components/finalizers,verbs=update
