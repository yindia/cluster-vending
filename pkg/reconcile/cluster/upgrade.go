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
// Its step (status, nextStep) is the one rule of an upgrade's order:
// GateUpgrade applies by it and the cluster's status
// (ClusterStatus.upgrade) reports it.
type upgrade struct {
	// target is spec.control_plane.kubernetes_version; controlPlane what
	// the EKS cluster reports (status.atProvider.version).
	target, controlPlane string
	// nodes are the versions the node groups report; nodeGroups the same
	// by the node group's managed resource name, nodeSpecs the versions
	// they are applied at (spec.forProvider.version).
	nodes      []string
	nodeGroups map[string]string
	nodeSpecs  map[string]string
	// addons are the versions the Addon objects ask for now, by add-on.
	addons map[string]string
}

// componentsState is how the components stand for an upgrade's last two
// steps: every platform (tenant) component READY, and the ones that are
// not, by name.
type componentsState struct {
	platformReady, tenantReady   bool
	platformBehind, tenantBehind []string
}

// sameMinor reports whether two versions are of one minor ("1.37" and
// "1.37.2"); false when either is empty.
func sameMinor(a, b string) bool {
	return a != "" && b != "" && kubernetesMinor(a) == kubernetesMinor(b)
}

// controlPlaneBehind: the EKS cluster reports a version other than the
// target's minor (nothing reported yet: not behind, it is being created).
func (u upgrade) controlPlaneBehind() bool {
	return u.target != "" && u.controlPlane != "" && !sameMinor(u.controlPlane, u.target)
}

// nodesBehind is the version of a node group that does not run the target
// yet; empty when none is behind.
func (u upgrade) nodesBehind() string {
	for _, v := range u.nodes {
		if !sameMinor(v, u.target) {
			return v
		}
	}
	return ""
}

// nodesSettled: every node group reports the target, is applied at it, and
// is READY with its provider caught up (so not UPDATING).
func (u upgrade) nodesSettled(views []*cloudv1.ManagedResource) bool {
	if u.nodesBehind() != "" {
		return false
	}
	for _, v := range u.nodeSpecs {
		if !sameMinor(v, u.target) {
			return false
		}
	}
	return kindSettled(views, KindNodeGroup)
}

// addonsSettled: the cluster's generation is applied (no add-on is held at
// the version it ran) and every add-on is READY on the version it is
// applied at.
func addonsSettled(row *model.Cluster, views []*cloudv1.ManagedResource) bool {
	return row.AppliedGeneration >= row.Generation && kindSettled(views, "Addon")
}

// kindSettled: every object of kind is READY with its provider caught up.
func kindSettled(views []*cloudv1.ManagedResource, kind string) bool {
	for _, v := range views {
		if v.GetKind() != kind || v.GetState() == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING {
			continue
		}
		if v.GetState() != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY || reconcile.Behind(v) {
			return false
		}
	}
	return true
}

// GateUpgrade holds what an upgrade applies later back until its step:
// while the control plane is behind, the node groups render at the version
// it reports; until the upgrade reaches its add-ons' step, the add-ons keep
// the versions they run. So EKS never sees nodes or add-ons ahead of the
// cluster, and the add-ons move only once the node groups are settled. It
// reports whether the plan is gated; the cluster is then applied again
// (advance), and its generation does not count as applied, until nothing
// is held.
func (s *Reconciler) GateUpgrade(ctx context.Context, row *model.Cluster, plan *crossplaneaws.ClusterPlan) (bool, error) {
	if plan.KubernetesVersion == "" {
		return false, nil
	}
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, row.ID)
	if err != nil {
		return false, fmt.Errorf("list cluster resources: %w", err)
	}
	u, err := s.observeUpgrade(ctx, records, plan.KubernetesVersion)
	if err != nil {
		return false, err
	}
	step := stepUnspecified
	if stored := storedUpgrade(row); sameMinor(stored.GetToVersion(), u.target) {
		step = stepOf(stored)
	}
	holdNodes := u.controlPlaneBehind()
	holdAddons := holdNodes || (step != stepUnspecified && !reached(step, stepAddonsPhase)) ||
		(step == stepUnspecified && u.nodesBehind() != "")
	if holdNodes {
		plan.NodeKubernetesVersion = u.controlPlane
	}
	if holdAddons {
		for i, a := range plan.Addons {
			if v, ok := u.addons[a.Name]; ok {
				plan.Addons[i].Version = v
			}
		}
	}
	return holdNodes || holdAddons, nil
}

// observeUpgrade reads what an upgrade to target looks like now, from the
// cache: the version the EKS cluster reports, the versions the node
// groups report and are applied at, and the versions the Addon objects
// ask for (spec.forProvider).
func (s *Reconciler) observeUpgrade(ctx context.Context, records []model.ManagedResource, target string) (upgrade, error) {
	u := upgrade{target: target, addons: map[string]string{}, nodeGroups: map[string]string{}, nodeSpecs: map[string]string{}}
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
				u.nodeGroups[r.Name] = v
			}
			if v, _, _ := unstructured.NestedString(obj.Object, "spec", "forProvider", "version"); v != "" {
				u.nodeSpecs[r.Name] = v
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

// status is ClusterStatus.upgrade: the step the upgrade is at (nextStep),
// what it moves from and to, since when (kept from the status recorded
// before, stored; else the row's last change, the update that asked for
// it), and what the step waits for or, when the cloud fails it, the
// provider's error. Nil when no upgrade is in progress, and for clusters
// that are not applied (deleting, imported read-only).
func (u upgrade) status(row *model.Cluster, views []*cloudv1.ManagedResource, stored *cloudv1.ClusterUpgrade, comps componentsState) *cloudv1.ClusterUpgrade {
	if row.State == StateDeleting || row.State == StateImported || row.State == StateBlocked ||
		u.target == "" || u.controlPlane == "" {
		return nil
	}
	var prev *cloudv1.ClusterUpgrade
	if sameMinor(stored.GetToVersion(), u.target) {
		prev = stored
	}
	steps := settledSteps{controlPlane: !u.controlPlaneBehind(), nodes: u.nodesSettled(views),
		addons: addonsSettled(row, views), platform: comps.platformReady, tenant: comps.tenantReady}
	step, applied := nextStep(prev, steps, u.nodesBehind() != "")
	if step == stepUnspecified {
		return nil
	}
	out := &cloudv1.ClusterUpgrade{ToVersion: u.target, Phase: step, Applied: applied,
		StartTime: timestamppb.New(row.UpdatedAt), PhaseStartTime: timestamppb.New(now())}
	if prev.GetStartTime() != nil {
		out.StartTime = prev.GetStartTime()
	}
	if stepOf(prev) == step && prev.GetPhaseStartTime() != nil {
		out.PhaseStartTime = prev.GetPhaseStartTime()
	}
	switch out.FromVersion = prev.GetFromVersion(); {
	case out.FromVersion != "":
	case step == stepControlPlane:
		out.FromVersion = u.controlPlane
	default:
		out.FromVersion = orName(u.nodesBehind(), u.controlPlane)
	}
	out.Message = u.stepMessage(out, views, comps)
	return out
}

// stepMessage says what the upgrade's step waits for, setting the step's
// details on out; when the cloud fails the step, the provider's error for
// the resource concerned.
func (u upgrade) stepMessage(out *cloudv1.ClusterUpgrade, views []*cloudv1.ManagedResource, comps componentsState) string {
	switch out.GetPhase() {
	case stepControlPlane:
		if failure := failureOf(views, func(kind string) bool { return kind == "Cluster" }); failure != "" {
			return failure
		}
		return fmt.Sprintf("upgrading the control plane from %s to %s; the node groups, the add-ons and the components follow",
			u.controlPlane, u.target)
	case stepNodeGroups:
		out.NodeGroups, _ = u.following(views)
		if failure := failureOf(views, func(kind string) bool { return kind == KindNodeGroup }); failure != "" {
			return failure
		}
		return fmt.Sprintf("the control plane runs %s; %s; the add-ons follow", u.target, nodeGroupsMessage(out.GetNodeGroups()))
	case stepAddonsPhase:
		_, out.AddonsBehind = u.following(views)
		if failure := failureOf(views, func(kind string) bool { return kind == "Addon" }); failure != "" {
			return failure
		}
		if len(out.GetAddonsBehind()) > 0 {
			return fmt.Sprintf("the node groups run %s; add-ons updating: %s", u.target, strings.Join(out.GetAddonsBehind(), ", "))
		}
		return fmt.Sprintf("the node groups run %s; the add-ons move to the versions EKS recommends for it", u.target)
	case stepPlatformComps:
		out.ComponentsBehind = comps.platformBehind
		if len(comps.platformBehind) > 0 {
			return fmt.Sprintf("the add-ons are upgraded; platform components moving to %s: %s", u.target,
				strings.Join(comps.platformBehind, ", "))
		}
		return fmt.Sprintf("the add-ons are upgraded; the platform components move to %s", u.target)
	default:
		out.ComponentsBehind = comps.tenantBehind
		if len(comps.tenantBehind) > 0 {
			return "the platform components are upgraded; tenant components moving: " + strings.Join(comps.tenantBehind, ", ")
		}
		return "the platform components are upgraded; the tenant components move"
	}
}

// following is how the node groups and add-ons follow the control plane:
// each node group with the version the provider last saw it run and its
// state, and the add-ons not settled on their version yet (by add-on
// name, from "<cluster>:<add-on>").
func (u upgrade) following(views []*cloudv1.ManagedResource) ([]*cloudv1.ClusterUpgradeNodeGroup, []string) {
	var groups []*cloudv1.ClusterUpgradeNodeGroup
	var addons []string
	for _, v := range views {
		if v.GetState() == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING {
			continue
		}
		switch v.GetKind() {
		case KindNodeGroup:
			name := v.GetExternalName()
			if name == "" {
				name = v.GetName()
			}
			groups = append(groups, &cloudv1.ClusterUpgradeNodeGroup{Name: name, FromVersion: u.nodeGroups[v.GetName()],
				ToVersion: u.target, State: v.GetState()})
		case "Addon":
			if v.GetState() != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY || reconcile.Behind(v) {
				name := v.GetExternalName()
				if _, after, found := strings.Cut(name, ":"); found {
					name = after
				}
				addons = append(addons, orName(name, v.GetName()))
			}
		}
	}
	return groups, addons
}

func orName(name, fallback string) string {
	if name == "" {
		return fallback
	}
	return name
}

// nodeGroupsMessage says how the node groups stand, e.g. "node group
// prod-default 1.36 → 1.37 (updating)".
func nodeGroupsMessage(groups []*cloudv1.ClusterUpgradeNodeGroup) string {
	if len(groups) == 0 {
		return "the node groups move to it"
	}
	parts := make([]string, 0, len(groups))
	for _, g := range groups {
		state := strings.ToLower(strings.TrimPrefix(g.GetState().String(), "MANAGED_RESOURCE_STATE_"))
		if g.GetFromVersion() == "" || sameMinor(g.GetFromVersion(), g.GetToVersion()) {
			parts = append(parts, fmt.Sprintf("node group %s on %s (%s)", g.GetName(), g.GetToVersion(), state))
			continue
		}
		parts = append(parts, fmt.Sprintf("node group %s %s → %s (%s)", g.GetName(), g.GetFromVersion(), g.GetToVersion(), state))
	}
	return strings.Join(parts, "; ")
}

// upgrading makes the status of a cluster with an upgrade in progress say
// so: UPDATING, not RUNNING, and a Ready condition that is False with what
// the upgrade waits for, not "N of N resources ready". The provider sees
// the cloud at its poll interval, so a node group shows as updating until
// its next look.
func upgrading(st *cloudv1.ClusterStatus) {
	u := st.GetUpgrade()
	if u == nil || st.GetState() == cloudv1.ClusterState_CLUSTER_STATE_DELETING ||
		st.GetState() == cloudv1.ClusterState_CLUSTER_STATE_FAILED {
		return
	}
	message := fmt.Sprintf("upgrading %s to %s: %s", u.GetFromVersion(), u.GetToVersion(), u.GetMessage())
	st.State, st.Message = cloudv1.ClusterState_CLUSTER_STATE_UPDATING, message
	for _, c := range st.GetConditions() {
		if c.GetType() == conditionReady {
			c.Status, c.Reason, c.Message = cloudv1.ConditionStatus_CONDITION_STATUS_FALSE, "Upgrading", message
		}
	}
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
