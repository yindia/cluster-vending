import type { ArchitectureRoute, NetworkArchitectureGraph, TopologyNodeData } from "./network-topology";

export type ArchitectureSecurityFinding = { id: string; nodeId: string; title: string; detail: string; level: "info" | "warning" };
export type ArchitectureSecurityCoverage = { securityGroups: number; networkAcls: number; rulesReported: false; exposureEvaluated: false; message: string };
const compare = (a: string, b: string) => a < b ? -1 : a > b ? 1 : 0;
const isDefault = (destination: string) => destination === "0.0.0.0/0" || destination === "::/0";
const isAcl = (kind: string) => kind === "NetworkACL" || kind === "NetworkAcl";

function inventory(graph: NetworkArchitectureGraph) {
  const rows = new Map<string, { nodeId: string; data: TopologyNodeData }>();
  const add = (data: TopologyNodeData, nodeId: string) => {
    const key = data.graphId || JSON.stringify([data.kind, data.cloudId || data.resourceId]);
    if (!rows.has(key)) rows.set(key, { nodeId, data });
    for (const attachment of data.attachments ?? []) add(attachment, attachment.graphId || nodeId);
  };
  for (const node of [...graph.nodes].sort((a, b) => compare(a.id, b.id))) add(node.data, node.id);
  for (const data of [...graph.supportingResources].sort((a, b) => compare(JSON.stringify(a), JSON.stringify(b)))) add(data, data.graphId || JSON.stringify([data.kind, data.resourceId]));
  return [...rows.values()];
}

/** Inventory presence cannot establish effective SG/NACL rules or exposure. */
export function architectureSecurityCoverage(graph: NetworkArchitectureGraph): ArchitectureSecurityCoverage {
  const rows = inventory(graph);
  return { securityGroups: rows.filter(row => row.data.kind === "SecurityGroup").length, networkAcls: rows.filter(row => isAcl(row.data.kind)).length,
    rulesReported: false, exposureEvaluated: false, message: "SG/NACL rules not reported; exposure not evaluated." };
}

/** Read-only observations of explicit graph facts. No reachability analysis,
 * desired-topology inference, rule evaluation, or cloud calls are performed. */
export function inspectArchitectureSecurity(graph: NetworkArchitectureGraph): ArchitectureSecurityFinding[] {
  const nodes = new Map(graph.nodes.map(node => [node.id, node]));
  const findings = new Map<string, ArchitectureSecurityFinding>();
  const add = (code: string, nodeId: string, title: string, detail: string, level: ArchitectureSecurityFinding["level"] = "info", identity = "") => {
    const id = JSON.stringify(["architecture-security", code, nodeId, identity]);
    const finding = { id, nodeId, title, detail, level };
    const previous = findings.get(id);
    if (!previous || compare(detail, previous.detail) < 0) findings.set(id, finding);
  };
  const routes = new Map<string, ArchitectureRoute[]>();
  for (const table of graph.nodes.filter(node => node.data.kind === "RouteTable")) routes.set(table.id, [...table.data.routes ?? []]);
  // Some projections supply compact route edges without inspector rows.
  for (const edge of graph.edges) {
    if (edge.data.relation !== "forwards_to" || nodes.get(edge.source)?.data.kind !== "RouteTable" || edge.data.evidence === "logical") continue;
    const target = nodes.get(edge.target);
    if (!target) continue;
    const rows = routes.get(edge.source)!;
    if (!rows.some(row => row.destination === edge.label && row.targetId === edge.target && row.evidence === edge.data.evidence)) rows.push({ id: edge.id, destination: edge.label, targetId: edge.target, targetKind: target.data.kind, targetName: target.data.resourceId, evidence: edge.data.evidence });
  }
  for (const [tableId, rows] of routes) {
    for (const row of rows.filter(row => isDefault(row.destination) && row.targetKind === "InternetGateway")) {
      add("igw-default", tableId, "Internet gateway default route", `${row.destination} references internet gateway ${row.targetName || row.targetId} (${row.evidence}). This reports routing configuration, not proof of public exposure or reachability.${row.message ? ` Reported route state: ${row.message}.` : ""}`, "info", JSON.stringify([row.destination, row.targetId || row.targetName, row.evidence]));
    }
  }
  for (const edge of graph.edges) {
    if (edge.data.crossZone !== true || !nodes.has(edge.source) || !nodes.has(edge.target)) continue;
    add("cross-zone", edge.source, "Cross-zone route reference", `${edge.label || "Route"} has an explicitly reported cross-zone target (${edge.data.evidence}). This does not establish successful traffic flow.`, "warning", edge.id);
  }
  const addresses = new Map(inventory(graph).filter(row => row.data.kind === "EIP").map(row => [row.data.graphId || row.nodeId, row.data]));
  for (const nat of graph.nodes.filter(node => node.data.kind === "NATGateway")) {
    const attached = new Map<string, TopologyNodeData>();
    for (const address of nat.data.attachments ?? []) if (address.kind === "EIP") attached.set(address.graphId || JSON.stringify([address.kind, address.resourceId]), address);
    for (const edge of graph.edges.filter(edge => edge.source === nat.id && edge.data.relation === "uses_address")) {
      const address = nodes.get(edge.target)?.data;
      if (address?.kind === "EIP") attached.set(address.graphId || JSON.stringify([address.kind, address.resourceId]), address);
    }
    for (const relation of graph.relationships ?? []) if (relation.source === nat.id && relation.type === "attached-to") {
      const address = addresses.get(relation.target);
      if (address) attached.set(address.graphId || JSON.stringify([address.kind, address.resourceId]), address);
    }
    for (const [key, address] of attached) add("public-address", nat.id, "Public address association", `NAT gateway references Elastic IP ${address.title || address.resourceId || address.cloudId}. An address association alone does not establish public exposure, inbound access, or working internet egress.`, "info", key);
  }
  for (const subnet of graph.nodes.filter(node => node.data.kind === "Subnet")) {
    const observed = new Set<string>(), configured = new Set<string>();
    const association = (target: string, evidence: string) => {
      if (nodes.get(target)?.data.kind !== "RouteTable") return;
      if (evidence === "observed") observed.add(target);
      if (evidence === "configured") configured.add(target);
    };
    for (const edge of graph.edges) if (edge.source === subnet.id && edge.data.relation === "routes_via") association(edge.target, edge.data.evidence);
    for (const relation of graph.relationships ?? []) if (relation.source === subnet.id && relation.type === "associated-with") association(relation.target, relation.evidence);
    if (observed.size > 1) add("association-ambiguous", subnet.id, "Ambiguous observed route-table associations", "The snapshot reports more than one observed route-table association for this subnet. The effective association is ambiguous; simultaneous use is not established.", "warning");
    if (configured.size && observed.size && [...configured].some(target => !observed.has(target))) add("association-drift", subnet.id, "Configured and observed associations differ", "Configured route-table references differ from the observed association. This is reconciliation drift, not evidence that multiple route tables are currently effective.", "warning");
    if (configured.size > 1 && observed.size <= 1) add("association-configured", subnet.id, "Multiple configured route-table references", "More than one configured route-table reference is reported. These references do not establish multiple effective associations.");
    const tables = new Set([...observed, ...configured]);
    if (![...tables].some(table => routes.get(table)?.some(row => isDefault(row.destination)))) add("default-unreported", subnet.id, "No default route reported", "This snapshot does not report a default route through the subnet's explicit route-table associations. Incomplete observations or unreported associations may explain the gap; absence and isolation are not established.");
  }
  for (const { nodeId, data } of inventory(graph)) if (data.kind === "SecurityGroup" || isAcl(data.kind)) add("filter-inventory", nodeId, `${data.kind === "SecurityGroup" ? "Security group" : "Network ACL"} inventory reported`, `${data.title || data.resourceId || data.cloudId} is present in resource inventory. SG/NACL rules not reported; exposure not evaluated.`);
  return [...findings.values()].sort((a, b) => compare(a.id, b.id));
}
