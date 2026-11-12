// Package crossplane holds what the Crossplane objects of every cloud
// provider share. Each provider has its own package with the builders of
// its objects: pkg/crossplane/aws (later gcp, azure).
package crossplane

// Labels on everything the server creates.
const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelTenantID  = "cluster-vending.io/tenant-id"
	// LabelNetwork is on every managed resource of a network: its
	// cloud.v1 Network.id. Clusters select the network's subnets by it.
	LabelNetwork = "cluster-vending.io/network"
	// LabelCluster is on every managed resource of a cluster: its
	// cloud.v1 Cluster.id.
	LabelCluster = "cluster-vending.io/cluster"
	// LabelComponent is on every managed resource of a component, next to
	// the cluster's labels: the component's catalog name.
	LabelComponent = "cluster-vending.io/component"
	// LabelSubnetRole (public / private) and LabelZone (zone ID) are on
	// subnets, NAT gateways and their EIPs.
	LabelSubnetRole = "cluster-vending.io/subnet-role"
	LabelZone       = "cluster-vending.io/zone"
	managedBy       = "cluster-vending"
)

// CarriedOverAnnotation marks an imported object that no spec field
// describes: it is not rendered from the spec, kept when its owner is
// rendered again, and deleted with its owner.
const CarriedOverAnnotation = "cluster-vending.io/carried-over"

// Labels returns the labels the server puts on what it creates; tenantID
// is empty for cluster-wide objects shared by every tenant.
func Labels(tenantID string) map[string]string {
	l := map[string]string{LabelManagedBy: managedBy}
	if tenantID != "" {
		l[LabelTenantID] = tenantID
	}
	return l
}

// NetworkLabels returns the labels of a network's managed resources.
func NetworkLabels(tenantID, networkID string) map[string]string {
	l := Labels(tenantID)
	l[LabelNetwork] = networkID
	return l
}

// ClusterLabels returns the labels of a cluster's managed resources.
func ClusterLabels(tenantID, clusterID string) map[string]string {
	l := Labels(tenantID)
	l[LabelCluster] = clusterID
	return l
}

// ComponentLabels returns the labels of a component's managed resources:
// its cluster's, so the cluster's forced deletion finds them too, and the
// component's name.
func ComponentLabels(tenantID, clusterID, component string) map[string]string {
	l := ClusterLabels(tenantID, clusterID)
	l[LabelComponent] = component
	return l
}
