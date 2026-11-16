import { ConditionStatus } from "../../gen/cloud/v1/conditions_pb";
import type { Cluster } from "../../gen/cloud/v1/cluster_pb";
import { ManagedResourceState, type ManagedResource } from "../../gen/cloud/v1/managed_resource_pb";
import type { CloudAccount } from "../../gen/cloud/v1/tenant_service_pb";
import { layoutNetworkArchitecture } from "./network-architecture-layout";
import { buildNetworkArchitectureOverview } from "./network-topology";
import type { Network, NetworkArchitectureResource } from "../../gen/cloud/v1/network_pb";
import type { ArchitectureDimensions, ArchitectureEdge, ArchitectureNode, TopologyNodeData } from "./network-topology";

export type ClusterArchitectureGraph = {
  nodes: ArchitectureNode[];
  edges: ArchitectureEdge[];
  /** Visible resources; category/context nodes are excluded. */
  resources: ArchitectureNode[];
  /** Every validated fact, including resources hidden by presentation options. */
  inventoryNodes: ArchitectureNode[];
  /** Validated factual relationships, plus overlay relationships when enabled. */
  allEdges: ArchitectureEdge[];
  diagnostics: string[];
  available: boolean;
};
const key = (kind: string, name: string) => JSON.stringify([kind, name]);
const compare = (a: string, b: string) => a < b ? -1 : a > b ? 1 : 0;
export type ClusterArchitectureOptions = { showNetwork?: boolean; network?: Network; account?: CloudAccount };
const workerKinds = new Set(["Node", "Instance", "EC2Instance", "KarpenterNode", "WorkerNode"]);
const coreKinds = new Set(["Cluster", "NodeGroup"]);
const networkKinds = new Set(["Subnet", "SecurityGroup"]);
const visibleCompute = (node: ArchitectureNode) => coreKinds.has(node.data.kind) || workerKinds.has(node.data.kind) && !node.data.referenceOnly;
export function clusterRelationship(relation: string): string {
  return ({ has_node_group: "Has node group", has_addon: "Has add-on", has_component: "Has component", uses_subnet: "Uses subnet", uses_security_group: "Uses security group", uses_launch_template: "Uses launch template" } as Record<string, string>)[relation] ?? relation.replaceAll("_", " ");
}
export function requireClusterTopologyScope(cluster: Cluster | undefined, tenantId: string, clusterId: string): Cluster {
  if (!cluster || !tenantId || !clusterId || cluster.tenantId !== tenantId || cluster.id !== clusterId) throw new Error("The architecture response did not match this cluster and tenant.");
  return cluster;
}
function dataFor(fact: NetworkArchitectureResource, managed?: ManagedResource): TopologyNodeData {
  const stale = !!managed && managed.generation > managed.observedGeneration;
  const ready = managed?.state === ManagedResourceState.READY;
  const failed = managed?.state === ManagedResourceState.FAILED;
  const confirmed = managed?.ready === ConditionStatus.TRUE && managed?.synced === ConditionStatus.TRUE;
  const state = fact.referenceOnly ? "Referenced target" : stale && (ready || failed) ? "Reconciling" : ready && !confirmed ? "Readiness unconfirmed" : managed?.state ? ManagedResourceState[managed.state]?.toLowerCase().replaceAll("_", " ") ?? "Unknown" : "Not reported";
  const tone = fact.referenceOnly ? "neutral" : stale && (ready || failed) ? "warning" : failed ? "error" : ready && confirmed && !managed?.paused ? "success" : state === "Not reported" && !managed?.paused ? "neutral" : "warning";
  return { title: fact.name, kind: fact.kind, resourceId: fact.referenceOnly ? "" : fact.name, cloudId: fact.cloudId,
    state: state[0].toUpperCase() + state.slice(1), tone, paused: !fact.referenceOnly && !!managed?.paused, href: "",
    referenceOnly: fact.referenceOnly, presentation: "resource", cidr: fact.cidrBlock, zoneId: fact.zoneId,
    message: fact.referenceOnly ? "Explicitly referenced target; its availability, ownership and readiness have not been independently verified." : stale && managed?.state !== ManagedResourceState.DELETING ? "Waiting for observation of the current generation." : [managed?.reason, managed?.message].filter(Boolean).join(": ") || (managed ? "No provider issues reported." : "Reconciliation status is not reported for this resource.") };
}
/** Uses only the cluster architecture contract. Inventory and desired counts do
 * not establish relationships. Kind/name is the managed-object identity, even
 * when two objects have the same cloud ID. Visibility does not discard facts. */
export function buildClusterArchitecture(cluster?: Cluster, dimensions: ArchitectureDimensions = new Map(), options: ClusterArchitectureOptions = {}): ClusterArchitectureGraph {
  const graph: ClusterArchitectureGraph = { nodes: [], edges: [], resources: [], inventoryNodes: [], allEdges: [], diagnostics: [], available: !!cluster?.status?.architecture };
  if (!cluster?.id || !cluster.tenantId) return graph;
  const architecture = cluster.status?.architecture;
  if (!architecture) { graph.diagnostics.push("Architecture references have not been reported. Inventory alone does not establish cluster relationships."); return graph; }
  const facts = new Map<string, NetworkArchitectureResource>();
  for (const fact of [...architecture.resources].sort((a, b) => compare(JSON.stringify(a), JSON.stringify(b)))) {
    if (!fact.kind || !fact.name) { graph.diagnostics.push("A resource without a kind or name cannot be identified."); continue; }
    const identity = key(fact.kind, fact.name), previous = facts.get(identity);
    if (previous && JSON.stringify(previous) !== JSON.stringify(fact)) graph.diagnostics.push(`Conflicting resource facts for ${fact.kind} ${fact.name}.`);
    if (!previous || previous.referenceOnly && !fact.referenceOnly) facts.set(identity, fact);
  }
  const inventory = new Map((cluster.status?.managedResources ?? []).map(resource => [key(resource.kind, resource.name), resource]));
  const byKey = new Map<string, ArchitectureNode>();
  for (const [identity, fact] of [...facts].sort(([a], [b]) => compare(a, b))) {
    const id = JSON.stringify(["cluster-resource", cluster.tenantId, cluster.id, fact.kind, fact.name]);
    const measured = dimensions.get(id);
    const node: ArchitectureNode = { id, type: "resource", position: { x: 0, y: 0 }, width: Math.max(288, measured?.width ?? 0), height: Math.max(128, measured?.height ?? 0), data: dataFor(fact, inventory.get(identity)) };
    node.data.graphId = id;
    byKey.set(identity, node); graph.inventoryNodes.push(node);
  }
  const edges = new Map<string, ArchitectureEdge>();
  for (const connection of architecture.connections) {
    const source = byKey.get(key(connection.sourceKind, connection.sourceName)), target = byKey.get(key(connection.targetKind, connection.targetName));
    if (!source || !target) { graph.diagnostics.push(`Unavailable endpoint: ${connection.sourceKind} ${connection.sourceName} → ${connection.targetKind} ${connection.targetName}.`); continue; }
    if (source.id === target.id || !connection.relation || !["configured", "observed"].includes(connection.evidence)) { graph.diagnostics.push(`Unsupported relationship for ${connection.sourceName} → ${connection.targetName}.`); continue; }
    const id = JSON.stringify([source.id, target.id, connection.relation, connection.label]);
    const existing = edges.get(id);
    if (!existing || connection.evidence === "observed") edges.set(id, { id, source: source.id, target: target.id, label: connection.label,
      data: { relation: connection.relation, evidence: connection.evidence as "configured" | "observed", derived: false } });
  }
  graph.allEdges = [...edges.values()].sort((a, b) => compare(a.id, b.id));
  graph.diagnostics = [...new Set(graph.diagnostics)].sort(compare);
  return visualizeCluster(graph, cluster, dimensions, options);
}

/** Shape the view without turning desired capacity or inventory into membership.
 * A node group stays outside AZ/subnet boundaries even when it uses many of them. */
function visualizeCluster(graph: ClusterArchitectureGraph, cluster: Cluster, dimensions: ArchitectureDimensions, options: ClusterArchitectureOptions): ClusterArchitectureGraph {
  const compute = graph.inventoryNodes.filter(visibleCompute).map(node => ({ ...node, position: { ...node.position }, data: { ...node.data } }));
  const coreIds = new Set(compute.map(node => node.id));
  const degree = new Map<string, number>();
  const relevantIds = new Set([...coreIds, ...options.showNetwork ? graph.inventoryNodes.filter(node => networkKinds.has(node.data.kind)).map(node => node.id) : []]);
  for (const edge of graph.allEdges) if (relevantIds.has(edge.source) && relevantIds.has(edge.target)) for (const id of [edge.source, edge.target]) degree.set(id, (degree.get(id) ?? 0) + 1);
  const gap = 48 + Math.min(12, Math.max(0, ...degree.values())) * 4;
  let overlayNodes: ArchitectureNode[] = [], overlayEdges: ArchitectureEdge[] = [];
  const outsideReferences: ArchitectureNode[] = [];
  const aliases = new Map<string, string>();
  const network = options.network;
  if (options.showNetwork && (!network || network.id !== cluster.spec?.network?.networkId || network.tenantId !== cluster.tenantId)) {
    graph.diagnostics.push(network ? "Network overlay did not match this cluster's network and tenant." : "Network architecture has not been loaded.");
  } else if (options.showNetwork && network) {
    const account = options.account?.tenantId === cluster.tenantId && options.account.id === network.spec?.bootstrapId ? options.account : undefined;
    if (options.account && !account) graph.diagnostics.push("Cloud account context did not match this network and tenant.");
    const overlay = buildNetworkArchitectureOverview(network, account, [], dimensions);
    graph.diagnostics.push(...overlay.unresolved);
    const clusterRefs = graph.inventoryNodes.filter(node => networkKinds.has(node.data.kind) && graph.allEdges.some(edge => coreIds.has(edge.source) && edge.target === node.id || coreIds.has(edge.target) && edge.source === node.id));
    for (const reference of clusterRefs) {
      const sameKind = overlay.nodes.filter(node => node.data.kind === reference.data.kind);
      const byCloud = reference.data.cloudId ? sameKind.filter(node => node.data.cloudId === reference.data.cloudId) : [];
      const byName = sameKind.filter(node => (!reference.data.cloudId || !node.data.cloudId || node.data.cloudId === reference.data.cloudId) && (node.data.resourceId === reference.data.resourceId && !!reference.data.resourceId || node.data.resourceId === reference.data.title));
      const matches = byCloud.length ? byCloud : byName;
      if (matches.length === 1) aliases.set(reference.id, matches[0].id);
      else {
        if (reference.data.cloudId && sameKind.some(node => node.data.resourceId === reference.data.title && node.data.cloudId && node.data.cloudId !== reference.data.cloudId)) graph.diagnostics.push(`Network cloud identity differs for ${reference.data.kind} ${reference.data.title}; reference retained separately.`);
        if (matches.length > 1) graph.diagnostics.push(`Ambiguous network identity for ${reference.data.kind} ${reference.data.title}; reference retained separately.`);
        outsideReferences.push({ ...reference, position: { ...reference.position }, data: { ...reference.data } });
      }
    }
    const canonicalEdges = new Map<string, ArchitectureEdge>();
    for (const edge of graph.allEdges) {
      const source = aliases.get(edge.source) ?? edge.source, target = aliases.get(edge.target) ?? edge.target;
      const id = JSON.stringify([source, target, edge.data.relation, edge.label]);
      if (!canonicalEdges.has(id) || edge.data.evidence === "observed") canonicalEdges.set(id, { ...edge, id, source, target });
    }
    graph.allEdges = [...canonicalEdges.values()];
    // Only actual worker facts with a single explicit placement may enter a
    // subnet. Conflicting configured/observed placements remain outside.
    const subnetIds = new Set([...overlay.nodes.filter(node => node.data.kind === "Subnet").map(node => node.id), ...clusterRefs.filter(node => node.data.kind === "Subnet").map(node => aliases.get(node.id) ?? node.id)]);
    for (const worker of compute.filter(node => workerKinds.has(node.data.kind))) {
      const placements = new Set(graph.allEdges.filter(edge => edge.source === worker.id && ["placed_in", "uses_subnet"].includes(edge.data.relation))
        .map(edge => edge.target).filter(id => subnetIds.has(id)));
      if (placements.size > 1) graph.diagnostics.push(`Ambiguous subnet placement for ${worker.data.title}; worker remains outside subnet boundaries.`);
      if (placements.size === 1) {
        const target = overlay.nodes.find(node => node.id === [...placements][0]);
        if (target?.type === "group") { worker.parentId = target.id; worker.extent = "parent"; overlay.nodes.push(worker); }
      }
    }
    const laidOut = layoutNetworkArchitecture(overlay, dimensions);
    overlayNodes = laidOut.nodes;
    overlayEdges = laidOut.edges;
    const metadata = new Map(graph.inventoryNodes.map(node => [aliases.get(node.id) ?? node.id, node]));
    for (const node of overlayNodes) if (node.data.resourceId || node.data.cloudId || workerKinds.has(node.data.kind)) metadata.set(node.id, node);
    for (const data of overlay.supportingResources) {
      if (!data.graphId) continue;
      const measured = dimensions.get(data.graphId);
      metadata.set(data.graphId, { id: data.graphId, type: "resource", position: { x: 0, y: 0 }, width: Math.max(288, measured?.width ?? 0), height: Math.max(128, measured?.height ?? 0), data: { ...data } });
    }
    graph.inventoryNodes = [...metadata.values()];
    graph.allEdges.push(...overlayEdges);
  }
  const freeCompute = compute.filter(node => !node.parentId);
  // Three measured cards per row keeps multiple node groups compact without
  // assigning a canonical AZ to a multi-subnet group.
  const pack = (nodes: ArchitectureNode[], top: number, columns = 3) => {
    const widths: number[] = [], heights: number[] = [];
    nodes.forEach((node, index) => { widths[index % columns] = Math.max(widths[index % columns] ?? 0, node.width); heights[Math.floor(index / columns)] = Math.max(heights[Math.floor(index / columns)] ?? 0, node.height); });
    const sum = (values: number[], end: number) => values.slice(0, end).reduce((total, value) => total + value + gap, 0);
    nodes.forEach((node, index) => { node.position = { x: sum(widths, index % columns), y: top + sum(heights, Math.floor(index / columns)) }; });
    return { width: Math.max(0, sum(widths, widths.length) - gap), bottom: top + Math.max(0, sum(heights, heights.length) - gap) };
  };
  const controls = freeCompute.filter(node => node.data.kind === "Cluster");
  const groups = freeCompute.filter(node => node.data.kind === "NodeGroup");
  const actualWorkers = freeCompute.filter(node => workerKinds.has(node.data.kind));
  const members = new Map<string, ArchitectureNode[]>(), unassigned: ArchitectureNode[] = [];
  for (const worker of actualWorkers) {
    const owners = new Set(graph.allEdges.flatMap(edge => {
      if (edge.target === worker.id && ["has_node", "has_worker", "has_instance", "has_worker_node", "contains"].includes(edge.data.relation) && groups.some(group => group.id === edge.source)) return [edge.source];
      if (edge.source === worker.id && ["member_of", "belongs_to", "owned_by", "part_of", "node_group"].includes(edge.data.relation) && groups.some(group => group.id === edge.target)) return [edge.target];
      return [];
    }));
    if (owners.size === 1) { const owner = [...owners][0]; members.set(owner, [...members.get(owner) ?? [], worker]); }
    else { unassigned.push(worker); if (owners.size > 1) graph.diagnostics.push(`Ambiguous node-group membership for ${worker.data.title}; worker remains separate.`); }
  }
  const controlBox = pack(controls, 0);
  const top = controls.length ? controlBox.bottom + gap : 0;
  // These rectangles are packing units only, never emitted as invented nodes.
  const units = groups.map(group => {
    const children = members.get(group.id) ?? [];
    const childBox = pack(children, group.height + gap, 2);
    const width = Math.max(group.width, childBox.width), height = children.length ? childBox.bottom : group.height;
    group.position = { x: (width - group.width) / 2, y: 0 };
    for (const child of children) child.position.x += (width - childBox.width) / 2;
    return { ...group, position: { x: 0, y: 0 }, width, height };
  });
  const groupBox = pack(units, top);
  for (const unit of units) for (const node of [groups.find(group => group.id === unit.id)!, ...members.get(unit.id) ?? []]) {
    node.position.x += unit.position.x; node.position.y += unit.position.y;
  }
  const looseBox = pack(unassigned, groups.length ? groupBox.bottom + gap : top);
  const workerBox = { width: Math.max(groupBox.width, looseBox.width), bottom: Math.max(groupBox.bottom, looseBox.bottom) };
  for (const node of groups.flatMap(group => [group, ...members.get(group.id) ?? []])) node.position.x += (workerBox.width - groupBox.width) / 2;
  for (const node of unassigned) node.position.x += (workerBox.width - looseBox.width) / 2;
  const workers = [...groups, ...actualWorkers];
  const computeBottom = Math.max(controlBox.bottom, workerBox.bottom);
  const overlayWidth = Math.max(0, ...overlayNodes.filter(node => !node.parentId).map(node => node.position.x + node.width));
  const referenceBox = pack(outsideReferences, computeBottom + (freeCompute.length && outsideReferences.length ? gap : 0));
  const width = Math.max(overlayWidth, controlBox.width, workerBox.width, referenceBox.width);
  for (const node of controls) node.position.x += (width - controlBox.width) / 2;
  for (const node of workers) node.position.x += (width - workerBox.width) / 2;
  for (const node of outsideReferences) node.position.x += (width - referenceBox.width) / 2;
  for (const node of overlayNodes) if (!node.parentId) node.position.y += freeCompute.length || outsideReferences.length ? Math.max(computeBottom, referenceBox.bottom) + gap : 0;
  graph.nodes = [...freeCompute, ...overlayNodes, ...outsideReferences];
  const ids = new Set(graph.nodes.map(node => node.id));
  graph.edges = graph.allEdges.filter(edge => ids.has(edge.source) && ids.has(edge.target) && edge.data.relation !== "nat_internet_egress")
    .map(edge => overlayEdges.some(networkEdge => networkEdge.id === edge.id) ? edge : { ...edge, label: "" });
  graph.resources = graph.nodes.filter(node => node.type !== "group" || !!node.data.resourceId || !!node.data.cloudId);
  // All metadata is kept independently of visibility. Inspection can resolve
  // uses_subnet even when network context is turned off.
  const positioned = new Map(graph.nodes.map(node => [node.id, node]));
  graph.inventoryNodes = graph.inventoryNodes.map(node => positioned.get(aliases.get(node.id) ?? node.id) ?? node);
  graph.diagnostics = [...new Set(graph.diagnostics)].sort(compare);
  return graph;
}
