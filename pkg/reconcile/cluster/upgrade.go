package cluster

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// upgrade is what the management cluster shows of a Kubernetes upgrade.
// Its phase is the one rule of an upgrade's order: GateUpgrade applies by
// it and the cluster's status (ClusterStatus.upgrade) reports it.
type upgrade struct {
	// target is spec.control_plane.kubernetes_version; controlPlane what
	// the EKS cluster reports (status.atProvider.version).
	target, controlPlane string
	// nodes are the versions the node groups report.
	nodes []string
	// addons are the versions the Addon objects ask for now, by add-on.
	addons map[string]string
}

// phase is the step the upgrade is at. EKS refuses node groups and
// add-ons ahead of the control plane, so the control plane goes first:
//
//	CONTROL_PLANE     the control plane reports another version than the
//	                  target
//	NODES_AND_ADDONS  the control plane runs the target and a node group
//	                  does not yet; or, following an upgrade to this
//	                  target (following), they have not settled
//	UNSPECIFIED       no upgrade: no target, no EKS cluster yet, or done
func (u upgrade) phase(following bool) cloudv1.ClusterUpgradePhase {
	switch {
	case u.target == "" || u.controlPlane == "":
		return cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_UNSPECIFIED
	case u.controlPlane != u.target:
		return cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_CONTROL_PLANE
	case following || u.nodesBehind() != "":
		return cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_NODES_AND_ADDONS
	default:
		return cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_UNSPECIFIED
	}
}

// nodesBehind is the version of a node group that does not run the target
// yet; empty when none is behind.
func (u upgrade) nodesBehind() string {
	for _, v := range u.nodes {
		if v != u.target {
			return v
		}
	}
	return ""
}

// GateUpgrade holds a Kubernetes upgrade's node groups and add-ons back
// while the upgrade is in its CONTROL_PLANE phase (upgrade.phase): the
// node groups render at the version the control plane reports and the
// add-ons keep the versions they run, so EKS never sees nodes or add-ons
// ahead of the cluster. It reports whether the plan is gated; the cluster
// is then applied again (advance) until the control plane reports the
// target.
func (s *Reconciler) GateUpgrade(ctx context.Context, row *model.Cluster, plan *crossplaneaws.ClusterPlan) (bool, error) {
	if plan.KubernetesVersion == "" {
		return false, nil
	}
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, row.ID)
	if err != nil {
		return false, fmt.Errorf("list cluster resources: %w", err)
	}
	u, err := s.observeUpgrade(ctx, records, plan.KubernetesVersion)
	if err != nil || u.phase(false) != cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_CONTROL_PLANE {
		return false, err
	}
	plan.NodeKubernetesVersion = u.controlPlane
	for i, a := range plan.Addons {
		if v, ok := u.addons[a.Name]; ok {
			plan.Addons[i].Version = v
		}
	}
	return true, nil
}

// observeUpgrade reads what an upgrade to target looks like now, from the
// cache: the version the EKS cluster reports, the versions the node
// groups report, and the versions the Addon objects ask for
// (spec.forProvider).
func (s *Reconciler) observeUpgrade(ctx context.Context, records []model.ManagedResource, target string) (upgrade, error) {
	u := upgrade{target: target, addons: map[string]string{}}
	for _, r := range records {
		if r.Kind != "Cluster" && r.Kind != "Addon" && r.Kind != KindNodeGroup {
			continue
		}
		obj, err := s.deps.ReadRecorded(ctx, r)
		if err != nil {
			return upgrade{}, err
		}
		if obj == nil {
			continue
		}
		switch r.Kind {
		case "Cluster":
			u.controlPlane, _, _ = unstructured.NestedString(obj.Object, "status", "atProvider", "version")
		case KindNodeGroup:
			if v, _, _ := unstructured.NestedString(obj.Object, "status", "atProvider", "version"); v != "" {
				u.nodes = append(u.nodes, v)
			}
		default:
			name, _, _ := unstructured.NestedString(obj.Object, "spec", "forProvider", "addonName")
			if v, _, _ := unstructured.NestedString(obj.Object, "spec", "forProvider", "addonVersion"); name != "" && v != "" {
				u.addons[name] = v
			}
		}
	}
	return u, nil
}

// followers are the kinds that follow the control plane in an upgrade.
var followers = map[string]bool{KindNodeGroup: true, "Addon": true}

// status is ClusterStatus.upgrade: the step the upgrade is at, what it
// moves from and to, since when (kept from the status recorded before,
// stored; else the row's last change, the update that asked for it), and
// what the step waits for or, when the cloud fails it, the provider's
// error. Nil when no upgrade is in progress, and for clusters that are
// not applied (deleting, imported read-only).
func (u upgrade) status(row *model.Cluster, views []*cloudv1.ManagedResource, stored *cloudv1.ClusterUpgrade) *cloudv1.ClusterUpgrade {
	if row.State == StateDeleting || row.State == StateImported || row.State == StateBlocked {
		return nil
	}
	same := stored.GetToVersion() == u.target
	out := &cloudv1.ClusterUpgrade{ToVersion: u.target, StartTime: timestamppb.New(row.UpdatedAt)}
	if same && stored.GetStartTime() != nil {
		out.StartTime = stored.GetStartTime()
	}
	switch out.Phase = u.phase(same && !followersSettled(row, views)); out.GetPhase() {
	case cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_CONTROL_PLANE:
		out.FromVersion = u.controlPlane
		out.Message = fmt.Sprintf("upgrading the control plane from %s to %s; the node group and the add-ons follow",
			u.controlPlane, u.target)
		if failure := failureOf(views, func(kind string) bool { return kind == "Cluster" }); failure != "" {
			out.Message = failure
		}
	case cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_NODES_AND_ADDONS:
		out.FromVersion = u.nodesBehind()
		if same && stored.GetFromVersion() != "" {
			out.FromVersion = stored.GetFromVersion()
		}
		out.Message = fmt.Sprintf("the control plane runs %s; upgrading the node group and the add-ons", u.target)
		if failure := failureOf(views, func(kind string) bool { return followers[kind] }); failure != "" {
			out.Message = failure
		}
	default:
		return nil
	}
	return out
}

// followersSettled reports whether the node groups and add-ons are done
// following an upgrade: the generation was applied in full (so at the
// target version) and each of them is READY with its provider caught up.
func followersSettled(row *model.Cluster, views []*cloudv1.ManagedResource) bool {
	if row.AppliedGeneration < row.Generation {
		return false
	}
	for _, v := range views {
		if !followers[v.GetKind()] || v.GetState() == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING {
			continue
		}
		if v.GetState() != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY ||
			reconcile.Behind(v) {
			return false
		}
	}
	return true
}

// failureOf is the provider's error of the first resource of the kinds
// selected that the cloud refuses or fails (Synced False), as
// "<Kind> <name>: <error>"; empty when none does.
func failureOf(views []*cloudv1.ManagedResource, selected func(kind string) bool) string {
	for _, v := range views {
		if selected(v.GetKind()) && v.GetSynced() == cloudv1.ConditionStatus_CONDITION_STATUS_FALSE && v.GetMessage() != "" {
			return fmt.Sprintf("%s %s: %s", v.GetKind(), v.GetName(), v.GetMessage())
		}
	}
	return ""
}

// storedUpgrade is the upgrade of the status recorded with the row, or nil.
func storedUpgrade(row *model.Cluster) *cloudv1.ClusterUpgrade {
	if len(row.Status) == 0 {
		return nil
	}
	st := &cloudv1.ClusterStatus{}
	if err := Lenient.Unmarshal(row.Status, st); err != nil {
		return nil
	}
	return st.GetUpgrade()
}

// CheckVersionStep refuses a Kubernetes version EKS cannot move to: a
// downgrade, or more than one minor version ahead of what the control
// plane runs (the stored version before EKS created it). Refused here, the
// stored spec never holds a version the cluster can only fail on.
func (s *Reconciler) CheckVersionStep(ctx context.Context, row *model.Cluster, stored, target string) error {
	from, err := s.LiveKubernetesVersion(ctx, row)
	if err != nil {
		return err
	}
	if from == "" {
		from = stored
	}
	fromMinor, ok1 := minorOf(from)
	toMinor, ok2 := minorOf(target)
	if !ok1 || !ok2 {
		return nil // unknown or unset: EKS decides
	}
	switch {
	case toMinor < fromMinor:
		return fault.New(fault.Invalid,
			fmt.Sprintf("kubernetes_version %s is older than the cluster's %s; EKS cannot downgrade", target, from))
	case toMinor > fromMinor+1:
		return fault.New(fault.Invalid,
			fmt.Sprintf("kubernetes_version %s skips a minor version: the cluster runs %s; upgrade one minor version at a time", target, from))
	}
	return nil
}

// minorOf is the minor of a Kubernetes version "1.<minor>".
func minorOf(version string) (int, bool) {
	major, minor, ok := strings.Cut(version, ".")
	if !ok || major != "1" {
		return 0, false
	}
	minor, _, _ = strings.Cut(minor, ".")
	n, err := strconv.Atoi(minor)
	return n, err == nil
}
