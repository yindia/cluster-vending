import { Code, ConnectError } from "@connectrpc/connect";
import type { Timestamp } from "@bufbuild/protobuf/wkt";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import type { Cluster } from "../../gen/cloud/v1/cluster_pb";
import { NodeGroupPhase, type NodeGroupNode, type NodeGroupSpec, type NodeGroupStatus } from "../../gen/cloud/v1/node_group_pb";
import { NodeStatusFilter, type ListNodeGroupNodesResponse } from "../../gen/cloud/v1/node_group_nodes_pb";
import { phaseDisplay, summarizeNodeGroup, type NodeGroupTone } from "./node-groups";

/** The node table is paged by the server: a page is 100 nodes, never the whole group. */
export const NODE_PAGE_SIZE = 100;

export type NodeStatusChoice = "all" | "ready" | "not-ready";
export const nodeFilterOptions: readonly { value: NodeStatusChoice; label: string }[] = [
  { value: "all", label: "All" }, { value: "ready", label: "Ready" }, { value: "not-ready", label: "Not ready" },
];

export function nodeStatusFilter(choice: string): NodeStatusFilter {
  return choice === "ready" ? NodeStatusFilter.READY : choice === "not-ready" ? NodeStatusFilter.NOT_READY : NodeStatusFilter.ALL;
}

const missing = "—";

/** The largest whole unit: minutes under an hour, hours under two days, then days. */
export function nodeAge(created: Timestamp | undefined, now: Date): string {
  if (!created) return missing;
  const minutes = Math.max(0, Math.floor((now.getTime() - timestampDate(created).getTime()) / 60_000));
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  return hours < 48 ? `${hours}h` : `${Math.floor(hours / 24)}d`;
}

export function capacityLabel(value: string): string {
  return value === "spot" ? "Spot" : value === "on-demand" ? "On-demand" : value || missing;
}

export interface NodeRow extends Record<string, unknown> {
  id: string; name: string; ready: boolean; statusLabel: string; tone: NodeGroupTone; detail: string;
  instanceType: string; zone: string; capacity: string; kubeletVersion: string; age: string;
  nodeClaim: string; nodeClaimTone: NodeGroupTone; nodeClaimDetail: string; providerId: string;
}

const count = (n: number, noun: string) => `${n} ${noun}${n === 1 ? "" : "s"}`;

export function nodeRow(node: NodeGroupNode, now: Date): NodeRow {
  const detail = node.ready
    ? [node.unschedulable ? "Cordoned" : "", node.taintsCount ? count(node.taintsCount, "taint") : ""].filter(Boolean).join(" · ")
    : node.conditions.join(", ");
  const claimTone: NodeGroupTone = !node.nodeclaimName ? "neutral" : node.nodeclaimReady ? "success" : "warning";
  return {
    id: node.name, name: node.name, ready: node.ready, statusLabel: node.ready ? "Ready" : "Not ready", tone: node.ready ? "success" : "error", detail,
    instanceType: node.instanceType || missing, zone: node.zone || missing, capacity: capacityLabel(node.capacityType),
    kubeletVersion: node.kubeletVersion || missing, age: nodeAge(node.createdAt, now),
    nodeClaim: node.nodeclaimName || missing, nodeClaimTone: claimTone, nodeClaimDetail: node.nodeclaimName ? node.nodeclaimPhase : "", providerId: node.providerId,
  };
}

/** Forward-only server tokens: the pager keeps those of the pages passed, so Previous is a pop. */
export interface Pager { tokens: readonly string[]; page: number; token: string; hasPrevious: boolean }
const pagerOf = (tokens: readonly string[]): Pager => ({ tokens, page: tokens.length, token: tokens[tokens.length - 1], hasPrevious: tokens.length > 1 });
export const pagerStart = (): Pager => pagerOf([""]);
export function pagerForward(pager: Pager, nextToken: string): Pager { return nextToken ? pagerOf([...pager.tokens, nextToken]) : pager; }
export function pagerBack(pager: Pager): Pager { return pager.hasPrevious ? pagerOf(pager.tokens.slice(0, -1)) : pager; }

/** "1–100 of 1,000": the page's first and last row of the matching total. */
export function pageRange(page: number, pageSize: number, shown: number, total: number): string {
  if (!total || !shown) return "0 of 0";
  const first = (page - 1) * pageSize + 1;
  return `${first.toLocaleString("en-US")}–${(first + shown - 1).toLocaleString("en-US")} of ${total.toLocaleString("en-US")}`;
}

/** The server refuses a token of a list that changed (a new collection, filter or search): start again. */
export function isTokenMismatch(error: unknown): boolean {
  return error instanceof ConnectError && error.code === Code.InvalidArgument && /page_token/.test(error.rawMessage);
}

export interface CollectionState { collected: boolean; collectedLabel: string; incomplete: boolean; notices: string[] }

function utc(time: Timestamp): string {
  return timestampDate(time).toISOString().replace("T", " ").replace(/\.\d+Z$/, "").replace("Z", "") + " UTC";
}

/** What the response says of the collection; "incomplete" only means a collection that stopped early. */
export function collectionState(response: ListNodeGroupNodesResponse): CollectionState {
  const collected = !!response.collectedAt;
  return { collected, collectedLabel: response.collectedAt ? utc(response.collectedAt) : "Not collected yet", incomplete: collected && !response.complete, notices: [...response.notices] };
}

export type NodeGroupKind = "system" | "user";
export interface GroupDetail {
  name: string; kind: NodeGroupKind; platformManaged: boolean;
  phase: NodeGroupPhase; phaseLabel: string; tone: NodeGroupTone; message: string; nodes?: number;
  /** A notice from the controller about why the node list is unavailable. */
  nodesNotice: string;
  summary: { label: string; value: string }[];
}

/** The default managed node group's range, which cluster-autoscaler moves it in: "2–6 (autoscaled)". */
function sizeLabel(min: number, max: number): string {
  return `${min}–${max} (autoscaled)`;
}

function userSummary(group: NodeGroupSpec) {
  const s = summarizeNodeGroup(group);
  return [{ label: "Capacity", value: s.capacity }, { label: "Instances", value: s.instances }, { label: "Architectures", value: s.architectures },
    { label: "Labels", value: s.labels }, { label: "Taints", value: s.taints }, { label: "Limits", value: s.limits }];
}

/**
 * What the detail page shows of a group: its status as the controller reports it (or Pending/Not reported before it
 * does) and the group's own settings. "system", the default managed node group that runs the platform, exists on
 * every cluster the platform manages, so it resolves before the controller reports it; any other name must be in the
 * spec or the status.
 */
export function groupDetail(cluster: Cluster, name: string): GroupDetail | undefined {
  const observed: NodeGroupStatus | undefined = cluster.status?.nodeGroups.find(group => group.name === name);
  const spec = cluster.spec?.nodeGroups.find(group => group.name === name);
  const kind: NodeGroupKind = name === "system" ? "system" : "user";
  if (kind === "user" && !spec && !observed) return undefined;
  const phase = observed?.phase ?? (kind === "system" ? NodeGroupPhase.UNSPECIFIED : NodeGroupPhase.PENDING);
  const [phaseLabel, tone] = phaseDisplay(phase);
  let summary: GroupDetail["summary"];
  if (kind === "system") {
    const pool = cluster.spec?.defaultNodePool;
    summary = [{ label: "Managed by", value: "The platform: the default managed node group, autoscaled by the cluster autoscaler between its minimum and maximum size. It runs Karpenter, CoreDNS, the cluster autoscaler, every platform component, tenant components and add-on controllers, and cannot be changed." }];
    if (pool?.instanceTypes.length) summary.push({ label: "Instance types", value: pool.instanceTypes.join(", ") });
    if (pool?.scalingConfig) summary.push({ label: "Size", value: sizeLabel(pool.scalingConfig.minSize, pool.scalingConfig.maxSize) });
    if (observed) summary.push({ label: "Nodes now", value: count(observed.nodes, "node") });
  } else summary = spec ? userSummary(spec) : [];
  return {
    name, kind, platformManaged: kind === "system" || !!observed?.platformManaged, phase, phaseLabel, tone, message: observed?.message ?? "", nodes: observed?.nodes, nodesNotice: observed?.nodesNotice ?? "", summary,
  };
}
