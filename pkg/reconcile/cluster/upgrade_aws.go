package cluster

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// followAWSUpgrade raises the stored spec's Kubernetes version to the minor
// EKS reports when that is newer: AWS upgrades a control plane by force at
// the end of its minor's support (see extended.go), and EKS refuses the
// older version the spec would render (a downgrade). The new generation is
// left unresolved, so resolve looks up the add-on versions of the new minor
// in this pass; the node groups and add-ons then follow as in any upgrade.
// EKS behind the spec (an upgrade under way) or on its minor changes
// nothing. A row a call changed meanwhile is left for the next pass.
func (s *Reconciler) followAWSUpgrade(ctx context.Context, row *model.Cluster) error {
	if row.State == StateDeleting || row.State == StateImported || row.State == StateBlocked || row.AppliedGeneration == 0 {
		return nil
	}
	spec, err := SpecOf(row)
	if err != nil {
		return err
	}
	target := spec.GetControlPlane().GetKubernetesVersion()
	if target == "" {
		return nil
	}
	reported, err := s.reportedControlPlane(ctx, row)
	if err != nil || !newer(reported, target) {
		return err
	}
	to := minorString(reported)
	spec.ControlPlane.KubernetesVersion = to
	raw, err := Encode(spec)
	if err != nil {
		return err
	}
	before := *row
	row.Spec, row.State = raw, StateUpdating
	row.Generation++
	if err := s.deps.Store.Clusters().Record(ctx, row); err != nil {
		*row = before
		if errors.Is(err, repository.ErrConflict) || errors.Is(err, repository.ErrNotFound) {
			return nil
		}
		return storeError(err)
	}
	s.logger().WarnContext(ctx, "EKS runs a newer Kubernetes minor than the spec; the spec follows it",
		"cluster_id", row.ID, "from", target, "to", to)
	reconcile.AddNote(ctx, reconcile.Warning(reconcile.ReasonControlPlaneUpgradedByAWS, fmt.Sprintf(
		"AWS upgraded the control plane from %s to %s (its support ended); spec.control_plane.kubernetes_version is %s now, and the node groups and add-ons follow",
		target, to, to)))
	return nil
}

// reportedControlPlane is the version the EKS cluster object reports
// (status.atProvider.version); "" when it reports none yet.
func (s *Reconciler) reportedControlPlane(ctx context.Context, row *model.Cluster) (string, error) {
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, row.ID)
	if err != nil {
		return "", fmt.Errorf("list cluster resources: %w", err)
	}
	for _, r := range records {
		if r.Kind != "Cluster" {
			continue
		}
		obj, err := s.deps.ReadRecorded(ctx, r)
		if err != nil || obj == nil {
			return "", err
		}
		v, _, _ := unstructured.NestedString(obj.Object, "status", "atProvider", "version")
		return v, nil
	}
	return "", nil
}
