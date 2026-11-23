import type { Timestamp } from "@bufbuild/protobuf/wkt";
import type { ClusterCount } from "../../gen/cloud/v1/cluster_summary_pb";
import type { ClusterStatus } from "../../gen/cloud/v1/cluster_pb";
import { ConditionStatus } from "../../gen/cloud/v1/conditions_pb";

// The ages after which the cluster status summary is "old" (PROP-02 5.2):
// the node counts after 15 minutes, the policy counts after 30.
export const NODES_OLD_AFTER_MS = 15 * 60_000;
export const POLICY_OLD_AFTER_MS = 30 * 60_000;

export type SummaryState = "not-read" | "read" | "unreachable";

export type SummaryItem = { label: string; value: string; note: string; old: boolean };

export type ClusterSummaryView = {
  state: SummaryState;
  // Shown as a banner when the platform cannot read the cluster.
  notice?: string;
  items: SummaryItem[];
};

const NOT_READ = "Not read yet";

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

function counts(list: ClusterCount[]): string {
  return list.map(c => `${c.name} ${c.count}`).join(" · ");
}

function collected(t: Timestamp | undefined, now: number, after: number): string {
  return `Collected ${ageLabel(t, now)}${isOld(t, now, after) ? " · old" : ""}`;
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
  const items: SummaryItem[] = [
    nodes
      ? { label: "Nodes", value: `${nodes.ready} / ${nodes.total} ready`, note: collected(nodes.nodesCollectedAt, now, NODES_OLD_AFTER_MS), old: isOld(nodes.nodesCollectedAt, now, NODES_OLD_AFTER_MS) }
      : { label: "Nodes", value: NOT_READ, note: "Counted by the controller's node pass", old: false },
    violations
      ? { label: "Policy violations", value: String(violations.total), note: [counts(violations.bySeverity), collected(violations.policyCollectedAt, now, POLICY_OLD_AFTER_MS)].filter(Boolean).join(" · "), old: isOld(violations.policyCollectedAt, now, POLICY_OLD_AFTER_MS) }
      : { label: "Policy violations", value: NOT_READ, note: "Counted every 15 minutes", old: false },
    events
      ? { label: "Policy events (1 h)", value: String(events.total), note: [counts(events.byReason), collected(events.policyCollectedAt, now, POLICY_OLD_AFTER_MS)].filter(Boolean).join(" · "), old: isOld(events.policyCollectedAt, now, POLICY_OLD_AFTER_MS) }
      : { label: "Policy events (1 h)", value: NOT_READ, note: "Admission and policy events of the last hour", old: false },
  ];
  if (!unreachable) return { state, items };
  const reason = reachable?.reason || "unreachable";
  const since = reachable?.lastSuccessfulAt ? `, ${new Date(millis(reachable.lastSuccessfulAt)!).toISOString()}` : "";
  return {
    state,
    items,
    notice: `The workload cluster cannot be reached right now (${reason}). Showing the counts from the last successful controller pass${since}.`,
  };
}

// nodeGroupTotals is a group's requested and allocatable resources, as the
// overview prints them; undefined before the node pass read the group.
export function nodeGroupTotals(group: { readyNodes: number; notReadyNodes: number; allocatableCpuMillis: bigint; allocatableMemoryBytes: bigint; requestedCpuMillis: bigint; requestedMemoryBytes: bigint; podCount: number }): { cpu: string; memory: string; pods: string } | undefined {
  if (!group.allocatableCpuMillis && !group.allocatableMemoryBytes && !group.readyNodes && !group.notReadyNodes && !group.podCount) return undefined;
  const share = (requested: bigint, allocatable: bigint, text: (n: bigint) => string) =>
    allocatable > 0n ? `${text(requested)} / ${text(allocatable)} (${(requested * 100n) / allocatable}%)` : "Not reported";
  const cpu = (m: bigint) => (m % 1000n === 0n ? `${m / 1000n}` : `${m}m`);
  const memory = (b: bigint) => `${(Number(b) / 2 ** 30).toFixed(1)} GiB`;
  return {
    cpu: share(group.requestedCpuMillis, group.allocatableCpuMillis, cpu),
    memory: share(group.requestedMemoryBytes, group.allocatableMemoryBytes, memory),
    pods: String(group.podCount),
  };
}
