import { ComponentManagement, ComponentPhase, type Component } from "@/gen/cloud/v1/component_pb";
import type { PlatformComponent } from "@/gen/cloud/v1/cluster_pb";

export type InstallationStatus = { label: string; tone: "neutral" | "success" | "warning" | "error"; key: string };

export function componentInstallationStatus(component: Component): InstallationStatus {
  // ListComponents includes catalog placeholders. Their stale phase must never
  // make an allocation without an enabled tenant Component look installed.
  if (component.management === ComponentManagement.TENANT && !component.id) return { label: "Disabled", tone: "neutral", key: "disabled" };
  const phase = component.status?.phase;
  if (phase === undefined || phase === ComponentPhase.UNSPECIFIED) return { label: "Not reported", tone: "neutral", key: "not-reported" };
  if (phase === ComponentPhase.READY && component.management !== ComponentManagement.PLATFORM &&
    (component.generation <= 0n || (component.status?.observedGeneration ?? 0n) < component.generation)) {
    return { label: "Reconciling", tone: "warning", key: String(phase) };
  }
  return componentPhaseStatus(phase);
}

export function platformInstallationStatus(component: PlatformComponent): InstallationStatus {
  return componentPhaseStatus(component.phase);
}

function componentPhaseStatus(phase: ComponentPhase): InstallationStatus {
  const label = ComponentPhase[phase]?.toLowerCase().replaceAll("_", " ").replace(/^./, (letter) => letter.toUpperCase()) ?? `Unknown (${phase})`;
  return { label, key: String(phase), tone: phase === ComponentPhase.READY ? "success" : phase === ComponentPhase.FAILED ? "error" :
    [ComponentPhase.DEGRADED, ComponentPhase.DELETING, ComponentPhase.PENDING, ComponentPhase.INSTALLING].includes(phase) ? "warning" : "neutral" };
}
