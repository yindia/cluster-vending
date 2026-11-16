package cluster

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// observeArchitecture reads an allowlist from already-scoped inventory. It does
// not discover cloud resources, read Secrets, install components, or write to
// either cluster. Missing objects/kinds are absent facts; other read errors fail
// the live view instead of presenting an incomplete topology as current.
func (s *Reconciler) observeArchitecture(ctx context.Context, row *model.Cluster, records []model.ManagedResource) (*cloudv1.ClusterArchitecture, error) {
	out := &cloudv1.ClusterArchitecture{}
	if row.Provider != model.ProviderAWS {
		return out, nil
	}
	// There is no actual control-plane topology to project before its inventory
	// exists. Do not manufacture a root from the desired Cluster spec.
	hasCluster := false
	for _, r := range records {
		if r.OwnerKind == model.OwnerCluster && clusterArchitectureRecord(row, r) && r.Kind == "Cluster" && r.APIVersion == "eks.aws.m.upbound.io/v1beta1" {
			hasCluster = true
		}
	}
	if !hasCluster {
		return out, nil
	}
	all := append([]model.ManagedResource(nil), records...)
	for _, owner := range []struct {
		kind model.OwnerKind
		id   string
	}{{model.OwnerNetwork, row.NetworkID}, {model.OwnerClusterComponents, row.ID}} {
		if owner.id == "" {
			continue
		}
		more, err := s.deps.Store.ManagedResources().ListByOwner(ctx, owner.kind, owner.id)
		if err != nil {
			return nil, err
		}
		all = append(all, more...)
	}
	components, err := s.deps.Store.Components().ListByCluster(ctx, row.TenantID, row.ID)
	if err != nil {
		return nil, err
	}
	componentIDs := map[string]bool{}
	for _, component := range components {
		if component.TenantID != row.TenantID || component.ClusterID != row.ID {
			continue
		}
		componentIDs[component.ID] = true
		more, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerComponent, component.ID)
		if err != nil {
			return nil, err
		}
		all = append(all, more...)
	}
	var objects []*unstructured.Unstructured
	seen := map[string]bool{}
	for _, r := range all {
		if r.OwnerKind == model.OwnerComponent {
			if !componentIDs[r.OwnerID] || r.TenantID != row.TenantID || r.Namespace != kube.TenantNamespace(row.TenantID) {
				continue
			}
		} else if !clusterArchitectureRecord(row, r) {
			continue
		}
		gvk := schema.FromAPIVersionAndKind(r.APIVersion, r.Kind)
		if !clusterArchitectureKind(gvk) {
			continue
		}
		if r.OwnerKind == model.OwnerNetwork && r.Kind != "Subnet" {
			continue
		}
		key := r.APIVersion + "/" + r.Kind + "/" + r.Namespace + "/" + r.Name
		if seen[key] {
			continue
		}
		seen[key] = true
		obj, err := s.deps.ReadRecorded(ctx, r)
		if err != nil {
			return nil, fmt.Errorf("read cluster architecture %s %s: %w", r.Kind, r.Name, err)
		}
		if obj != nil {
			objects = append(objects, obj)
		}
	}
	return clusterArchitecture(row, objects), nil
}

func clusterArchitectureRecord(row *model.Cluster, r model.ManagedResource) bool {
	if r.TenantID != row.TenantID || r.Namespace != kube.TenantNamespace(row.TenantID) {
		return false
	}
	switch r.OwnerKind {
	case model.OwnerCluster, model.OwnerClusterComponents:
		return r.OwnerID == row.ID
	case model.OwnerNetwork:
		return row.NetworkID != "" && r.OwnerID == row.NetworkID
	default:
		return false
	}
}

func clusterArchitectureKind(gvk schema.GroupVersionKind) bool {
	switch gvk.GroupVersion().String() {
	case "eks.aws.m.upbound.io/v1beta1":
		switch gvk.Kind {
		case "Cluster", "NodeGroup", "Addon", "ClusterAuth":
			return true
		}
	case "ec2.aws.m.upbound.io/v1beta1":
		switch gvk.Kind {
		case "Subnet", "SecurityGroup", "LaunchTemplate":
			return true
		}
	case "helm.m.crossplane.io/v1beta1":
		return gvk.Kind == "Release" || gvk.Kind == "ProviderConfig"
	}
	return false
}

func clusterArchitectureScope(row *model.Cluster, obj *unstructured.Unstructured) bool {
	if !clusterArchitectureKind(obj.GroupVersionKind()) || obj.GetNamespace() != kube.TenantNamespace(row.TenantID) || obj.GetName() == "" {
		return false
	}
	labels := obj.GetLabels()
	if tenant := labels[crossplane.LabelTenantID]; tenant != "" && tenant != row.TenantID {
		return false
	}
	if obj.GetKind() == "Subnet" {
		return row.NetworkID != "" && labels[crossplane.LabelNetwork] == row.NetworkID
	}
	return labels[crossplane.LabelCluster] == row.ID
}
