package naas

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
)

const (
	ConditionParentSafety = "SubTenantSafety"
	ReasonDependents      = "SubTenantsRemain"
	ReasonReadFailed      = "SubTenantDependencyReadFailed"
)

// ClusterIdentity names a management Cluster, not a workload-cluster resource.
type ClusterIdentity struct {
	Namespace, ID, Name string
	UID                 types.UID
}

// ClusterDependents must receive the management API's fresh reader. A full
// namespaced list includes unlabeled and terminating objects; missing APIs and
// read errors fail closed. Labels are conservative hints, never a list filter.
func ClusterDependents(ctx context.Context, reader client.Reader, parent ClusterIdentity) ([]string, error) {
	return clusterDependents(ctx, reader, parent, true)
}

// LiveClusterDependents is ClusterDependents without the children already
// being deleted: what a cluster's deletion request is refused for.
func LiveClusterDependents(ctx context.Context, reader client.Reader, parent ClusterIdentity) ([]string, error) {
	return clusterDependents(ctx, reader, parent, false)
}

func clusterDependents(ctx context.Context, reader client.Reader, parent ClusterIdentity, terminating bool) ([]string, error) {
	if reader == nil {
		return nil, fmt.Errorf("fresh SubTenant reader is unavailable")
	}
	children := &cloudv1alpha1.SubTenantList{}
	if err := reader.List(ctx, children, client.InNamespace(parent.Namespace)); err != nil {
		return nil, fmt.Errorf("list SubTenant dependencies: %w", err)
	}
	// Row callers lack the CR UID. Only resolve it when a child actually has
	// a UID reference; empty namespaces incur no additional parent read.
	if parent.UID == "" && slices.ContainsFunc(children.Items, func(child cloudv1alpha1.SubTenant) bool { return child.Spec.ClusterRef.UID != "" }) {
		obj := &cloudv1alpha1.Cluster{}
		if err := reader.Get(ctx, client.ObjectKey{Namespace: parent.Namespace, Name: parent.Name}, obj); err != nil {
			return nil, fmt.Errorf("read parent cluster identity: %w", err)
		}
		parent.UID = obj.UID
	}
	var names []string
	for _, child := range children.Items {
		if !terminating && !child.DeletionTimestamp.IsZero() {
			continue
		}
		ref, labels := child.Spec.ClusterRef, child.Labels
		if parent.ID != "" && (ref.ID == parent.ID || labels[cloudstore.LabelClusterID] == parent.ID || labels[cloudv1alpha1.LabelCluster] == parent.ID) ||
			parent.UID != "" && ref.UID == string(parent.UID) ||
			parent.Name != "" && labels[cloudv1alpha1.LabelCluster] == parent.Name {
			names = append(names, child.Name)
		}
	}
	slices.Sort(names)
	return names, nil
}

func ParentBlockedMessage(action string, names []string) string {
	const maxNames = 10
	displayed := strings.Join(names[:min(len(names), maxNames)], ", ")
	if len(names) > maxNames {
		displayed += fmt.Sprintf(", and %d more", len(names)-maxNames)
	}
	return fmt.Sprintf("%s is blocked: %d SubTenants remain (%s), including any still terminating; delete them and wait for cleanup before retrying", action, len(names), displayed)
}
