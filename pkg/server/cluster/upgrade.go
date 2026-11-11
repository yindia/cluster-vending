package cluster

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
)

// gateUpgrade holds a Kubernetes upgrade's node groups and add-ons back
// until the control plane runs the target version: while the EKS cluster
// reports another version (status.atProvider.version), the node groups
// render at that version and the add-ons keep the versions they run, so
// EKS never sees nodes or add-ons ahead of the cluster. It reports whether
// the plan is gated; the cluster is then applied again (advance) until the
// control plane reports the target.
func (s *ClusterService) gateUpgrade(ctx context.Context, row *model.Cluster, plan *crossplaneaws.ClusterPlan) (bool, error) {
	if plan.KubernetesVersion == "" {
		return false, nil
	}
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, row.ID)
	if err != nil {
		return false, fmt.Errorf("list cluster resources: %w", err)
	}
	live, running, err := s.liveVersions(ctx, records)
	if err != nil || live == "" || live == plan.KubernetesVersion {
		return false, err
	}
	plan.NodeKubernetesVersion = live
	for i, a := range plan.Addons {
		if v, ok := running[a.Name]; ok {
			plan.Addons[i].Version = v
		}
	}
	return true, nil
}

// liveVersions is the Kubernetes version the EKS cluster reports and the
// add-on versions the Addon objects ask for now (spec.forProvider), from
// the cache.
func (s *ClusterService) liveVersions(ctx context.Context, records []model.ManagedResource) (string, map[string]string, error) {
	cluster := ""
	addons := map[string]string{}
	for _, r := range records {
		if r.Kind != "Cluster" && r.Kind != "Addon" {
			continue
		}
		u, err := s.deps.ReadRecorded(ctx, r)
		if err != nil || u == nil {
			if err != nil {
				return "", nil, err
			}
			continue
		}
		if r.Kind == "Cluster" {
			cluster, _, _ = unstructured.NestedString(u.Object, "status", "atProvider", "version")
			continue
		}
		name, _, _ := unstructured.NestedString(u.Object, "spec", "forProvider", "addonName")
		if v, _, _ := unstructured.NestedString(u.Object, "spec", "forProvider", "addonVersion"); name != "" && v != "" {
			addons[name] = v
		}
	}
	return cluster, addons, nil
}
