import { Code, ConnectError } from "@connectrpc/connect";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { LiveReadErrorSchema } from "../../gen/cloud/v1/live_pb";
import type { GetNodeResourcesResponse, NodeResourceTotals } from "../../gen/cloud/v1/node_resources_pb";
import { LiveUnavailableDetailSchema } from "../../gen/cloud/v1/sort_pb";
import { cpuShare, memoryShare, podShare, type Share } from "./node-group-nodes";

/** A node group's live totals: requested of allocatable, and what the nodes use now when metrics are readable. */
export interface NodeTotals {
  nodes: number; ready: number; notReady: number;
  cpu?: Share; memory?: Share; pods?: Share;
  podCount: number; podsWithoutRequests: number;
  usedCpu?: Share; usedMemory?: Share;
}

export type NodeTotalsState =
  | { kind: "ready"; totals: NodeTotals; liveAt: string; complete: boolean; notices: string[] }
  /** The live read answered but lists no such group: it has no nodes right now. */
  | { kind: "empty"; liveAt: string }
  /** No live reader, or it could not read the cluster: why, for the page. */
  | { kind: "unavailable"; message: string };

export const LIVE_READER_OFF = "Live totals need the platform's live reader, which this deployment does not run. The requests below are from the controller's last pass.";

function utc(response: GetNodeResourcesResponse): string {
  return response.liveAt ? timestampDate(response.liveAt).toISOString().replace("T", " ").replace(/\.\d+Z$/, "").replace("Z", "") + " UTC" : "Time not reported";
}

export function nodeTotals(group: NodeResourceTotals): NodeTotals {
  const podCount = Number(group.podCount);
  return {
    nodes: group.nodes, ready: group.readyNodes, notReady: Math.max(0, group.nodes - group.readyNodes),
    cpu: cpuShare(group.requestedCpuMillis, group.allocatableCpuMillis),
    memory: memoryShare(group.requestedMemoryBytes, group.allocatableMemoryBytes),
    pods: podShare(podCount, group.allocatablePods), podCount, podsWithoutRequests: Number(group.podsWithoutRequests),
    usedCpu: group.usedCpuMillis === undefined ? undefined : cpuShare(group.usedCpuMillis, group.allocatableCpuMillis),
    usedMemory: group.usedMemoryBytes === undefined ? undefined : memoryShare(group.usedMemoryBytes, group.allocatableMemoryBytes),
  };
}

/** The group's entry of a GetNodeResources answer. */
export function nodeTotalsOf(response: GetNodeResourcesResponse, group: string): NodeTotalsState {
  const liveAt = utc(response);
  const entry = response.groups.find((item) => item.nodeGroup === group);
  if (!entry) return { kind: "empty", liveAt };
  return { kind: "ready", totals: nodeTotals(entry), liveAt, complete: response.complete, notices: [...response.notices] };
}

/** Why the live totals could not be read; undefined for an error the page reports as it is. */
export function nodeTotalsFailure(error: unknown): string | undefined {
  if (!(error instanceof ConnectError)) return undefined;
  if (error.code === Code.Unimplemented) return LIVE_READER_OFF;
  const unavailable = error.findDetails(LiveUnavailableDetailSchema)[0];
  if (unavailable) return `Live totals are unavailable right now${unavailable.message ? `: ${unavailable.message}` : ""}.`;
  const live = error.findDetails(LiveReadErrorSchema)[0];
  if (!live) return undefined;
  if (live.reason === "AUTHENTICATION_REQUIRED") return "Live totals need authentication: the API runs without it.";
  if (live.reason === "LIVE_READER_UNAVAILABLE") return "The platform's live reader cannot be reached right now, so live totals are unavailable.";
  return `The workload cluster cannot be read live right now (${live.message || live.reason}).`;
}
