package cluster

import (
	"fmt"

	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// managedLifecycleEvents compares persisted observations, so restarts and
// unchanged reconcile passes do not replay lifecycle starts or completions.
func managedLifecycleEvents(before, after *v1.ClusterStatus) []reconcile.Note {
	if after.GetState() == v1.ClusterState_CLUSTER_STATE_IMPORTED {
		return nil
	}
	previous := map[string]*v1.ManagedResource{}
	current := map[string]bool{}
	for _, resource := range before.GetManagedResources() {
		previous[resource.Kind+"/"+resource.Name] = resource
	}
	var notes []reconcile.Note
	for _, resource := range after.GetManagedResources() {
		prefix, label := "", ""
		switch resource.GetKind() {
		case "NodeGroup":
			prefix, label = "NodeGroup", "node group"
		case "Addon":
			prefix, label = "Addon", "add-on"
		default:
			continue
		}
		key := resource.Kind + "/" + resource.Name
		current[key] = true
		prev := previous[key]
		if resource.GetPaused() {
			continue
		}
		name := resource.Name
		if prefix == "Addon" {
			name = addonName(name)
		}
		normal := func(suffix, message string) {
			notes = append(notes, reconcile.Normal(prefix+suffix, label+" "+name+" "+message))
		}
		state := resource.GetState()
		switch {
		case state == v1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING:
			if prev != nil && prev.GetState() != state {
				normal("Deleted", "managed resource was removed")
			}
		case state == v1.ManagedResourceState_MANAGED_RESOURCE_STATE_DELETING:
			if prev == nil || prev.GetState() != state {
				normal("Deleting", "is being deleted")
			}
		case state == v1.ManagedResourceState_MANAGED_RESOURCE_STATE_FAILED:
			if prev == nil || prev.GetState() != state || prev.GetReason() != resource.GetReason() {
				notes = append(notes, reconcile.Warning(prefix+"Failed", fmt.Sprintf("%s %s: %s", label, name, resource.GetMessage())))
			}
		case prev == nil || prev.GetState() == v1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING:
			if state == v1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY {
				suffix := "Created"
				if prefix == "Addon" {
					suffix = "Installed"
				}
				normal(suffix, "is ready")
			} else {
				suffix := "Creating"
				if prefix == "Addon" {
					suffix = "Installing"
				}
				normal(suffix, "is being installed")
			}
		case state == v1.ManagedResourceState_MANAGED_RESOURCE_STATE_UPDATING && prev.GetState() != state ||
			resource.GetGeneration() > prev.GetGeneration() && (prev.GetState() == v1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY || prev.GetObservedGeneration() > 0):
			normal("Upgrading", "is being updated")
			if state == v1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY && resource.GetObservedGeneration() >= resource.GetGeneration() {
				normal("Upgraded", "is ready")
			}
		case state == v1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY && (prev.GetState() != state || prev.GetObservedGeneration() > 0 && prev.GetGeneration() > prev.GetObservedGeneration() && resource.GetObservedGeneration() >= resource.GetGeneration()):
			suffix := "Created"
			if prefix == "Addon" {
				suffix = "Installed"
			}
			if prev.GetState() == v1.ManagedResourceState_MANAGED_RESOURCE_STATE_UPDATING || prev.GetObservedGeneration() > 0 && prev.GetGeneration() > prev.GetObservedGeneration() {
				suffix = "Upgraded"
			}
			normal(suffix, "is ready")
		}
	}
	for _, resource := range before.GetManagedResources() {
		if resource.GetKind() != "NodeGroup" && resource.GetKind() != "Addon" {
			continue
		}
		if current[resource.Kind+"/"+resource.Name] || resource.GetState() == v1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING {
			continue
		}
		if resource.GetState() != v1.ManagedResourceState_MANAGED_RESOURCE_STATE_DELETING {
			notes = append(notes, reconcile.Normal(resource.Kind+"Deleting", resource.Kind+" "+resource.Name+" is being removed"))
		}
		notes = append(notes, reconcile.Normal(resource.Kind+"Deleted", resource.Kind+" "+resource.Name+" was removed from platform inventory"))
	}
	return notes
}

func tenantComponentEvents(name string, before, after *v1.ComponentStatus) []reconcile.Note {
	var recovered []reconcile.Note
	if after.GetRecoveries() > before.GetRecoveries() {
		recovered = append(recovered, reconcile.Warning(reconcile.ReasonStuckReleaseRecovered,
			fmt.Sprintf("component %s: %s so it installs again (recovery %d of %d)", name,
				orDefault(after.GetLastRecovery(), "recovered its stuck Helm release"), after.GetRecoveries(), maxRecoveries)))
	}
	return append(recovered, tenantPhaseEvents(name, before, after)...)
}

func tenantPhaseEvents(name string, before, after *v1.ComponentStatus) []reconcile.Note {
	sameTarget := before.GetTargetVersion() == after.GetTargetVersion() && before.GetObservedGeneration() == after.GetObservedGeneration()
	if before.GetPhase() == after.GetPhase() && sameTarget {
		return nil
	}
	message := "component " + name
	if after.GetTargetVersion() != "" {
		message += " " + after.GetTargetVersion()
	}
	var reason string
	switch after.GetPhase() {
	case v1.ComponentPhase_COMPONENT_PHASE_INSTALLING:
		reason = reconcile.ReasonComponentInstalling
		if before.GetInstalledVersion() != "" {
			reason = reconcile.ReasonComponentUpgrading
		}
	case v1.ComponentPhase_COMPONENT_PHASE_READY:
		reason = reconcile.ReasonComponentReady
		if before.GetInstalledVersion() != "" && (before.GetPhase() != after.GetPhase() || before.GetInstalledVersion() != after.GetInstalledVersion()) {
			reason = reconcile.ReasonComponentUpgraded
		}
	case v1.ComponentPhase_COMPONENT_PHASE_DELETING:
		reason = reconcile.ReasonComponentDeleting
	case v1.ComponentPhase_COMPONENT_PHASE_FAILED:
		return []reconcile.Note{reconcile.Warning(reconcile.ReasonComponentFailed, message+": "+after.GetMessage())}
	default:
		return nil
	}
	return []reconcile.Note{reconcile.Normal(reason, message+": "+componentEventAction(reason))}
}

func platformInstallingReason(before, after *v1.PlatformComponent) string {
	if before.GetVersion() != "" && (before.GetVersion() != after.GetVersion() || before.GetPhase() == v1.ComponentPhase_COMPONENT_PHASE_READY) {
		return reconcile.ReasonPlatformComponentUpgrading
	}
	return reconcile.ReasonPlatformComponentInstalling
}

func platformReadyReason(before, after *v1.PlatformComponent) string {
	installed := before.GetInstalledVersion()
	if installed == "" && before.GetPhase() == v1.ComponentPhase_COMPONENT_PHASE_READY {
		installed = before.GetVersion()
	}
	if installed != "" && installed != after.GetVersion() {
		return reconcile.ReasonPlatformComponentUpgraded
	}
	return reconcile.ReasonPlatformComponentReady
}

func componentEventAction(reason string) string {
	switch reason {
	case reconcile.ReasonComponentInstalling:
		return "installation started"
	case reconcile.ReasonComponentUpgrading:
		return "upgrade started"
	case reconcile.ReasonComponentDeleting:
		return "deletion started"
	case reconcile.ReasonComponentUpgraded:
		return "upgrade completed"
	default:
		return "ready"
	}
}
