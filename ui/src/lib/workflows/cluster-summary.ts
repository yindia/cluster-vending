import type { Timestamp } from "@bufbuild/protobuf/wkt";
import type { ClusterCount } from "../../gen/cloud/v1/cluster_summary_pb";
import type { ClusterStatus } from "../../gen/cloud/v1/cluster_pb";
import { ConditionStatus } from "../../gen/cloud/v1/conditions_pb";
import type { NodeGroupStatus } from "../../gen/cloud/v1/node_group_pb";
import { phaseDisplay, type NodeGroupTone } from "./node-groups";

// The ages after which the cluster status summary is "old" (PROP-02 5.2):
// the node counts after 15 minutes, the policy counts after 30.
export const NODES_OLD_AFTER_MS = 15 * 60_000;
export const POLICY_OLD_AFTER_MS = 30 * 60_000;

// Requested shares of allocatable at which a meter turns warning, then error:
// past them new pods start to wait for capacity.
export const UTILIZATION_WARNING_PERCENT = 75;
export const UTILIZATION_ERROR_PERCENT = 90;

export type SummaryState = "not-read" | "read" | "unreachable";

export type SummaryTone = "success" | "warning" | "error" | "neutral";

export type SummaryItem = {
  label: string; value: string; note: string; old: boolean;
  // The counts under the total: severities or event reasons, largest first.
  breakdown?: readonly ClusterCount[];
};

export type ReachabilityView = { value: string; note: string; tone: SummaryTone };

export type ClusterSummaryView = {
  state: SummaryState;
  // Shown as a banner when the platform cannot read the cluster.
  notice?: string;
  items: SummaryItem[];
  reachability: ReachabilityView;
};

export const NOT_READ = "Not read yet";

function millis(t: Timestamp | undefined): number | undefined {
  return t ? Number(t.seconds) * 1000 + Math.floor(t.nanos / 1e6) : undefined;
}

export function ageLabel(t: Timestamp | undefined, now: number): string {
  const at = millis(t);
  if (at === undefined) return "never";
  const minutes = Math.max(0, Math.floor((now - at) / 60_000));
  if (minutes < 1) return "just now";
  if (minutes < 60) return `${minutes} min ago`;
  const hours = Math.floor(minutes / 60);
  return hours < 48 ? `${hours} h ago` : `${Math.floor(hours / 24)} d ago`;
}

function isOld(t: Timestamp | undefined, now: number, after: number): boolean {
  const at = millis(t);
  return at !== undefined && now - at > after;
}

function collected(t: Timestamp | undefined, now: number, after: number): string {
  return `Collected ${ageLabel(t, now)}${isOld(t, now, after) ? " · old" : ""}`;
}

function largestFirst(list: readonly ClusterCount[]): ClusterCount[] {
  return [...list].sort((a, b) => b.count - a.count || a.name.localeCompare(b.name));
}

// severityTone is how a violation severity is shown: critical and high need
// attention, medium is a warning, the rest are informational.
export function severityTone(name: string): SummaryTone {
  const severity = name.toLowerCase();
  if (severity === "critical" || severity === "high") return "error";
  if (severity === "medium") return "warning";
  return "neutral";
}

// reachabilityView is whether the last controller pass could read the
// workload cluster, and how long ago one last did.
export function reachabilityView(status: ClusterStatus | undefined, now: number): ReachabilityView {
  const reachable = status?.reachable;
  if (!reachable || reachable.status === ConditionStatus.UNSPECIFIED) return { value: NOT_READ, note: "Checked on every controller pass", tone: "neutral" };
  const last = `Last read ${ageLabel(reachable.lastSuccessfulAt, now)}`;
  if (reachable.status === ConditionStatus.FALSE) return { value: "Unreachable", note: [reachable.reason || "unreachable", last].join(" · "), tone: "error" };
  if (reachable.status === ConditionStatus.TRUE) {
    const old = isOld(reachable.lastSuccessfulAt, now, NODES_OLD_AFTER_MS);
    return { value: "Reachable", note: old ? `${last} · old` : last, tone: old ? "warning" : "success" };
  }
  return { value: "Unknown", note: reachable.message || last, tone: "warning" };
}

// clusterSummaryView is what the overview shows of the cluster status
// summary: "not read yet" (never zeros) before a controller pass, the counts
// with their ages and the "old" marking, and, when the platform cannot read
// the cluster, the last counts with the reason.
export function clusterSummaryView(status: ClusterStatus | undefined, now: number): ClusterSummaryView {
  const nodes = status?.nodes;
  const violations = status?.violations;
  const events = status?.violationEvents;
  const reachable = status?.reachable;
  const unreachable = reachable?.status === ConditionStatus.FALSE;
  const state: SummaryState = unreachable ? "unreachable" : nodes || violations || events || reachable ? "read" : "not-read";
  const nodesNote = nodes && [nodes.notReady > 0 ? `${nodes.notReady} not ready` : "", collected(nodes.nodesCollectedAt, now, NODES_OLD_AFTER_MS)].filter(Boolean).join(" · ");
  const items: SummaryItem[] = [
    nodes
      ? { label: "Nodes", value: `${nodes.ready} / ${nodes.total} ready`, note: nodesNote ?? "", old: isOld(nodes.nodesCollectedAt, now, NODES_OLD_AFTER_MS) }
      : { label: "Nodes", value: NOT_READ, note: "Counted by the controller's node pass", old: false },
    violations
      ? { label: "Policy violations", value: String(violations.total), note: collected(violations.policyCollectedAt, now, POLICY_OLD_AFTER_MS), old: isOld(violations.policyCollectedAt, now, POLICY_OLD_AFTER_MS), breakdown: largestFirst(violations.bySeverity) }
      : { label: "Policy violations", value: NOT_READ, note: "Counted every 15 minutes", old: false },
    events
      ? { label: "Policy events (1 h)", value: String(events.total), note: collected(events.policyCollectedAt, now, POLICY_OLD_AFTER_MS), old: isOld(events.policyCollectedAt, now, POLICY_OLD_AFTER_MS), breakdown: largestFirst(events.byReason) }
      : { label: "Policy events (1 h)", value: NOT_READ, note: "Admission and policy events of the last hour", old: false },
  ];
  const reachability = reachabilityView(status, now);
  if (!unreachable) return { state, items, reachability };
  const reason = reachable?.reason || "unreachable";
  const since = reachable?.lastSuccessfulAt ? `, ${new Date(millis(reachable.lastSuccessfulAt)!).toISOString()}` : "";
  return {
    state,
    items,
    reachability,
    notice: `The workload cluster cannot be reached right now (${reason}). Showing the counts from the last successful controller pass${since}.`,
  };
}

// ResourceShare is requested against allocatable, as text and a percentage
// for a meter.
export type ResourceShare = { requested: string; allocatable: string; percent: number; text: string };

export function formatCpu(millis: bigint): string {
  if (millis < 1000n) return `${millis}m`;
  return (Number(millis) / 1000).toFixed(1).replace(/\.0$/, "");
}

export function formatMemory(bytes: bigint): string {
  const gib = Number(bytes) / 2 ** 30;
  if (gib >= 1024) return `${(gib / 1024).toFixed(1)} TiB`;
  if (gib >= 1) return `${gib.toFixed(1)} GiB`;
  return `${Math.round(Number(bytes) / 2 ** 20)} MiB`;
}

const formatCount = (n: bigint) => String(n);

// share is undefined while the allocatable side is not reported: a zero
// allocatable is unknown, not a full group.
export function share(requested: bigint, allocatable: bigint, format: (n: bigint) => string, unit = ""): ResourceShare | undefined {
  if (allocatable <= 0n) return undefined;
  const percent = Number((requested * 100n) / allocatable);
  const of = `${format(allocatable)}${unit}`;
  return { requested: format(requested), allocatable: of, percent, text: `${format(requested)} / ${of} (${percent}%)` };
}

// utilizationTone is the meter colour of a requested share.
export function utilizationTone(percent: number): "accent" | "warning" | "error" {
  if (percent >= UTILIZATION_ERROR_PERCENT) return "error";
  if (percent >= UTILIZATION_WARNING_PERCENT) return "warning";
  return "accent";
}

type GroupCounts = Pick<NodeGroupStatus, "readyNodes" | "notReadyNodes" | "allocatableCpuMillis" | "allocatableMemoryBytes" | "requestedCpuMillis" | "requestedMemoryBytes" | "podCount">;

// isGroupRead is whether the node pass has read the group: until then all its
// totals are zero and mean "unknown".
export function isGroupRead(group: GroupCounts): boolean {
  return !!(group.allocatableCpuMillis || group.allocatableMemoryBytes || group.readyNodes || group.notReadyNodes || group.podCount);
}

// nodeGroupTotals is a group's requested and allocatable resources, as the
// overview prints them; undefined before the node pass read the group.
export function nodeGroupTotals(group: GroupCounts): { cpu: string; memory: string; pods: string } | undefined {
  if (!isGroupRead(group)) return undefined;
  return {
    cpu: share(group.requestedCpuMillis, group.allocatableCpuMillis, formatCpu, " cores")?.text ?? "Not reported",
    memory: share(group.requestedMemoryBytes, group.allocatableMemoryBytes, formatMemory)?.text ?? "Not reported",
    pods: String(group.podCount),
  };
}

// CapacityView is the cluster-wide totals of the node groups the node pass
// has read.
export type CapacityView = {
  groups: number;
  readGroups: number;
  platformGroups: number;
  cpu?: ResourceShare;
  memory?: ResourceShare;
  pods?: ResourceShare;
  podCount: number;
  podsWithoutRequests: number;
  old: boolean;
};

type Sums = { cpu: bigint; cpuAllocatable: bigint; memory: bigint; memoryAllocatable: bigint; podsAllocatable: bigint; pods: number; withoutRequests: number };
const emptySums: Sums = { cpu: 0n, cpuAllocatable: 0n, memory: 0n, memoryAllocatable: 0n, podsAllocatable: 0n, pods: 0, withoutRequests: 0 };

function addGroup(sums: Sums, group: NodeGroupStatus): Sums {
  return {
    cpu: sums.cpu + group.requestedCpuMillis,
    cpuAllocatable: sums.cpuAllocatable + group.allocatableCpuMillis,
    memory: sums.memory + group.requestedMemoryBytes,
    memoryAllocatable: sums.memoryAllocatable + group.allocatableMemoryBytes,
    podsAllocatable: sums.podsAllocatable + group.allocatablePods,
    pods: sums.pods + group.podCount,
    withoutRequests: sums.withoutRequests + group.podsWithoutRequests,
  };
}

// clusterCapacity sums the read node groups; undefined while none is read,
// so an unread cluster never shows zero capacity.
export function clusterCapacity(status: ClusterStatus | undefined, now: number): CapacityView | undefined {
  const groups = status?.nodeGroups ?? [];
  const read = groups.filter(isGroupRead);
  if (!read.length) return undefined;
  const sums = read.reduce(addGroup, emptySums);
  return {
    groups: groups.length,
    readGroups: read.length,
    platformGroups: groups.filter(group => group.platformManaged).length,
    cpu: share(sums.cpu, sums.cpuAllocatable, formatCpu, " cores"),
    memory: share(sums.memory, sums.memoryAllocatable, formatMemory),
    pods: share(BigInt(sums.pods), sums.podsAllocatable, formatCount),
    podCount: sums.pods,
    podsWithoutRequests: sums.withoutRequests,
    old: isOld(status?.nodes?.nodesCollectedAt, now, NODES_OLD_AFTER_MS),
  };
}

export interface NodeGroupSummaryRow extends Record<string, unknown> {
  name: string;
  platformManaged: boolean;
  phaseLabel: string;
  tone: NodeGroupTone;
  message: string;
  read: boolean;
  // "2 / 3 ready" once read; the observed node count before.
  nodes: string;
  notReady: number;
  cpu?: ResourceShare;
  memory?: ResourceShare;
  pods?: ResourceShare;
  podCount?: number;
  podsWithoutRequests?: number;
}

// nodeGroupSummaryRows is one row per observed node group, platform groups
// first; resource columns stay empty until the node pass read the group.
export function nodeGroupSummaryRows(groups: readonly NodeGroupStatus[]): NodeGroupSummaryRow[] {
  return [...groups]
    .sort((a, b) => Number(b.platformManaged) - Number(a.platformManaged) || a.name.localeCompare(b.name))
    .map(group => {
      const [phaseLabel, tone] = phaseDisplay(group.phase);
      const read = isGroupRead(group);
      const base = { name: group.name, platformManaged: group.platformManaged, phaseLabel, tone, message: group.message, read };
      if (!read) return { ...base, nodes: group.nodes ? `${group.nodes} nodes` : NOT_READ, notReady: 0 };
      return {
        ...base,
        nodes: `${group.readyNodes} / ${group.readyNodes + group.notReadyNodes} ready`,
        notReady: group.notReadyNodes,
        cpu: share(group.requestedCpuMillis, group.allocatableCpuMillis, formatCpu, " cores"),
        memory: share(group.requestedMemoryBytes, group.allocatableMemoryBytes, formatMemory),
        pods: share(BigInt(group.podCount), group.allocatablePods, formatCount),
        podCount: group.podCount,
        podsWithoutRequests: group.podsWithoutRequests,
      };
    });
}

/** The control plane's CloudWatch log group, and its console page when that is an https URL. Undefined when logging is off. */
export function controlPlaneLogs(aws: { controlPlaneLogGroup: string; controlPlaneLogsUrl: string } | undefined): { group: string; url?: string } | undefined {
  if (!aws?.controlPlaneLogGroup) return undefined;
  let url: string | undefined;
  try { url = new URL(aws.controlPlaneLogsUrl).protocol === "https:" ? aws.controlPlaneLogsUrl : undefined; } catch { url = undefined; }
  return { group: aws.controlPlaneLogGroup, url };
}
