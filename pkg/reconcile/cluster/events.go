package cluster

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// object is the Cluster object events of row are about.
func object(row *model.Cluster) reconcile.Object {
	return reconcile.Object{Kind: reconcile.KindCluster, Namespace: kube.TenantNamespace(row.TenantID), Name: row.Name}
}

// resourcesLeftPattern is the count in a deletion's message: it changes
// with every object that goes, which is not news.
var resourcesLeftPattern = regexp.MustCompile(`; [0-9]+ resources left`)

// statusEvents says what happened between two recorded statuses of a
// cluster: the events of a pass. before is what the last pass recorded
// (empty for the first), after what this one found. A pass that found
// things as they were gives none.
func statusEvents(before, after *cloudv1.ClusterStatus) []reconcile.Note {
	out := reconcile.PassFailure(before.GetConditions(), after.GetConditions(), ConditionReconciled)
	out = append(out, reconcile.PauseNotes(before.GetConditions(), after.GetConditions())...)
	out = append(out, resourceEvents(before, after)...) // the steps first, then the state they led to
	out = append(out, stateEvents(before, after)...)
	out = append(out, upgradeEvents(before, after)...)
	out = append(out, platformEvents(before, after)...)
	out = append(out, moveEvents(before, after)...)
	out = append(out, kubernetesEvents(before, after)...)
	return out
}

// kubernetesEvents are the cluster's Kubernetes minor becoming ending.
func kubernetesEvents(before, after *cloudv1.ClusterStatus) []reconcile.Note {
	if after.GetKubernetes().GetEnding() && !before.GetKubernetes().GetEnding() {
		return []reconcile.Note{reconcile.Warning(reconcile.ReasonKubernetesEnding, after.GetKubernetes().GetMessage())}
	}
	return nil
}

// stateEvents are the events of the cluster's state: reaching it, and,
// while it stays FAILED or DELETING, a change of what it says.
func stateEvents(before, after *cloudv1.ClusterStatus) []reconcile.Note {
	was, is := before.GetState(), after.GetState()
	imported := importEvents(before, after)
	switch is {
	case cloudv1.ClusterState_CLUSTER_STATE_PROVISIONING:
		if was != is && len(imported) == 0 {
			return []reconcile.Note{reconcile.Normal(reconcile.ReasonProvisioning, "creating the cluster's resources")}
		}
	case cloudv1.ClusterState_CLUSTER_STATE_RUNNING:
		if was != is {
			return append(imported, reconcile.Normal(reconcile.ReasonRunning,
				fmt.Sprintf("generation %d is applied and all %d resources are ready",
					after.GetObservedGeneration(), len(after.GetManagedResources()))))
		}
	case cloudv1.ClusterState_CLUSTER_STATE_UPDATING:
		if was != is && len(imported) == 0 {
			return []reconcile.Note{reconcile.Normal(reconcile.ReasonUpdating, orDefault(after.GetMessage(), "applying a change"))}
		}
	case cloudv1.ClusterState_CLUSTER_STATE_FAILED:
		if after.GetImport().GetPhase() == cloudv1.ImportPhase_IMPORT_PHASE_BLOCKED ||
			reconcile.PassFailed(after.GetConditions(), ConditionReconciled, after.GetMessage()) {
			break // ImportBlocked, or the failed pass (PassFailure), says it
		}
		if was != is || failing(before.GetMessage()) != failing(after.GetMessage()) {
			return []reconcile.Note{reconcile.Warning(reconcile.ReasonFailed, orDefault(after.GetMessage(), "the cluster failed"))}
		}
	case cloudv1.ClusterState_CLUSTER_STATE_DELETING:
		if was != is || waitingFor(before.GetMessage()) != waitingFor(after.GetMessage()) {
			return []reconcile.Note{reconcile.Normal(reconcile.ReasonDeleting, orDefault(after.GetMessage(), "deleting"))}
		}
	default:
	}
	return imported
}

// importEvents are the events of an import: discovered, blocked, taken
// over.
func importEvents(before, after *cloudv1.ClusterStatus) []reconcile.Note {
	was, is := before.GetImport().GetPhase(), after.GetImport().GetPhase()
	if was == is {
		return nil
	}
	switch is {
	case cloudv1.ImportPhase_IMPORT_PHASE_PLANNED:
		return []reconcile.Note{reconcile.Normal(reconcile.ReasonImportDiscovered,
			fmt.Sprintf("found the existing cluster's resources (%d); observed only until it is taken over",
				len(after.GetImport().GetResources())))}
	case cloudv1.ImportPhase_IMPORT_PHASE_BLOCKED:
		return []reconcile.Note{reconcile.Warning(reconcile.ReasonImportBlocked, "the import cannot be taken over: "+firstBlocking(after.GetImport()))}
	case cloudv1.ImportPhase_IMPORT_PHASE_CONVERGING:
		return []reconcile.Note{reconcile.Normal(reconcile.ReasonTakenOver, "the platform owns the cluster's resources now; applying them")}
	default:
		return nil
	}
}

// resourceEvents are the steps of creating a cluster that its resources
// show: the EKS cluster ACTIVE, the node group ready, the add-ons ready.
func resourceEvents(before, after *cloudv1.ClusterStatus) []reconcile.Note {
	var out []reconcile.Note
	if !allReady(before, "Cluster") && allReady(after, "Cluster") {
		msg := "the EKS cluster is ACTIVE"
		if v := after.GetCurrentKubernetesVersion(); v != "" {
			msg += " on Kubernetes " + v
		}
		out = append(out, reconcile.Normal(reconcile.ReasonControlPlaneActive, msg))
	}
	if !allReady(before, "NodeGroup") && allReady(after, "NodeGroup") {
		out = append(out, reconcile.Normal(reconcile.ReasonNodeGroupReady, "the default node group is ready"))
	}
	if !allReady(before, "Addon") && allReady(after, "Addon") {
		out = append(out, reconcile.Normal(reconcile.ReasonAddonsReady,
			fmt.Sprintf("all %d add-ons are ready", count(after, "Addon"))))
	}
	return out
}

// allReady reports whether st has objects of kind and every one is READY.
func allReady(st *cloudv1.ClusterStatus, kind string) bool {
	found := false
	for _, v := range st.GetManagedResources() {
		if v.GetKind() != kind {
			continue
		}
		if v.GetState() != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY {
			return false
		}
		found = true
	}
	return found
}

func count(st *cloudv1.ClusterStatus, kind string) int {
	n := 0
	for _, v := range st.GetManagedResources() {
		if v.GetKind() == kind {
			n++
		}
	}
	return n
}

// upgradeEvents are a Kubernetes upgrade starting, reaching its node
// groups, and finishing.
func upgradeEvents(before, after *cloudv1.ClusterStatus) []reconcile.Note {
	was, is := before.GetUpgrade(), after.GetUpgrade()
	switch {
	case was == nil && is != nil:
		return []reconcile.Note{reconcile.Normal(reconcile.ReasonUpgradeStarted,
			fmt.Sprintf("upgrading Kubernetes from %s to %s: the control plane first", is.GetFromVersion(), is.GetToVersion()))}
	case was.GetPhase() == cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_CONTROL_PLANE &&
		is.GetPhase() == cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_NODES_AND_ADDONS:
		names := make([]string, 0, len(is.GetNodeGroups()))
		for _, g := range is.GetNodeGroups() {
			names = append(names, g.GetName())
		}
		what := "the node groups"
		if len(names) > 0 {
			what = "node group " + strings.Join(names, ", ")
		}
		return []reconcile.Note{reconcile.Normal(reconcile.ReasonUpgradeNodes,
			fmt.Sprintf("the control plane runs %s; upgrading %s to %s, then the add-ons", is.GetToVersion(), what, is.GetToVersion()))}
	case was != nil && is == nil && after.GetState() != cloudv1.ClusterState_CLUSTER_STATE_DELETING:
		return []reconcile.Note{reconcile.Normal(reconcile.ReasonUpgradeFinished,
			fmt.Sprintf("Kubernetes %s: control plane, node groups and add-ons upgraded", was.GetToVersion()))}
	default:
		return nil
	}
}

// platformEvents are what changed about the platform components: a
// component entering INSTALLING, READY or FAILED, one that is no longer
// installed, a stuck release that was recovered, and all of them ready.
func platformEvents(before, after *cloudv1.ClusterStatus) []reconcile.Note {
	var out []reconcile.Note
	was := map[string]*cloudv1.PlatformComponent{}
	for _, c := range before.GetPlatformComponents() {
		was[c.GetName()] = c
	}
	for _, c := range after.GetPlatformComponents() {
		prev, known := was[c.GetName()]
		delete(was, c.GetName())
		name := c.GetName()
		if v := c.GetVersion(); v != "" {
			name += " " + v
		}
		if known && c.GetRecoveries() > prev.GetRecoveries() {
			out = append(out, reconcile.Warning(reconcile.ReasonStuckReleaseRecovered,
				fmt.Sprintf("%s: removed the record of its stuck Helm release so it installs again (recovery %d of %d)",
					name, c.GetRecoveries(), maxRecoveries)))
			if c.GetRecoveries() >= maxRecoveries {
				out = append(out, reconcile.Warning(reconcile.ReasonRecoveryLimitReached,
					fmt.Sprintf("%s was recovered %d times and is not recovered again: look at chart %s in the cluster",
						name, c.GetRecoveries(), orDefault(c.GetChart(), c.GetName()))))
			}
		}
		if known && prev.GetPhase() == c.GetPhase() {
			continue
		}
		switch c.GetPhase() {
		case cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED:
			out = append(out, reconcile.Warning(reconcile.ReasonPlatformComponentFailed, name+": "+componentFailure(c)))
		case cloudv1.ComponentPhase_COMPONENT_PHASE_READY:
			out = append(out, reconcile.Normal(reconcile.ReasonPlatformComponentReady, name+" is installed"))
		case cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING:
			out = append(out, reconcile.Normal(reconcile.ReasonPlatformComponentInstalling, "installing "+name))
		default: // waiting is not news: the status says what for
		}
	}
	for _, c := range before.GetPlatformComponents() { // in the recorded order
		if _, gone := was[c.GetName()]; gone && after.GetState() != cloudv1.ClusterState_CLUSTER_STATE_DELETING {
			out = append(out, reconcile.Normal(reconcile.ReasonPlatformComponentRemoved, c.GetName()+" is no longer installed"))
		}
	}
	if is, then := conditionOf(after, ConditionPlatformComponents), conditionOf(before, ConditionPlatformComponents); is != nil &&
		is.GetStatus() == cloudv1.ConditionStatus_CONDITION_STATUS_TRUE &&
		(then == nil || then.GetStatus() != cloudv1.ConditionStatus_CONDITION_STATUS_TRUE) {
		out = append(out, reconcile.Normal(reconcile.ReasonPlatformComponentsReady, orDefault(is.GetMessage(), "every platform component is installed")))
	}
	return out
}

// componentFailure is why a platform component failed, with what the Helm
// provider says of its chart.
func componentFailure(c *cloudv1.PlatformComponent) string {
	msg := orDefault(c.GetMessage(), "failed")
	if c.GetChart() != "" {
		msg += fmt.Sprintf(" (chart %s", c.GetChart())
		if c.GetHelmState() != "" {
			msg += ", Helm state " + c.GetHelmState()
		}
		if c.GetFailures() > 0 {
			msg += fmt.Sprintf(", failed %d times", c.GetFailures())
		}
		msg += ")"
	}
	if e := c.GetLastError(); e != "" && e != c.GetMessage() {
		msg += ": " + e
	}
	return msg
}

func conditionOf(st *cloudv1.ClusterStatus, kind string) *cloudv1.Condition {
	for _, c := range st.GetConditions() {
		if c.GetType() == kind {
			return c
		}
	}
	return nil
}

// waitingFor is what a deletion's message says it waits for: without the
// count of resources, and with only the fact that something is stuck
// (its details carry a duration that grows).
func waitingFor(message string) string {
	message = resourcesLeftPattern.ReplaceAllString(message, "")
	if i := strings.Index(message, "; stuck: "); i >= 0 {
		message = message[:i] + "; stuck"
	}
	return message
}

// failing is what a failure's message is about: the part before the
// provider's own text ("<Kind> <name>"), which may differ on every try.
func failing(message string) string {
	if i := strings.Index(message, ": "); i >= 0 {
		return message[:i]
	}
	return message
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// happened is what a pass has to report about the cluster: what its
// steps said themselves (reconcile.AddNote: a repair), then what the
// status change means (statusEvents).
func happened(ctx context.Context, before, after *cloudv1.ClusterStatus) []reconcile.Note {
	return append(reconcile.Notes(ctx), statusEvents(before, after)...)
}

// emit reports notes as events of the cluster's object.
func (s *Reconciler) emit(ctx context.Context, row *model.Cluster, notes []reconcile.Note) {
	for _, n := range notes {
		s.deps.Emit(ctx, object(row), n.Type, n.Reason, n.Message)
	}
}
