package cluster

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"
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
	// karpenterNodes: the cluster has Karpenter node groups (the platform
	// node group is recorded READY), so their nodes follow the target too;
	// kubelets is what the pass read of them (nil: not read).
	karpenterNodes bool
	kubelets       *kubeletView
}

// componentsState is how the components stand for an upgrade's last two
// steps: every platform (tenant) component READY, and the ones that are
// not, by name.
type componentsState struct {
	platformReady, tenantReady   bool
	platformBehind, tenantBehind []string
	// platformUnchanged, tenantUnchanged: this pass wrote no Release of any
	// platform (tenant) component, there being at least one: their versions
	// and values are what the cluster already runs.
	platformUnchanged, tenantUnchanged bool
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
	if u.nodesBehind() != "" || !u.karpenterSettled() {
		return false
	}
	for _, v := range u.nodeSpecs {
		if !sameMinor(v, u.target) {
			return false
		}
	}
	return kindSettled(views, KindNodeGroup)
}

// karpenterSettled: no Karpenter node group, or every NodeClaim of the
// platform's pools has a node and every node of them runs the target's
// kubelet minor (the tenant's pools are the tenant's to roll:
// kubeletView.tenantBehind warns of them). A read that did not happen or
// is incomplete is not settled: the step waits.
func (u upgrade) karpenterSettled() bool {
	if !u.karpenterNodes {
		return true
	}
	if u.kubelets == nil || !u.kubelets.known || u.kubelets.launching > 0 {
		return false
	}
	for _, m := range u.kubelets.minors {
		if !sameMinor(m, u.target) {
			return false
		}
	}
	return true
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

// status is ClusterStatus.upgrade: the step the upgrade is at (nextStep),
// what it moves from and to, since when (kept from the status recorded
// before, stored; else the row's last change, the update that asked for
// it), and what the step waits for or, when the cloud fails it, the
// provider's error. Nil when no upgrade is in progress, and for clusters
// that are not applied (deleting, imported read-only).
func (u upgrade) status(row *model.Cluster, views []*cloudv1.ManagedResource, stored *cloudv1.ClusterUpgrade, comps componentsState) *cloudv1.ClusterUpgrade {
	if row.State == StateDeleting || row.State == StateImported || row.State == StateBlocked ||
		u.target == "" {
		return nil
	}
	var prev *cloudv1.ClusterUpgrade
	if sameMinor(stored.GetToVersion(), u.target) {
		prev = stored
	}
	if prev != nil {
		if upgradePauseReason(row, prev, views) != "" {
			return proto.CloneOf(prev)
		}
		prev = resumeUpgradeClock(row, prev)
	}
	if u.controlPlane == "" {
		return nil
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
	if u.kubelets != nil {
		out.TenantPoolsBehind = u.kubelets.tenantBehind(u.target)
	}
	out.SkippedSteps = skippedSteps(prev, out, comps)
	if slices.Contains(out.GetSkippedSteps(), step) {
		out.Message += "; component versions unchanged, nothing to do in this step"
	}
	if upgradePauseReason(row, out, views) != "" {
		out.Applied = false
	} else if step == stepAddonsPhase && stepOf(prev) == step {
		watchAddons(row, prev, out, views)
	}
	return out
}

// skippedSteps are the component steps found to have nothing to do: those
// of prev, and the step out is at when this pass is the one that applies it
// (prev was at it, not applied yet: the components were planned for the
// new minor in this pass) and no component changed. Decided once, in that
// pass: after it the Releases match whatever they were written for.
func skippedSteps(prev, out *cloudv1.ClusterUpgrade, comps componentsState) []cloudv1.ClusterUpgradePhase {
	skipped := slices.Clone(prev.GetSkippedSteps())
	step := out.GetPhase()
	applying := stepOf(prev) == step && !prev.GetApplied()
	switch {
	case !applying, slices.Contains(skipped, step):
	case step == stepPlatformComps && comps.platformUnchanged, step == stepTenantComps && comps.tenantUnchanged:
		skipped = append(skipped, step)
	}
	return skipped
}

// watchAddons is the add-ons' step watched as a release move watches a
// component: add-ons that fail (the provider's error, e.g. AWS's
// UPDATE_FAILED or DEGRADED) or do not settle within componentWatch are
// recovered, applied again (recoverAddons: the provider updates them with
// resolveConflicts OVERWRITE), up to maxMoveRecoveries times; then the
// upgrade halts, saying why, until cluster retry.
func watchAddons(row *model.Cluster, prev, out *cloudv1.ClusterUpgrade, views []*cloudv1.ManagedResource) {
	if row.Retry != nil {
		return // a person retried: the watch starts over (recoveries 0)
	}
	out.Recoveries, out.Halted = prev.GetRecoveries(), prev.GetHalted()
	if out.GetHalted() {
		out.Message = prev.GetMessage()
		return
	}
	since := now().Sub(out.GetPhaseStartTime().AsTime())
	failure := addonFailure(views)
	failed := failure != "" && (out.GetRecoveries() == 0 || since > recoveryGrace)
	if !out.GetApplied() || (!failed && since <= componentWatch) {
		return
	}
	why := orDefault(failure, fmt.Sprintf("not settled within %s", componentWatch))
	names := strings.Join(out.GetAddonsBehind(), ", ")
	if out.GetRecoveries() < maxMoveRecoveries {
		out.Recoveries++
		out.PhaseStartTime = timestamppb.New(now()) // the watch starts over
		out.Message = fmt.Sprintf("recovering the add-ons %s (attempt %d of %d): %s", names, out.GetRecoveries(), maxMoveRecoveries, why)
		return
	}
	out.Halted = true
	out.Message = fmt.Sprintf("halted: the add-ons %s did not settle after %d recoveries: %s; cluster retry --id %s tries again",
		names, maxMoveRecoveries, why, row.ID)
}

// addonFailure is why an add-on fails: the provider's error, or the
// failed state AWS reports (CREATE_FAILED, UPDATE_FAILED, DEGRADED come as
// a Ready condition that is False, with its reason); "" when none fails.
func addonFailure(views []*cloudv1.ManagedResource) string {
	if failure := failureOf(views, func(kind string) bool { return kind == "Addon" }); failure != "" {
		return failure
	}
	for _, v := range views {
		if v.GetKind() == "Addon" && v.GetState() == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_FAILED {
			return strings.TrimSpace(fmt.Sprintf("Addon %s: %s %s", v.GetName(), v.GetReason(), v.GetMessage()))
		}
	}
	return ""
}

// addonsRecovering reports whether a pass's upgrade status recovers the
// add-ons: it counts one more recovery than the status stored before.
func addonsRecovering(before, after *cloudv1.ClusterUpgrade) bool {
	return after.GetPhase() == stepAddonsPhase && after.GetRecoveries() > before.GetRecoveries() && !after.GetHalted()
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

// Upgrading reports whether the status recorded with the row has an
// upgrade in progress.
func Upgrading(row *model.Cluster) bool { return storedUpgrade(row) != nil }

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
