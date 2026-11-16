import { SubnetRole, type NetworkArchitectureResource } from "../../gen/cloud/v1/network_pb";
import { resourceKey, type NetworkInfrastructure } from "./network-infrastructure";
import type { ArchitectureEdge, ArchitectureNode, NetworkArchitectureGraph, NetworkTopology, TopologyNodeData } from "./network-topology";

/** Semantic graph only: AWS discovery and positioning deliberately live elsewhere. */
export function buildArchitectureGraph(model: NetworkInfrastructure, inventory: NetworkTopology): NetworkArchitectureGraph {
  const out: NetworkArchitectureGraph = { nodes: [], edges: [], supportingResources: [], unresolved: [...model.diagnostics], total: inventory.total, matched: 0 };
  if (!model.networkId) return out;
  const { resources, connections, identities } = model;
  const identity = (kind: string, name: string) => identities.get(resourceKey(kind, name))!;
  const facts = [...resources.values()];
  const byName = new Map(inventory.nodes.filter(node => node.data.kind !== "Network" && node.data.kind !== "Account").map(node => [resourceKey(node.data.kind, node.data.resourceId), node.data]));
  const nodes = new Map<string, ArchitectureNode>();
  const edges = new Map<string, ArchitectureEdge>();
  const context = (id: string, title: string, kind: string, boundary?: TopologyNodeData["boundary"], parentId?: string): ArchitectureNode => ({
    id, type: boundary ? "group" : "external", parentId, extent: parentId ? "parent" : undefined,
    position: { x: 0, y: 0 }, width: 0, height: 0,
    data: { title, kind, resourceId: "", cloudId: "", state: "Placement context", tone: "neutral", paused: false, href: "", message: "Diagram context, not an additional AWS resource.", presentation: boundary ? "group" : "external", boundary },
  });
  const dataFor = (fact: NetworkArchitectureResource): TopologyNodeData => ({
    ...(byName.get(resourceKey(fact.kind, fact.name)) ?? { title: fact.name, kind: fact.kind, resourceId: fact.name, cloudId: fact.cloudId, state: "Not reported", tone: "neutral", paused: false, href: "", message: "Architecture reference reported; reconciliation status is unavailable." }),
    graphId: identity(fact.kind, fact.name), referenceOnly: fact.referenceOnly, cloudId: fact.cloudId, cidr: fact.cidrBlock || fact.destinationCidrBlock || fact.destinationIpv6CidrBlock, zoneId: fact.zoneId,
  });
  const parent = (fact: NetworkArchitectureResource, relation: string, kind: string) => {
    const matches = new Set(connections.filter(e => e.sourceKind === fact.kind && e.sourceName === fact.name && e.relation === relation && e.targetKind === kind).map(e => identity(e.targetKind, e.targetName)));
    if (matches.size > 1) out.unresolved.push(`${fact.kind} ${fact.name}: configured/observed placement differs or is ambiguous.`);
    return matches.size === 1 ? [...matches][0] : undefined;
  };
  const attach = (node: ArchitectureNode, parentId?: string) => { node.parentId = parentId; node.extent = parentId ? "parent" : undefined; };
  const add = (source: string, target: string, relation: string, label: string, evidence: ArchitectureEdge["data"]["evidence"], derived = false, routeIds: string[] = []) => {
    if (!nodes.has(source) || !nodes.has(target) || source === target) return;
    const id = JSON.stringify([source, target, relation, label, evidence]);
    const previous = edges.get(id);
    edges.set(id, { id, source, target, label, data: { relation, evidence, derived, relationship: relation === "routes_via" ? "associated-with" : relation === "uses_address" ? "attached-to" : "routes-to", routeIds: [...new Set([...previous?.data.routeIds ?? [], ...routeIds])].sort() } });
  };
  const hidden = new Set(["Route", "RouteTableAssociation"]);
  for (const fact of facts) {
    const data = dataFor(fact);
    if (fact.referenceOnly) { data.state = "Referenced target"; data.tone = "neutral"; data.message = "Explicit route target outside the owned inventory. Availability and ownership are not verified."; }
    if (hidden.has(fact.kind)) { out.supportingResources.push(data); continue; }
    data.presentation = ["VPC", "Subnet"].includes(fact.kind) ? "group" : "resource";
    if (fact.kind === "VPC") data.boundary = "vpc";
    if (fact.kind === "Subnet") data.boundary = fact.subnetRole === SubnetRole.PUBLIC ? "public" : fact.subnetRole === SubnetRole.PRIVATE ? "private" : "subnet";
    if (fact.kind === "EIP" && fact.publicIp) data.title = fact.publicIp;
    nodes.set(identity(fact.kind, fact.name), { id: identity(fact.kind, fact.name), type: data.presentation, position: { x: 0, y: 0 }, width: 0, height: 0, data });
  }
  const region = context(`region:${model.networkId}`, model.region || "Region not reported", "AWS Region", "region");
  if (nodes.size) {
    nodes.set(region.id, region);
    // The inventory adapter already validates tenant and bootstrap ownership.
    // Account context is not an extra provisioned resource or traffic endpoint.
    const owner = inventory.nodes.find(node => node.data.kind === "Account");
    if (owner) {
      const account = context(owner.id, owner.data.title, "AWS Account", "account");
      account.data = { ...owner.data, kind: "AWS Account", presentation: "group", boundary: "account" };
      nodes.set(account.id, account);
      attach(region, account.id);
    }
  }
  for (const vpc of facts.filter(f => f.kind === "VPC")) {
    const vpcId = identity(vpc.kind, vpc.name);
    attach(nodes.get(vpcId)!, region.id);
  }
  for (const fact of facts.filter(f => f.kind !== "VPC" && !hidden.has(f.kind))) {
    const node = nodes.get(identity(fact.kind, fact.name))!;
    const vpc = parent(fact, "contained_in", "VPC");
    if (fact.kind === "NATGateway") {
      const subnet = parent(fact, "placed_in", "Subnet");
      attach(node, subnet);
      if (!subnet) node.data.message = [node.data.message, "Subnet placement is unavailable or ambiguous."].filter(Boolean).join(" ");
    } else if (fact.kind === "Subnet" && vpc && fact.zoneId) {
      const zoneId = `zone:${vpc}:${fact.zoneId}`;
      if (!nodes.has(zoneId)) {
        const zone = context(zoneId, fact.zoneId, "Availability zone", "zone", vpc);
        zone.data.zoneId = fact.zoneId; nodes.set(zoneId, zone);
      }
      attach(node, zoneId);
    } else if (fact.kind === "RouteTable" && vpc) attach(node, vpc);
    else attach(node, vpc);
  }
  // Elastic IP allocation is regional, not a physical resource inside a subnet.
  // An unambiguous allocation becomes an inspectable attached badge below.
  for (const fact of facts.filter(f => f.kind === "EIP")) attach(nodes.get(identity(fact.kind, fact.name))!, region.id);
  const routeTarget = (kind: string, name: string) => {
    const target = identity(kind, name);
    if (kind !== "VPC") return target;
    const localId = `local:${target}`;
    if (!nodes.has(localId)) {
      const local = context(localId, "VPC local network", "Local route destination", undefined, target);
      local.data.cidr = resources.get(resourceKey("VPC", name))?.cidrBlock;
      nodes.set(localId, local);
    }
    return localId;
  };
  for (const e of connections) {
    if (e.relation === "routes_via" || e.relation === "uses_address") add(identity(e.sourceKind, e.sourceName), identity(e.targetKind, e.targetName), e.relation, "", e.evidence as "configured" | "observed");
    // Inline observed route table entries include AWS's local route and targets
    // that have no separate Crossplane Route object.
    if (e.relation === "forwards_to" && e.sourceKind === "RouteTable" && e.label) {
      const table = nodes.get(identity(e.sourceKind, e.sourceName))!;
      const target = routeTarget(e.targetKind, e.targetName);
      const evidence = e.evidence as "configured" | "observed";
      const routeId = JSON.stringify([table.id, e.label, target, evidence]);
      table.data.routes ??= [];
      table.data.routes.push({ id: routeId, destination: e.label, targetId: target, targetKind: e.targetKind, targetName: e.targetName, evidence, message: [e.routeState, e.routeOrigin].filter(Boolean).join(" · ") });
      add(table.id, target, "forwards_to", e.label, evidence, false, [routeId]);
    }
  }
  for (const route of facts.filter(f => f.kind === "Route")) {
    const containers = connections.filter(e => e.sourceKind === "Route" && e.sourceName === route.name && e.targetKind === "RouteTable" && e.relation === "contained_in");
    const targets = connections.filter(e => e.sourceKind === "Route" && e.sourceName === route.name && e.relation === "forwards_to");
    for (const container of containers) {
      const table = nodes.get(identity("RouteTable", container.targetName));
      if (!table) continue;
      const matches = targets.filter(e => e.evidence === container.evidence);
      // Coalesced evidence may be reused only if it cannot combine two different
      // table/target versions into an invented route during a reconciliation.
      const candidates = containers.length > 1 && matches.length ? matches : targets;
      for (const forward of candidates) {
        const destination = forward.label || route.destinationCidrBlock || route.destinationIpv6CidrBlock;
        if (!destination) { out.unresolved.push(`Route ${route.name} has no reported destination.`); continue; }
        const target = routeTarget(forward.targetKind, forward.targetName);
        const evidence = container.evidence === "observed" && forward.evidence === "observed" ? "observed" : "configured";
        table.data.routes ??= [];
        table.data.routes.push({ id: identity(route.kind, route.name), destination, targetId: target, targetKind: forward.targetKind, targetName: forward.targetName, evidence, message: byName.get(resourceKey(route.kind, route.name))?.message });
        add(table.id, target, "forwards_to", destination, evidence, false, [identity(route.kind, route.name)]);
      }
      if (!candidates.length) {
        table.data.routes ??= [];
        table.data.routes.push({ id: identity(route.kind, route.name), destination: route.destinationCidrBlock || route.destinationIpv6CidrBlock || "Not reported", targetId: "", targetKind: "Unresolved", targetName: "Target not reported", evidence: container.evidence as "configured" | "observed" });
      }
    }
  }
  // A logical NAT-to-internet segment requires the actual public placement and
  // that subnet's explicit default route to an IGW. Never infer same-AZ egress.
  for (const placement of connections.filter(e => e.sourceKind === "NATGateway" && e.targetKind === "Subnet" && e.relation === "placed_in")) {
    const natId = identity(placement.sourceKind, placement.sourceName), subnetId = identity(placement.targetKind, placement.targetName);
    if (nodes.get(natId)?.parentId !== subnetId || nodes.get(subnetId)?.data.boundary !== "public") continue;
    for (const association of [...edges.values()].filter(e => e.source === subnetId && e.data.relation === "routes_via")) {
      for (const route of [...edges.values()].filter(e => e.source === association.target && e.data.relation === "forwards_to" && ["0.0.0.0/0", "::/0"].includes(e.label) && nodes.get(e.target)?.data.kind === "InternetGateway")) {
        const evidence = placement.evidence === "observed" && association.data.evidence === "observed" && route.data.evidence === "observed" ? "observed" : "configured";
        add(natId, route.target, "nat_internet_egress", "Internet egress", evidence, true, route.data.routeIds);
      }
    }
  }
  const igws = [...nodes.values()].filter(n => n.data.kind === "InternetGateway");
  if (igws.length) {
    const internet = context(`internet:${model.networkId}`, "Internet", "Logical destination", undefined, region.id);
    internet.data.state = "Logical context"; nodes.set(internet.id, internet);
    for (const igw of igws) add(igw.id, internet.id, "logical_egress", "Internet", "logical", true);
  }
  // Compact attachment presentation keeps the allocation identity inspectable.
  // A shared/ambiguous address remains a standalone resource instead of being
  // copied onto two NAT cards or implying physical subnet containment.
  for (const address of [...nodes.values()].filter(n => n.data.kind === "EIP")) {
    const owners = [...edges.values()].filter(e => e.target === address.id && e.data.relation === "uses_address");
    const distinct = [...new Set(owners.map(e => e.source))];
    if (distinct.length !== 1 || nodes.get(distinct[0])?.data.kind !== "NATGateway") continue;
    const nat = nodes.get(distinct[0])!;
    nat.data.attachments ??= []; nat.data.attachments.push(address.data);
    out.supportingResources.push(address.data); nodes.delete(address.id);
    for (const edge of owners) edges.delete(edge.id);
  }
  for (const node of inventory.nodes.filter(n => n.data.kind === "Cluster")) out.supportingResources.push({ ...node.data, graphId: node.id });
  for (const [key, data] of byName) if (!resources.has(key) && data.kind !== "Cluster") out.supportingResources.push(data);
  for (const edge of edges.values()) {
    if (edge.data.relation !== "forwards_to" || nodes.get(edge.target)?.data.kind !== "NATGateway") continue;
    const nat = nodes.get(edge.target)!;
    const targetZone = nodes.get(nat.parentId ?? "")?.data.zoneId;
    edge.data.crossZone = !!targetZone && [...edges.values()].some(e => e.target === edge.source && e.data.relation === "routes_via" && !!nodes.get(e.source)?.data.zoneId && nodes.get(e.source)?.data.zoneId !== targetZone);
  }
  // Preserve all provider relationships, including resources represented by
  // compact attachments or inspector rows. Containment is never a visible edge.
  out.relationships = connections.map(e => {
    const contains = e.relation === "contained_in" || e.relation === "placed_in";
    const type = contains ? "contains" : e.relation === "forwards_to" ? "routes-to" : e.relation === "uses_address" ? "attached-to" : "associated-with";
    const source = identity(contains ? e.targetKind : e.sourceKind, contains ? e.targetName : e.sourceName);
    const target = identity(contains ? e.sourceKind : e.targetKind, contains ? e.sourceName : e.targetName);
    return { id: JSON.stringify([source, target, type, e.label, e.evidence]), source, target, type, evidence: e.evidence as "observed" | "configured" };
  });
  for (const node of inventory.nodes.filter(n => n.data.kind === "Cluster")) out.relationships.push({ id: `uses-network:${node.id}`, source: node.id, target: `network:${encodeURIComponent(model.networkId)}`, type: "uses-network", evidence: "observed" });
  const owner = [...nodes.values()].find(node => node.data.boundary === "account");
  if (owner) for (const node of nodes.values()) {
    // An owned resource with unresolved subnet/VPC placement still belongs to
    // this account. External route targets carry no such ownership assertion.
    if (node !== owner && !node.parentId && !node.data.referenceOnly) attach(node, owner.id);
  }
  out.nodes = [...nodes.values()].sort((a, b) => a.id.localeCompare(b.id));
  out.edges = [...edges.values()].sort((a, b) => a.id.localeCompare(b.id));
  out.total = new Set([...byName.keys(), ...resources.keys()]).size;
  out.matched = facts.filter(f => !hidden.has(f.kind)).length;
  out.unresolved = [...new Set(out.unresolved)].sort();
  return out;
}
