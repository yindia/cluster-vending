import type { NodeGroupManifest } from "../../gen/cloud/v1/node_group_manifest_pb";
import { NodeGroupPhase, type NodeGroupStatus } from "../../gen/cloud/v1/node_group_pb";

/**
 * Node groups are read-only in the console. The platform creates the system node group (the default EKS managed
 * node group) and the Karpenter platform pool from its own configuration; users neither define nor change them.
 */
/** The platform's default EKS managed node group. */
export const SYSTEM_GROUP = "system";
/** The platform's Karpenter NodePool. */
export const PLATFORM_POOL = "platform-pool";
/** "platform" was a Karpenter pool of an earlier layout; a status an earlier controller recorded for it is never a row. */
const LEGACY_PLATFORM_POOL = "platform";
/** The note every platform-managed group is shown with. */
export const PLATFORM_MANAGED_LABEL = "Managed by the platform";

export type NodeGroupTone = "success" | "error" | "warning" | "neutral";
export interface NodeGroupRow extends Record<string, unknown> {
  name: string; platformManaged: boolean;
  /** False while the controller has not reported the group yet. */
  observed: boolean;
  phase: NodeGroupPhase; phaseLabel: string; tone: NodeGroupTone; message: string;
  nodes?: number;
  /** The group's Karpenter objects as the controller applies them; absent while unknown, and for the system group (an EKS node group, not Karpenter objects). */
  manifest?: NodeGroupManifest;
}

const phaseLabels: Record<number, [string, NodeGroupTone]> = {
  [NodeGroupPhase.UNSPECIFIED]: ["Not reported", "neutral"], [NodeGroupPhase.PENDING]: ["Pending", "warning"],
  [NodeGroupPhase.READY]: ["Ready", "success"], [NodeGroupPhase.FAILED]: ["Failed", "error"], [NodeGroupPhase.DELETING]: ["Deleting", "warning"],
};
/** The label and tone a node group phase is shown with. */
export function phaseDisplay(phase: NodeGroupPhase): [string, NodeGroupTone] {
  return phaseLabels[phase] ?? [`Unknown (${phase})`, "neutral"];
}
const waiting = "Waiting for the controller to report this node group.";
const systemWaiting = "Pending — waiting for the controller";

/** Manifests with something to show: the system group's entry has no YAML. */
export function hasManifestYaml(manifest: NodeGroupManifest | undefined): manifest is NodeGroupManifest {
  return !!manifest && !!(manifest.ec2NodeClassYaml || manifest.nodePoolYaml);
}

/**
 * The cluster's node groups as the controller reports them: the platform's groups first (the system group, then the
 * Karpenter platform pool), then any other group still reported (one an earlier layout left behind, while it drains).
 * The system row is always shown (Pending until the controller reports it) unless `manifests` is known and has no
 * platform entry: the platform manages no node groups of an imported cluster. A platform-managed manifest the status
 * does not list yet is a Pending row. Each row carries its manifest by name.
 */
export function nodeGroupRows(status: readonly NodeGroupStatus[], manifests?: readonly NodeGroupManifest[]): NodeGroupRow[] {
  // First report wins: a repeated name must never produce two rows (the table keys rows by name).
  const byName = new Map<string, NodeGroupStatus>();
  for (const item of status) if (!byName.has(item.name) && !(item.platformManaged && item.name === LEGACY_PLATFORM_POOL)) byName.set(item.name, item);
  const manifestOf = new Map<string, NodeGroupManifest>();
  for (const item of manifests ?? []) if (!manifestOf.has(item.name)) manifestOf.set(item.name, item);
  const row = (name: string, observed: NodeGroupStatus | undefined, platform = false): NodeGroupRow => {
    const phase = observed?.phase ?? NodeGroupPhase.PENDING;
    const [phaseLabel, tone] = phaseDisplay(phase);
    const manifest = manifestOf.get(name);
    return {
      name, platformManaged: observed?.platformManaged ?? platform, observed: !!observed,
      phase, phaseLabel, tone, message: observed ? observed.message : platform ? systemWaiting : waiting, nodes: observed?.nodes,
      manifest: hasManifestYaml(manifest) ? manifest : undefined,
    };
  };
  const reported = [...byName.values()];
  const platform = reported.filter(item => item.platformManaged).map(item => row(item.name, item));
  const managesNodes = !manifests || manifests.some(item => item.platformManaged);
  if (managesNodes && !platform.some(item => item.name === SYSTEM_GROUP)) platform.unshift(row(SYSTEM_GROUP, undefined, true));
  for (const item of manifests ?? []) {
    if (item.platformManaged && !platform.some(existing => existing.name === item.name)) platform.push(row(item.name, undefined, true));
  }
  // The system group, then the platform pool, then any other platform group in report order.
  const rank = (name: string) => name === SYSTEM_GROUP ? 0 : name === PLATFORM_POOL ? 1 : 2;
  const ordered = platform.map((item, index) => ({ item, index })).sort((a, b) => rank(a.item.name) - rank(b.item.name) || a.index - b.index).map(({ item }) => item);
  const others = reported.filter(item => !item.platformManaged && !ordered.some(existing => existing.name === item.name)).map(item => row(item.name, item));
  return [...ordered, ...others];
}
