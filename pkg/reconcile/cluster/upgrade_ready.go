package cluster

import (
	"github.com/yindia/cluster-vending/pkg/clustersummary"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// UpgradeHold is why an automatic Kubernetes upgrade must not start on the
// cluster as stored now; "" when it may. The order is the install order:
// the EKS add-ons (with every other resource: the cluster is RUNNING), then
// the platform components, then the tenant components, each all Ready.
// A component being removed does not hold an upgrade back. components are
// the cluster's tenant components.
func UpgradeHold(row *model.Cluster, components []model.Component) string {
	if row.State != StateRunning {
		return "the cluster is not RUNNING (its EKS add-ons and resources are not all Ready)"
	}
	if why := clustersummary.Wait(StoredSummary(row), now()); why != "" {
		return why
	}
	if name := platformNotReady(row); name != "" {
		return "platform component " + name + " is not Ready"
	}
	for _, c := range components {
		if c.State == componentDeleting {
			continue
		}
		if tenantStatus(c).GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_READY {
			return "tenant component " + c.Component + " is not Ready"
		}
	}
	return ""
}

// platformNotReady is the first platform component the row's recorded
// status lists that is neither Ready nor being removed; "" when none.
func platformNotReady(row *model.Cluster) string {
	for _, c := range storedStatus(row).GetPlatformComponents() {
		switch c.GetPhase() {
		case cloudv1.ComponentPhase_COMPONENT_PHASE_READY, cloudv1.ComponentPhase_COMPONENT_PHASE_DELETING:
		default:
			return c.GetName()
		}
	}
	return ""
}

// StoredSummary is the cluster status summary the row carries (PROP-02 5.2);
// nil when no pass has recorded one, or it cannot be read.
func StoredSummary(row *model.Cluster) *cloudv1.ClusterStatus {
	if len(row.Summary) == 0 {
		return nil
	}
	sum := &cloudv1.ClusterStatus{}
	if err := Lenient.Unmarshal(row.Summary, sum); err != nil {
		return nil
	}
	return sum
}
