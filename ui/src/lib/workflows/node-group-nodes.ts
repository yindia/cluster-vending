import { Code, ConnectError } from "@connectrpc/connect";
import type { Timestamp } from "@bufbuild/protobuf/wkt";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import type { Cluster, ClusterStatus } from "../../gen/cloud/v1/cluster_pb";
import { LiveReadErrorSchema } from "../../gen/cloud/v1/live_pb";
import { NodeGroupPhase, type NodeGroupNode, type NodeGroupStatus } from "../../gen/cloud/v1/node_group_pb";
import { NodeStatusFilter, type ListNodeGroupNodesResponse } from "../../gen/cloud/v1/node_group_nodes_pb";
import { PLATFORM_POOL, SYSTEM_GROUP, phaseDisplay, type NodeGroupTone } from "./node-groups";

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

/** Requested of allocatable, as a whole percentage and "1.25 of 1.93 cores"; undefined when nothing is allocatable. */
export interface Share { requested: number; allocatable: number; percent: number; label: string; short: string }

const trim = (n: number, digits: number) => String(Number(n.toFixed(digits)));
export const cores = (millis: bigint | number) => trim(Number(millis) / 1000, 2);
export const gibibytes = (bytes: bigint | number) => trim(Number(bytes) / 2 ** 30, 1);

function shareOf(requested: bigint | number, allocatable: bigint | number, text: (n: number) => string, unit: string): Share | undefined {
  const req = Number(requested), alloc = Number(allocatable);
  if (!(alloc > 0)) return undefined;
  return { requested: req, allocatable: alloc, percent: Math.round((req * 100) / alloc), label: `${text(req)} of ${text(alloc)} ${unit}`, short: `${text(req)} / ${text(alloc)} ${unit}` };
}
export const cpuShare = (requested: bigint | number, allocatable: bigint | number) => shareOf(requested, allocatable, cores, "cores");
export const memoryShare = (requested: bigint | number, allocatable: bigint | number) => shareOf(requested, allocatable, gibibytes, "GiB");
export const podShare = (count: number, allocatable: bigint | number) => shareOf(count, allocatable, n => n.toLocaleString("en-US"), "pods");

export interface NodeRow extends Record<string, unknown> {
  id: string; name: string; ready: boolean; statusLabel: string; tone: NodeGroupTone; detail: string;
  instanceType: string; zone: string; capacity: string; architecture: string; kubeletVersion: string; age: string;
  nodeClaim: string; nodeClaimTone: NodeGroupTone; nodeClaimDetail: string; providerId: string;
  /** "65%" of allocatable CPU requested, and "1.25 of 1.93 cores" (with live usage when the metrics API serves it). */
  cpu: string; cpuDetail: string; memory: string; memoryDetail: string;
  /** "16 of 17" pods, and how many of them request nothing. */
  pods: string; podsDetail: string;
}

const percentLabel = (share: Share | undefined) => share ? `${share.percent}%` : missing;

const count = (n: number, noun: string) => `${n} ${noun}${n === 1 ? "" : "s"}`;

export function nodeRow(node: NodeGroupNode, now: Date): NodeRow {
  const detail = node.ready
    ? [node.unschedulable ? "Cordoned" : "", node.taintsCount ? count(node.taintsCount, "taint") : ""].filter(Boolean).join(" · ")
    : node.conditions.join(", ");
  const claimTone: NodeGroupTone = !node.nodeclaimName ? "neutral" : node.nodeclaimReady ? "success" : "warning";
  const cpu = cpuShare(node.requestedCpuMillis, node.allocatableCpuMillis);
  const memory = memoryShare(node.requestedMemoryBytes, node.allocatableMemoryBytes);
  return {
    id: node.name, name: node.name, ready: node.ready, statusLabel: node.ready ? "Ready" : "Not ready", tone: node.ready ? "success" : "error", detail,
    instanceType: node.instanceType || missing, zone: node.zone || missing, capacity: capacityLabel(node.capacityType),
    kubeletVersion: node.kubeletVersion || missing, age: nodeAge(node.createdAt, now),
    architecture: node.architecture,
    nodeClaim: node.nodeclaimName || missing, nodeClaimTone: claimTone, nodeClaimDetail: node.nodeclaimName ? node.nodeclaimPhase : "", providerId: node.providerId,
    cpu: percentLabel(cpu), cpuDetail: usage(cpu?.short, node.usedCpuMillis, value => `${cores(value)} used`),
    memory: percentLabel(memory), memoryDetail: usage(memory?.short, node.usedMemoryBytes, value => `${gibibytes(value)} GiB used`),
    pods: node.allocatablePods ? `${node.podCount} of ${node.allocatablePods}` : node.podCount ? String(node.podCount) : missing,
    podsDetail: node.podsWithoutRequests ? `${node.podsWithoutRequests} without requests` : "",
  };
}

function usage(label: string | undefined, used: bigint | undefined, text: (value: bigint) => string): string {
  return [label ?? "", used === undefined ? "" : text(used)].filter(Boolean).join(" · ");
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

/** The server refuses a token of a list that changed (a new collection or live view, filter, sort or search): start again. */
export function isTokenMismatch(error: unknown): boolean {
  return error instanceof ConnectError && error.code === Code.InvalidArgument && /page_token|restart the list/.test(error.rawMessage);
}

/**
 * Why a live read failed (PROP-02 5.9), as the notice the node page shows instead of rows: the reason and the counts
 * of the cluster status summary from the last successful controller pass. Undefined for any other error.
 */
export function liveReadFailure(error: unknown, status: ClusterStatus | undefined): string | undefined {
  if (!(error instanceof ConnectError)) return undefined;
  const detail = error.findDetails(LiveReadErrorSchema)[0];
  if (!detail) return undefined;
  const nodes = status?.nodes;
  const since = nodes?.nodesCollectedAt ? `, ${utc(nodes.nodesCollectedAt)}` : "";
  const counts = nodes ? ` Nodes: ${nodes.ready} of ${nodes.total} ready.` : " No controller pass has read the cluster yet.";
  const lead = detail.reason === "AUTHENTICATION_REQUIRED"
    ? "Live reads need authentication."
    : `The workload cluster cannot be reached right now (${detail.message || detail.reason}).`;
  return `${lead} Showing the counts from the last successful controller pass${since}.${counts}`;
}

export interface CollectionState { collected: boolean; collectedLabel: string; incomplete: boolean; notices: string[]; live: boolean }

function utc(time: Timestamp): string {
  return timestampDate(time).toISOString().replace("T", " ").replace(/\.\d+Z$/, "").replace("Z", "") + " UTC";
}

/** What the response says of the collection; "incomplete" only means a collection that stopped early. */
export function collectionState(response: ListNodeGroupNodesResponse): CollectionState {
  if (response.liveAt) {
    // Read live by the live reader: its time, never a collection's.
    return { collected: true, live: true, collectedLabel: utc(response.liveAt), incomplete: !response.complete, notices: [...response.notices] };
  }
  const collected = !!response.collectedAt;
  return { collected, live: false, collectedLabel: response.collectedAt ? utc(response.collectedAt) : "Not collected yet", incomplete: collected && !response.complete, notices: [...response.notices] };
}

export type NodeGroupKind = "system" | "pool" | "other";
export interface GroupDetail {
  name: string; kind: NodeGroupKind; platformManaged: boolean;
  phase: NodeGroupPhase; phaseLabel: string; tone: NodeGroupTone; message: string; nodes?: number;
  /** What the group is for, a sentence under the header; empty for a group the platform does not explain. */
  about: string;
  /** The controller's report of the group, when it has made one. */
  observed?: NodeGroupStatus;
  /** A notice from the controller about why the node list is unavailable. */
  nodesNotice: string;
}

/** The system group's purpose: the platform manages it, so the page explains it once instead of offering settings. */
export const SYSTEM_ABOUT = "The default managed node group, with one static size the platform administrator sets: nothing autoscales it. It runs Karpenter and the EKS add-ons, is created from the platform's configuration, and cannot be changed.";
/** The platform pool's purpose. */
export const PLATFORM_POOL_ABOUT = "The Karpenter NodePool the platform creates from its configuration. Karpenter scales it with demand; it runs the platform's components and your workloads, and cannot be changed.";

/**
 * What the detail page shows of a group: its status as the controller reports it (or Not reported before it does).
 * "system", the default managed node group, exists on every cluster the platform manages, so it resolves before the
 * controller reports it; any other name must be in the status. Node groups are read-only: there are no settings to show.
 */
export function groupDetail(cluster: Cluster, name: string): GroupDetail | undefined {
  const observed: NodeGroupStatus | undefined = cluster.status?.nodeGroups.find(group => group.name === name);
  const kind: NodeGroupKind = name === SYSTEM_GROUP ? "system" : name === PLATFORM_POOL ? "pool" : "other";
  if (kind !== "system" && !observed) return undefined;
  const phase = observed?.phase ?? NodeGroupPhase.UNSPECIFIED;
  const [phaseLabel, tone] = phaseDisplay(phase);
  return {
    name, kind, platformManaged: kind === "system" || !!observed?.platformManaged, phase, phaseLabel, tone, message: observed?.message ?? "", nodes: observed?.nodes, nodesNotice: observed?.nodesNotice ?? "",
    about: kind === "system" ? SYSTEM_ABOUT : kind === "pool" ? PLATFORM_POOL_ABOUT : "", observed,
  };
}

/**
 * How many nodes the group has, shown once: the list's own total when it is unfiltered and collected (live or stored),
 * else the controller's count, else undefined (not reported yet).
 */
export function nodeCount(listTotal: number | undefined, collected: boolean, filtered: boolean, reported: number | undefined): number | undefined {
  if (listTotal !== undefined && collected && !filtered) return listTotal;
  return reported;
}

export interface GroupResources {
  /** "live": summed from the whole node list on screen; "reported": the controller's last pass. */
  source: "live" | "reported";
  ready: number; notReady: number;
  cpu?: Share; memory?: Share; pods?: Share; podCount: number; podsWithoutRequests: number;
}

/**
 * The group's requested and allocatable resources. The nodes are summed when the caller has the whole group (an
 * unfiltered, complete, single page), since that read is newer; otherwise the controller's report, if it has one.
 */
export function groupResources(observed: NodeGroupStatus | undefined, wholeGroup: readonly NodeGroupNode[] | undefined): GroupResources | undefined {
  if (wholeGroup?.length) {
    const sum = (pick: (node: NodeGroupNode) => bigint | number) => wholeGroup.reduce((total, node) => total + BigInt(pick(node)), 0n);
    const ready = wholeGroup.filter(node => node.ready).length;
    const podCount = Number(sum(node => node.podCount));
    return {
      source: "live", ready, notReady: wholeGroup.length - ready,
      cpu: cpuShare(sum(node => node.requestedCpuMillis), sum(node => node.allocatableCpuMillis)),
      memory: memoryShare(sum(node => node.requestedMemoryBytes), sum(node => node.allocatableMemoryBytes)),
      pods: podShare(podCount, sum(node => node.allocatablePods)), podCount, podsWithoutRequests: Number(sum(node => node.podsWithoutRequests)),
    };
  }
  if (!observed) return undefined;
  const reported = observed.allocatableCpuMillis || observed.allocatableMemoryBytes || observed.readyNodes || observed.notReadyNodes || observed.podCount;
  if (!reported) return undefined;
  return {
    source: "reported", ready: observed.readyNodes, notReady: observed.notReadyNodes,
    cpu: cpuShare(observed.requestedCpuMillis, observed.allocatableCpuMillis),
    memory: memoryShare(observed.requestedMemoryBytes, observed.allocatableMemoryBytes),
    pods: podShare(observed.podCount, observed.allocatablePods), podCount: observed.podCount, podsWithoutRequests: observed.podsWithoutRequests,
  };
}

/** A usage notice (no metrics API) is informational: requests still show; any other notice is a warning. */
export const isInfoNotice = (notice: string) => notice.startsWith("node usage unavailable");
