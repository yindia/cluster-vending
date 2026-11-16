import { readNetworkInfrastructure } from "./network-infrastructure";
import { buildArchitectureGraph } from "./network-architecture-graph";
import { layoutNetworkArchitecture } from "./network-architecture-layout";
import { toJsonString } from "@bufbuild/protobuf";
import { ClusterSchema, ClusterState, type Cluster } from "../../gen/cloud/v1/cluster_pb";
import { ConditionStatus, type Condition } from "../../gen/cloud/v1/conditions_pb";
import { ManagedResourceSchema, ManagedResourceState, type ManagedResource } from "../../gen/cloud/v1/managed_resource_pb";
import { NetworkState, type Network } from "../../gen/cloud/v1/network_pb";
import { CloudAccountState, type CloudAccount } from "../../gen/cloud/v1/tenant_service_pb";

export type TopologyNodeData = {
  title: string; kind: string; resourceId: string; cloudId: string; state: string;
  tone: "neutral" | "success" | "warning" | "error";
  paused: boolean; href: string; message: string;
  presentation?: "group" | "resource" | "external";
  boundary?: "account" | "region" | "vpc" | "zone" | "public" | "private" | "subnet" | "routing" | "shared";
  routes?: ArchitectureRoute[];
  graphId?: string; attachments?: TopologyNodeData[]; referenceOnly?: boolean;
  cidr?: string; zoneId?: string;
};
export type TopologyNode = { id: string; position: { x: number; y: number }; data: TopologyNodeData };
export type TopologyEdge = { id: string; source: string; target: string };
export type NetworkTopology = { nodes: TopologyNode[]; edges: TopologyEdge[]; kinds: string[]; total: number; matched: number };
export type NetworkTopologyOptions = { search?: string; kind?: string };

const compare = (a: string, b: string) => a < b ? -1 : a > b ? 1 : 0;
const id = (prefix: string, ...parts: string[]) => `${prefix}:${parts.map(part => encodeURIComponent(part)).join(":")}`;
const label = (name: string | undefined) => !name || name === "UNSPECIFIED" ? "Not reported" : name.toLowerCase().replaceAll("_", " ").replace(/^./, letter => letter.toUpperCase());
const conditionPaused = (conditions: readonly Condition[] = []) => conditions.some(condition => ["Frozen", "Paused"].includes(condition.type) && condition.status === ConditionStatus.TRUE);
const conditionMessage = (conditions: readonly Condition[] = []) => conditions.filter(condition => condition.status === ConditionStatus.FALSE || condition.type === "Frozen").map(condition => [condition.reason, condition.message].filter(Boolean).join(": ")).filter(Boolean).join("; ");

function displayState(state: string, ready: boolean, failed: boolean, stale: boolean, paused: boolean): Pick<TopologyNodeData, "state" | "tone"> {
  if ((ready || failed) && stale) return { state: "Reconciling", tone: "warning" };
  return { state, tone: failed ? "error" : ready && !paused ? "success" : state === "Not reported" && !paused ? "neutral" : "warning" };
}

function resourceNode(network: Network, resource: ManagedResource): TopologyNode | undefined {
  // Anonymous rows cannot be assigned a reliable object identity. Never invent
  // an ordinal ID or a VPC/subnet from desired layout or cloud ID summaries.
  if (!resource.name && !resource.externalName) return undefined;
  const key = resource.name ? [resource.kind, "name", resource.name] : [resource.kind, "external", resource.externalName];
  const stale = resource.generation > resource.observedGeneration;
  const reportedReady = resource.state === ManagedResourceState.READY;
  const confirmed = resource.ready === ConditionStatus.TRUE && resource.synced === ConditionStatus.TRUE;
  const state = reportedReady && !stale && !confirmed ? "Readiness unconfirmed" : label(ManagedResourceState[resource.state]);
  const status = displayState(state, reportedReady && confirmed, resource.state === ManagedResourceState.FAILED, stale, resource.paused);
  if (reportedReady && stale) Object.assign(status, { state: "Reconciling", tone: "warning" });
  return { id: id("managed-resource", network.id, JSON.stringify(key)), position: { x: 0, y: 0 }, data: {
    title: resource.name || resource.externalName, kind: resource.kind || "Managed resource", resourceId: resource.name,
    cloudId: resource.externalName, ...status, paused: resource.paused, href: "",
    message: stale && resource.state !== ManagedResourceState.DELETING ? "Waiting for observation of the current generation." : [resource.reason, resource.message].filter(Boolean).join(": "),
  } };
}

/** Provider-neutral ownership graph. Inventory inclusion proves network ownership;
 * it does not prove relationships between the managed resources themselves.
 * Counts exclude context nodes, anonymous rows and duplicate object identities. */
export function buildNetworkTopology(network: Network | undefined, account?: CloudAccount, clusters: readonly Cluster[] = [], options: NetworkTopologyOptions = {}): NetworkTopology {
  const graph: NetworkTopology = { nodes: [], edges: [], kinds: [], total: 0, matched: 0 };
  if (!network?.id) return graph;
  const networkId = id("network", network.id);
  const networkPaused = !!network.status?.freeze && !network.status.freeze.letThrough || conditionPaused(network.status?.conditions);
  const ready = network.status?.state === NetworkState.READY;
  const networkNode: TopologyNode = { id: networkId, position: { x: 320, y: 0 }, data: {
    title: network.displayName || network.name || network.id, kind: "Network", resourceId: network.id,
    cloudId: network.status?.resources?.cloudNetworkId ?? "",
    ...displayState(label(NetworkState[network.status?.state ?? 0]), ready, network.status?.state === NetworkState.FAILED, (network.status?.observedGeneration ?? 0n) < network.generation, networkPaused),
    paused: networkPaused, href: `/networks/${encodeURIComponent(network.id)}`,
    message: network.status && network.status.observedGeneration < network.generation && network.status.state !== NetworkState.DELETING ? "Waiting for observation of the current generation." : network.status?.message || conditionMessage(network.status?.conditions),
  } };
  if (account?.id && network.tenantId && account.tenantId === network.tenantId && account.id === network.spec?.bootstrapId) {
    const paused = conditionPaused(account.status?.conditions);
    graph.nodes.push({ id: id("account", account.id), position: { x: 0, y: 0 }, data: {
      title: account.accountName || account.id, kind: "Account", resourceId: account.id,
      // Use the explicit cloud account identifier, never an assumed-role ARN.
      cloudId: account.provider.case === "aws" ? account.provider.value.accountId : "",
      ...displayState(label(CloudAccountState[account.status?.state ?? 0]), account.status?.state === CloudAccountState.READY, account.status?.state === CloudAccountState.FAILED, false, paused),
      paused, href: `/accounts/${encodeURIComponent(account.id)}`, message: conditionMessage(account.status?.conditions),
    } });
    graph.edges.push({ id: id("edge", graph.nodes[0].id, networkId), source: graph.nodes[0].id, target: networkId });
  }
  graph.nodes.push(networkNode);
  const contextCount = graph.nodes.length;

  const resources = new Map<string, { node: TopologyNode; source: ManagedResource }>();
  const resourceRank = (resource: ManagedResource) => JSON.stringify([Number(resource.state === ManagedResourceState.FAILED), Number(resource.paused), resource.state, resource.ready, resource.synced, toJsonString(ManagedResourceSchema, resource)]);
  for (const resource of network.status?.managedResources ?? []) {
    const node = resourceNode(network, resource);
    if (!node) continue;
    const previous = resources.get(node.id)?.source;
    if (!previous || resource.generation > previous.generation || resource.generation === previous.generation && (resource.observedGeneration > previous.observedGeneration || resource.observedGeneration === previous.observedGeneration && compare(resourceRank(resource), resourceRank(previous)) > 0)) resources.set(node.id, { node, source: resource });
  }
  const clusterRows = new Map<string, Cluster>();
  for (const cluster of clusters) {
    if (!cluster.id || !network.tenantId || cluster.tenantId !== network.tenantId || cluster.spec?.network?.networkId !== network.id) continue;
    const previous = clusterRows.get(cluster.id);
    if (!previous || cluster.generation > previous.generation || cluster.generation === previous.generation && ((cluster.status?.observedGeneration ?? 0n) > (previous.status?.observedGeneration ?? 0n) || (cluster.status?.observedGeneration ?? 0n) === (previous.status?.observedGeneration ?? 0n) && compare(toJsonString(ClusterSchema, cluster), toJsonString(ClusterSchema, previous)) > 0)) clusterRows.set(cluster.id, cluster);
  }
  const clusterNodes: TopologyNode[] = [...clusterRows.values()].map(cluster => {
    const paused = !!cluster.status?.freeze && !cluster.status.freeze.letThrough || conditionPaused(cluster.status?.conditions);
    return { id: id("cluster", cluster.id), position: { x: 0, y: 0 }, data: {
      title: cluster.displayName || cluster.name || cluster.id, kind: "Cluster", resourceId: cluster.id,
      cloudId: cluster.status?.managedResources.find(resource => resource.kind === "Cluster")?.externalName ?? "",
      ...displayState(label(ClusterState[cluster.status?.state ?? 0]), cluster.status?.state === ClusterState.RUNNING, cluster.status?.state === ClusterState.FAILED, (cluster.status?.observedGeneration ?? 0n) < cluster.generation, paused),
      paused, href: `/clusters/${encodeURIComponent(cluster.id)}`, message: (cluster.status?.observedGeneration ?? 0n) < cluster.generation && cluster.status?.state !== ClusterState.DELETING ? "Waiting for observation of the current generation." : cluster.status?.message || conditionMessage(cluster.status?.conditions),
    } };
  });
  const resourceNodes = [...resources.values()].map(({ node }) => node);
  const all = [...clusterNodes, ...resourceNodes];
  graph.kinds = [...new Set(all.map(node => node.data.kind))].sort(compare);
  graph.total = all.length;
  const terms = options.search?.trim().toLowerCase().split(/\s+/).filter(Boolean) ?? [];
  const matches = (node: TopologyNode) => (!options.kind || options.kind === "all" || node.data.kind.toLowerCase() === options.kind.toLowerCase()) && terms.every(term => `${node.data.title} ${node.data.kind} ${node.data.resourceId} ${node.data.cloudId} ${node.data.state} ${node.data.message}`.toLowerCase().includes(term));
  const order = (a: TopologyNode, b: TopologyNode) => compare(a.data.kind, b.data.kind) || compare(a.id, b.id);
  const visibleClusters = clusterNodes.filter(matches).sort(order);
  const visibleResources = resourceNodes.filter(matches).sort(order);
  // 256px cards fit 320px column spacing; 200px rows leave space for handles.
  // Clusters have their own branch above a three-column resource grid.
  const resourceStart = visibleClusters.length ? Math.ceil(visibleClusters.length / 3) * 200 + 80 : 0;
  for (const [items, start] of [[visibleClusters, 0], [visibleResources, resourceStart]] as const) {
    items.forEach((node, index) => {
      node.position = { x: 640 + index % 3 * 320, y: start + Math.floor(index / 3) * 200 };
      graph.nodes.push(node);
      graph.edges.push({ id: id("edge", networkId, node.id), source: networkId, target: node.id });
    });
  }
  graph.matched = visibleClusters.length + visibleResources.length;
  const center = Math.max(0, ...graph.nodes.slice(contextCount).map(node => node.position.y)) / 2;
  for (const node of graph.nodes.slice(0, contextCount)) node.position.y = center;
  return graph;
}

export type ArchitectureRoute = { id: string; destination: string; targetId: string; targetKind: string; targetName: string; evidence: "configured" | "observed"; message?: string };
export type ArchitectureNode = TopologyNode & { type: "group" | "resource" | "external"; parentId?: string; extent?: "parent"; width: number; height: number; headerHeight?: number; headerWidth?: number };
export type ArchitectureEdge = TopologyEdge & { label: string; data: { relation: string; evidence: "configured" | "observed" | "logical"; derived: boolean; relationship?: "contains" | "associated-with" | "routes-to" | "attached-to" | "uses-network"; routeIds?: string[]; crossZone?: boolean } };
export type ArchitectureRelationship = { id: string; source: string; target: string; type: "contains" | "associated-with" | "routes-to" | "attached-to" | "uses-network"; evidence: "configured" | "observed" | "logical" };
export type NetworkArchitectureGraph = { relationships?: ArchitectureRelationship[]; nodes: ArchitectureNode[]; edges: ArchitectureEdge[]; supportingResources: TopologyNodeData[]; unresolved: string[]; total: number; matched: number };
export type ArchitectureDimensions = ReadonlyMap<string, { width: number; height: number; headerHeight?: number; headerWidth?: number }>;

/** Discovery, semantic relationships and geometry have separate ownership. */
export function buildNetworkArchitecture(network: Network | undefined, account?: CloudAccount, clusters: readonly Cluster[] = [], dimensions?: ArchitectureDimensions): NetworkArchitectureGraph {
  const inventory = buildNetworkTopology(network, account, clusters);
  return layoutNetworkArchitecture(buildArchitectureGraph(readNetworkInfrastructure(network), inventory), dimensions);
}
export function buildNetworkArchitectureOverview(network: Network | undefined, account?: CloudAccount, clusters: readonly Cluster[] = [], dimensions?: ArchitectureDimensions, showLocalRoutes = false): NetworkArchitectureGraph {
  const graph = buildArchitectureGraph(readNetworkInfrastructure(network), buildNetworkTopology(network, account, clusters));
  if (!showLocalRoutes) {
    const local = new Set(graph.nodes.filter(n => n.data.kind === "Local route destination").map(n => n.id));
    graph.nodes = graph.nodes.filter(n => !local.has(n.id));
    graph.edges = graph.edges.filter(e => !local.has(e.target));
  }
  return layoutNetworkArchitecture(graph, dimensions);
}
