import assert from "node:assert/strict";
import test from "node:test";
import { clone, create, toJsonString } from "@bufbuild/protobuf";
import { ClusterSchema, ClusterState } from "../../gen/cloud/v1/cluster_pb";
import { ConditionStatus } from "../../gen/cloud/v1/conditions_pb";
import { ManagedResourceSchema, ManagedResourceState } from "../../gen/cloud/v1/managed_resource_pb";
import { NetworkArchitectureConnectionSchema, NetworkArchitectureResourceSchema, NetworkArchitectureSchema, NetworkSchema, NetworkState, SubnetRole, type NetworkArchitectureResource } from "../../gen/cloud/v1/network_pb";
import { CloudAccountSchema } from "../../gen/cloud/v1/tenant_service_pb";
import { buildNetworkArchitecture, buildNetworkArchitectureOverview, buildNetworkTopology, type ArchitectureNode } from "./network-topology";
import { createNetworkTopologyFixture, networkTopologyFixtureIds } from "./network-topology-fixtures";

const mr = (kind: string, name: string) => create(ManagedResourceSchema, { kind, name, externalName: `cloud-${name}`, generation: 3n, observedGeneration: 3n, ready: ConditionStatus.TRUE, synced: ConditionStatus.TRUE, state: ManagedResourceState.READY });
const network = () => create(NetworkSchema, { id: "network-a", tenantId: "tenant-a", name: "payments", generation: 3n, spec: { bootstrapId: "account-a" }, status: { state: NetworkState.READY, observedGeneration: 3n } });
const cluster = (id: string, tenantId = "tenant-a", networkId = "network-a") => create(ClusterSchema, { id, tenantId, name: id, generation: 3n, spec: { network: { networkId } }, status: { state: ClusterState.RUNNING, observedGeneration: 3n } });
const fact = (kind: string, name: string, extra: Partial<Omit<NetworkArchitectureResource, "$typeName" | "$unknown">> = {}) => create(NetworkArchitectureResourceSchema, { kind, name, cloudId: `cloud-${name}`, ...extra });
const link = (sourceKind: string, sourceName: string, targetKind: string, targetName: string, relation: string, evidence = "observed") => create(NetworkArchitectureConnectionSchema, { sourceKind, sourceName, targetKind, targetName, relation, evidence, label: relation === "forwards_to" ? "0.0.0.0/0" : relation });

function architecture() {
  const n = network();
  const resources = [
    fact("VPC", "vpc", { cidrBlock: "10.0.0.0/16" }),
    fact("Subnet", "private", { zoneId: "zone-a", subnetRole: SubnetRole.PRIVATE, cidrBlock: "10.0.1.0/24" }),
    fact("Subnet", "public", { zoneId: "zone-a", subnetRole: SubnetRole.PUBLIC, cidrBlock: "10.0.2.0/24" }),
    fact("RouteTable", "private-table"), fact("RouteTable", "public-table"),
    fact("Route", "private-default", { destinationCidrBlock: "0.0.0.0/0" }), fact("Route", "public-default", { destinationCidrBlock: "0.0.0.0/0" }),
    fact("NATGateway", "nat"), fact("EIP", "eip", { publicIp: "203.0.113.5" }), fact("InternetGateway", "igw"), fact("RouteTableAssociation", "association"),
  ];
  const connections = [
    ...[resources[1], resources[2], resources[3], resources[4], resources[7], resources[9]].map(r => link(r.kind, r.name, "VPC", "vpc", "contained_in")),
    link("Subnet", "private", "RouteTable", "private-table", "routes_via"), link("Subnet", "public", "RouteTable", "public-table", "routes_via"),
    link("Route", "private-default", "RouteTable", "private-table", "contained_in"), link("Route", "public-default", "RouteTable", "public-table", "contained_in"),
    link("Route", "private-default", "NATGateway", "nat", "forwards_to"), link("Route", "public-default", "InternetGateway", "igw", "forwards_to"),
    link("NATGateway", "nat", "Subnet", "public", "placed_in"), link("NATGateway", "nat", "EIP", "eip", "uses_address"),
  ];
  n.status!.architecture = create(NetworkArchitectureSchema, { resources, connections });
  n.status!.managedResources = [...resources.map(r => mr(r.kind, r.name)), mr("Policy", "flow-log-policy")];
  return n;
}

test("inventory scopes account and clusters to explicit tenant/network IDs and counts leaves", () => {
  const n = network();
  n.status!.managedResources = [mr("VPC", "vpc"), mr("Subnet", "private")];
  const account = create(CloudAccountSchema, { id: "account-a", tenantId: "tenant-a", accountName: "Production" });
  const graph = buildNetworkTopology(n, account, [cluster("owned"), cluster("foreign", "foreign"), cluster("other-network", "tenant-a", "other")]);
  assert.equal(graph.total, 3);
  assert.equal(graph.matched, 3);
  assert.equal(graph.nodes.length, 5);
  assert.equal(graph.edges.filter(edge => edge.source.startsWith("account:")).length, 1);
  assert.equal(buildNetworkTopology(n, { ...account, tenantId: "foreign" }).nodes.some(node => node.data.kind === "Account"), false);
  assert.equal(buildNetworkTopology(n, { ...account, id: "unrelated" }).nodes.some(node => node.data.kind === "Account"), false);
  assert.equal(graph.nodes.some(node => node.data.title === "foreign" || node.data.title === "other-network"), false);
});

test("inventory IDs, deduplication and layout are stable with a three-column grid", () => {
  const n = network();
  n.status!.managedResources = Array.from({ length: 100 }, (_, i) => mr("Subnet", `subnet-${String(i).padStart(3, "0")}`));
  n.status!.managedResources.push(mr("Subnet", "subnet-000"));
  const before = toJsonString(NetworkSchema, n);
  const graph = buildNetworkTopology(n);
  n.status!.managedResources.reverse();
  assert.deepEqual(buildNetworkTopology(n), graph);
  assert.equal(graph.total, 100);
  const leaves = graph.nodes.filter(node => node.id.startsWith("managed-resource:"));
  assert.equal(new Set(leaves.map(node => node.position.x)).size, 3);
  assert.equal(Math.max(...leaves.map(node => node.position.y)), 6600);
  assert.equal(new Set(graph.nodes.map(node => node.id)).size, graph.nodes.length);
  n.status!.managedResources.reverse();
  assert.equal(toJsonString(NetworkSchema, n), before);
  const foreignAccount = create(CloudAccountSchema, { id: "account-a", tenantId: "foreign" });
  assert.equal(buildNetworkTopology(n, foreignAccount).nodes[0].position.y, 3300);
});

test("stale ready/failed status is reconciling without repeating an old error; paused stays explicit", () => {
  const n = network();
  n.status!.state = NetworkState.FAILED;
  n.status!.observedGeneration = 2n;
  n.status!.message = "old network failure";
  n.status!.managedResources = [mr("Subnet", "stale-ready"), { ...mr("NATGateway", "stale-failed"), state: ManagedResourceState.FAILED, message: "old provider error", observedGeneration: 2n }, { ...mr("VPC", "paused"), paused: true }];
  n.status!.managedResources[0].observedGeneration = 2n;
  const c = cluster("stale-cluster");
  c.status!.state = ClusterState.FAILED; c.status!.observedGeneration = 2n; c.status!.message = "old cluster failure";
  const graph = buildNetworkTopology(n, undefined, [c]);
  for (const node of graph.nodes.filter(node => node.data.resourceId !== "paused")) {
    assert.equal(node.data.state, "Reconciling");
    assert.equal(node.data.tone, "warning");
    assert.match(node.data.message, /Waiting for observation/);
    assert.doesNotMatch(node.data.message, /old /);
  }
  const paused = graph.nodes.find(node => node.data.resourceId === "paused")!;
  assert.equal(paused.data.paused, true); assert.equal(paused.data.tone, "warning");
});

test("empty/incomplete inventory invents neither provider resources nor routes", () => {
  assert.equal(buildNetworkTopology(undefined).nodes.length, 0);
  const n = network();
  assert.equal(buildNetworkTopology(n).total, 0);
  n.status!.managedResources = [create(ManagedResourceSchema), create(ManagedResourceSchema, { kind: "Subnet", externalName: "subnet-known" })];
  const graph = buildNetworkTopology(n);
  assert.equal(graph.total, 1);
  assert.equal(graph.nodes.at(-1)?.data.cloudId, "subnet-known");
  const arch = buildNetworkArchitecture(n);
  assert.equal(arch.nodes.length, 0);
  assert.equal(arch.edges.length, 0);
  assert.equal(arch.supportingResources.length, 1);
  assert.match(arch.unresolved.join(" "), /references are not available/);
});

test("architecture uses actual CIDRs, zones and nested parent-relative geometry", () => {
  const graph = buildNetworkArchitecture(architecture());
  const nodes = new Map(graph.nodes.map(node => [node.id, node]));
  const subnet = graph.nodes.find(node => node.data.kind === "Subnet" && node.data.resourceId === "public")!;
  const zone = nodes.get(subnet.parentId!)!;
  assert.equal(subnet.data.cidr, "10.0.2.0/24");
  assert.equal(subnet.data.boundary, "public");
  assert.equal(zone.data.zoneId, "zone-a");
  assert.equal(nodes.get(zone.parentId!)?.data.kind, "VPC");
  const nat = graph.nodes.find(node => node.data.kind === "NATGateway")!;
  assert.equal(nat.parentId, subnet.id);
  for (const node of graph.nodes) {
    if (!node.parentId) continue;
    const parent = nodes.get(node.parentId)!;
    assert.ok(graph.nodes.indexOf(parent) < graph.nodes.indexOf(node));
    assert.ok(node.position.x >= 0 && node.position.y >= 0);
    assert.ok(node.position.x + node.width <= parent.width, `${node.data.kind} exceeds parent width`);
    assert.ok(node.position.y + node.height <= parent.height, `${node.data.kind} exceeds parent height`);
  }
  assert.equal(graph.supportingResources.some(node => node.kind === "Policy"), true);
  assert.equal(graph.supportingResources.some(node => node.kind === "RouteTableAssociation"), true);
  assert.equal(graph.nodes.some(node => node.data.kind === "Policy"), false);
});

test("both architecture entry points show VPC route tables, compact targets and independent EIP attachments", () => {
  const n = architecture();
  const before = toJsonString(NetworkSchema, n);
  const graph = buildNetworkArchitecture(n);
  assert.deepEqual(buildNetworkArchitectureOverview(n), graph);
  const byName = (name: string) => graph.nodes.find(node => node.data.resourceId === name)!;
  const has = (source: string, target: string) => graph.edges.find(edge => edge.source === byName(source).id && edge.target === byName(target).id);
  assert.equal(graph.nodes.some(node => node.data.kind === "Route"), false);
  assert.ok(graph.supportingResources.some(node => node.kind === "Route" && node.resourceId === "private-default"));
  assert.equal(has("private", "private-table")?.data.relation, "routes_via");
  assert.equal(has("private-table", "nat")?.label, "0.0.0.0/0");
  assert.equal(has("public-table", "igw")?.label, "0.0.0.0/0");
  assert.equal(graph.nodes.some(node => node.data.kind === "EIP"), false);
  const address = graph.supportingResources.find(resource => resource.kind === "EIP" && resource.resourceId === "eip")!;
  assert.ok(address);
  assert.equal(address.cloudId, "cloud-eip");
  const natData = byName("nat").data;
  assert.ok("attachments" in natData && Array.isArray(natData.attachments));
  assert.equal(natData.attachments.length, 1);
  assert.deepEqual(natData.attachments[0], address);
  assert.equal(address.title, "203.0.113.5");
  assert.equal(has("nat", "igw")?.data.relation, "nat_internet_egress");
  assert.equal(has("nat", "igw")?.data.derived, true);
  const table = byName("private-table");
  assertVpcOwnership(table, new Map(graph.nodes.map(node => [node.id, node])));
  assert.equal(table.data.routes?.[0].targetId, byName("nat").id);
  assert.ok(has("private-table", "nat")?.data.routeIds?.includes(table.data.routes![0].id));
  assert.equal(graph.nodes.find(node => node.data.kind === "Logical destination")?.data.state, "Logical context");
  assert.equal(graph.edges.find(edge => edge.data.relation === "logical_egress")?.data.evidence, "logical");
  assert.equal(toJsonString(NetworkSchema, n), before);
});

test("NAT internet egress needs explicit public placement, association, and default forwarding", () => {
  for (const missing of ["placed_in", "routes_via", "contained_in", "forwards_to"]) {
    const n = architecture();
    n.status!.architecture!.connections = n.status!.architecture!.connections.filter(edge => edge.relation !== missing);
    assert.equal(buildNetworkArchitecture(n).edges.some(edge => edge.data.relation === "nat_internet_egress"), false, missing);
  }
  const n = architecture();
  n.status!.architecture!.connections.find(edge => edge.relation === "placed_in")!.evidence = "configured";
  const derived = buildNetworkArchitecture(n).edges.find(edge => edge.data.relation === "nat_internet_egress")!;
  assert.equal(derived.data.evidence, "configured");
  assert.equal(derived.data.derived, true);
  n.status!.architecture!.connections.find(edge => edge.sourceName === "public-default" && edge.relation === "forwards_to")!.label = "10.240.0.0/16";
  assert.equal(buildNetworkArchitecture(n).edges.some(edge => edge.data.relation === "nat_internet_egress"), false, "non-default routes do not prove internet egress");
});

test("architecture is stable under response ordering; missing and ambiguous refs stay unresolved", () => {
  const n = architecture();
  const expected = buildNetworkArchitecture(n);
  const reversed = clone(NetworkSchema, n);
  reversed.status!.architecture!.resources.reverse(); reversed.status!.architecture!.connections.reverse(); reversed.status!.managedResources.reverse();
  assert.deepEqual(buildNetworkArchitecture(reversed), expected);
  n.status!.architecture!.resources.push(fact("Subnet", "other-public", { subnetRole: SubnetRole.PUBLIC }));
  n.status!.architecture!.connections.push(link("NATGateway", "nat", "Subnet", "other-public", "placed_in", "configured"), link("Route", "private-default", "NATGateway", "missing", "forwards_to"));
  const graph = buildNetworkArchitecture(n);
  assert.equal(graph.nodes.find(node => node.data.resourceId === "nat")?.parentId, undefined);
  assert.match(graph.unresolved.join(" "), /ambiguous/);
  assert.match(graph.unresolved.join(" "), /unavailable endpoint/);
  assert.equal(graph.edges.some(edge => edge.label.includes("missing")), false);
});

test("route-table edges pair drifted evidence and use each forwarding edge's destination CIDR", () => {
  const n = architecture();
  const facts = n.status!.architecture!;
  facts.resources.push(fact("RouteTable", "configured-table"), fact("NATGateway", "configured-nat"));
  facts.connections.push(
    link("Route", "private-default", "RouteTable", "configured-table", "contained_in", "configured"),
    { ...link("Route", "private-default", "NATGateway", "configured-nat", "forwards_to", "configured"), label: "10.20.0.0/16" },
  );
  facts.connections.find(edge => edge.sourceName === "private-default" && edge.relation === "forwards_to" && edge.evidence === "observed")!.label = "2001:db8::/64";
  const graph = buildNetworkArchitecture(n);
  const node = (name: string) => graph.nodes.find(candidate => candidate.data.resourceId === name)!;
  const paths = (table: string) => graph.edges.filter(edge => edge.source === node(table).id && edge.data.relation === "forwards_to");
  assert.deepEqual(paths("configured-table").map(edge => [edge.target, edge.label, edge.data.evidence]), [[node("configured-nat").id, "10.20.0.0/16", "configured"]]);
  assert.deepEqual(paths("private-table").map(edge => [edge.target, edge.label, edge.data.evidence]), [[node("nat").id, "2001:db8::/64", "observed"]]);
});

test("coalesced observed forwarding can participate in a weaker configured table chain", () => {
  const n = architecture();
  n.status!.architecture!.connections.find(edge => edge.sourceName === "private-default" && edge.relation === "contained_in")!.evidence = "configured";
  const graph = buildNetworkArchitecture(n);
  const table = graph.nodes.find(node => node.data.resourceId === "private-table")!;
  const path = graph.edges.find(edge => edge.source === table.id && edge.data.relation === "forwards_to");
  assert.equal(path?.label, "0.0.0.0/0");
  assert.equal(path?.data.evidence, "configured");
});

test("inline non-default endpoint, peering and transit targets remain explicit unverified references", () => {
  const n = architecture();
  const facts = n.status!.architecture!;
  const targets = [
    { kind: "VPCEndpoint", name: "vpce-referenced", destination: "10.20.0.0/16" },
    { kind: "VPCPeeringConnection", name: "pcx-referenced", destination: "10.30.0.0/16" },
    { kind: "TransitGateway", name: "tgw-referenced", destination: "10.40.0.0/16" },
  ];
  for (const target of targets) {
    facts.resources.push(fact(target.kind, target.name, { cloudId: target.name, referenceOnly: true }));
    facts.connections.push(create(NetworkArchitectureConnectionSchema, { sourceKind: "RouteTable", sourceName: "private-table", targetKind: target.kind, targetName: target.name, relation: "forwards_to", label: target.destination, evidence: "observed", routeState: target.kind === "TransitGateway" ? "blackhole" : "active", routeOrigin: "CreateRoute" }));
  }
  const before = toJsonString(NetworkSchema, n);
  const graph = buildNetworkArchitecture(n);
  const table = graph.nodes.find(node => node.data.resourceId === "private-table")!;
  for (const target of targets) {
    const reference = graph.nodes.find(node => node.data.resourceId === target.name)!;
    assert.ok(reference);
    assert.equal(reference.data.kind, target.kind);
    assert.equal(reference.data.cloudId, target.name);
    assert.equal(reference.data.referenceOnly, true);
    assert.equal(reference.data.state, "Referenced target");
    assert.equal(reference.data.tone, "neutral");
    assert.match(reference.data.message, /not verified/);
    assert.equal(reference.parentId, undefined, "route reference alone does not prove physical containment");
    const path = graph.edges.find(edge => edge.source === table.id && edge.target === reference.id && edge.label === target.destination)!;
    assert.ok(path);
    assert.equal(path.data.relation, "forwards_to");
    assert.equal(path.data.evidence, "observed");
    const row = table.data.routes!.find(row => row.destination === target.destination)!;
    assert.equal(row.targetId, reference.id);
    assert.match(row.message!, target.kind === "TransitGateway" ? /blackhole/ : /active/);
    assert.equal(graph.edges.some(edge => edge.source === reference.id), false, "unreported downstream connections must not be invented");
  }
  assert.equal(toJsonString(NetworkSchema, n), before);
});

test("unattached or ambiguously attached EIPs retain standalone identities", () => {
  const unattached = architecture();
  unattached.status!.architecture!.connections = unattached.status!.architecture!.connections.filter(edge => edge.relation !== "uses_address");
  const standalone = buildNetworkArchitecture(unattached);
  assert.equal(standalone.nodes.filter(node => node.data.kind === "EIP").length, 1);
  const ambiguous = architecture();
  ambiguous.status!.architecture!.resources.push(fact("NATGateway", "other-nat"));
  ambiguous.status!.architecture!.connections.push(link("NATGateway", "other-nat", "EIP", "eip", "uses_address", "configured"));
  const graph = buildNetworkArchitecture(ambiguous);
  const address = graph.nodes.filter(node => node.data.kind === "EIP");
  assert.equal(address.length, 1, "ambiguous attachment must not hide an independently addressable EIP");
  assert.equal(address[0].data.resourceId, "eip");
  assert.equal(address[0].data.cloudId, "cloud-eip");
});

test("public subnets precede private ones and scoped cluster membership stays in supporting inventory", () => {
  const graph = buildNetworkArchitecture(architecture(), undefined, [cluster("payments"), cluster("foreign", "other-tenant"), cluster("elsewhere", "tenant-a", "other-network")]);
  const publicSubnet = graph.nodes.find(node => node.data.resourceId === "public")!;
  const privateSubnet = graph.nodes.find(node => node.data.resourceId === "private")!;
  assert.equal(publicSubnet.parentId, privateSubnet.parentId);
  assert.ok(publicSubnet.position.y < privateSubnet.position.y);
  assert.equal(graph.nodes.some(node => node.data.kind === "Cluster"), false);
  const scoped = graph.supportingResources.filter(node => node.kind === "Cluster");
  assert.equal(scoped.length, 1);
  assert.equal(scoped[0].resourceId, "payments");
  assert.equal(scoped[0].href, "/clusters/payments");
});

function assertVpcOwnership(table: ArchitectureNode, nodes: Map<string, ArchitectureNode>) {
  let parentId = table.parentId;
  const visited = new Set<string>();
  while (parentId) {
    assert.equal(visited.has(parentId), false);
    visited.add(parentId);
    const parent = nodes.get(parentId)!;
    assert.ok(parent, "route table parent must exist");
    assert.notEqual(parent.data.kind, "Subnet", "route tables do not belong inside subnets");
    assert.notEqual(parent.data.boundary, "zone", "route tables are VPC-scoped, not AZ resources");
    if (parent.data.kind === "VPC") return;
    parentId = parent.parentId;
  }
  assert.fail("route table must belong to a VPC");
}

function absoluteRect(node: ArchitectureNode, nodes: Map<string, ArchitectureNode>) {
  let { x, y } = node.position;
  let parentId = node.parentId;
  const visited = new Set([node.id]);
  while (parentId) {
    assert.equal(visited.has(parentId), false, "containment must not cycle");
    visited.add(parentId);
    const parent = nodes.get(parentId)!;
    assert.ok(parent, "parent must exist");
    x += parent.position.x; y += parent.position.y; parentId = parent.parentId;
  }
  return { x, y, width: node.width, height: node.height };
}
function overlap(a: ReturnType<typeof absoluteRect>, b: ReturnType<typeof absoluteRect>) {
  return a.x < b.x + b.width && a.x + a.width > b.x && a.y < b.y + b.height && a.y + a.height > b.y;
}

for (const fixtureId of networkTopologyFixtureIds) {
  test(`architecture fixture ${fixtureId}: geometry, placement, explicit targets, counts and deduplication`, () => {
    const { network: n, expected } = createNetworkTopologyFixture(fixtureId);
    const before = toJsonString(NetworkSchema, n);
    const graph = buildNetworkArchitecture(n);
    const nodes = new Map(graph.nodes.map(node => [node.id, node]));
    const resource = (name: string) => graph.nodes.find(node => node.data.resourceId === name)!;
    assert.equal(nodes.size, graph.nodes.length);
    assert.equal(new Set(graph.edges.map(edge => edge.id)).size, graph.edges.length);
    assert.equal(graph.total, Object.values(expected.resourceCounts).reduce((sum, count) => sum + count, 0));
    // Compact attachment badges still represent matched resource identities.
    assert.equal(graph.matched, graph.total - expected.resourceCounts.Route - expected.resourceCounts.RouteTableAssociation);
    for (const [kind, count] of Object.entries(expected.resourceCounts)) {
      const canvas = graph.nodes.filter(node => node.data.kind === kind);
      const supporting = graph.supportingResources.filter(node => node.kind === kind);
      assert.equal(canvas.length + supporting.length, count, `${fixtureId}: every ${kind} retained exactly once`);
      if (["Route", "RouteTableAssociation", "EIP"].includes(kind)) assert.equal(canvas.length, 0);
    }
    assert.equal(graph.nodes.filter(node => node.data.boundary === "zone").length, expected.zoneIds.length);
    for (const node of graph.nodes) {
      assert.ok(node.width > 0 && node.height > 0);
      assert.ok(Number.isFinite(node.position.x) && Number.isFinite(node.position.y));
      if (!node.parentId) continue;
      const parent = nodes.get(node.parentId)!;
      assert.ok(graph.nodes.indexOf(parent) < graph.nodes.indexOf(node));
      assert.ok(node.position.x >= 0 && node.position.y >= (parent.headerHeight ?? 0));
      assert.ok(node.position.x + node.width <= parent.width, `${fixtureId}: ${node.data.kind} exceeds parent width`);
      assert.ok(node.position.y + node.height <= parent.height, `${fixtureId}: ${node.data.kind} exceeds parent height`);
    }
    for (let i = 0; i < graph.nodes.length; i++) {
      const a = graph.nodes[i];
      for (const b of graph.nodes.slice(i + 1)) {
        if (a.parentId !== b.parentId && (a.type === "group" || b.type === "group")) continue;
        assert.equal(overlap(absoluteRect(a, nodes), absoluteRect(b, nodes)), false, `${fixtureId}: ${a.id} overlaps ${b.id}`);
      }
    }
    for (const table of graph.nodes.filter(node => node.data.kind === "RouteTable")) assertVpcOwnership(table, nodes);
    for (const placement of expected.natPlacements) {
      const nat = resource(placement.name);
      assert.equal(nat.parentId, resource(placement.subnet).id);
      const attachment = graph.supportingResources.find(row => row.kind === "EIP" && row.resourceId === placement.address);
      assert.ok(attachment, "attached EIP retains independent inventory identity");
      assert.ok("attachments" in nat.data && Array.isArray(nat.data.attachments));
      assert.equal(nat.data.attachments.length, 1);
      assert.deepEqual(nat.data.attachments[0], attachment);
    }
    for (const nat of expected.unplacedNatNames) assert.equal(resource(nat).parentId, undefined);
    const natEgress = graph.edges.filter(edge => edge.data.relation === "nat_internet_egress");
    assert.equal(natEgress.length, expected.natPlacements.length);
    for (const path of natEgress) {
      assert.ok(expected.natPlacements.some(placement => resource(placement.name).id === path.source));
      assert.equal(nodes.get(path.target)?.data.kind, "InternetGateway");
      assert.equal(path.data.derived, true);
    }
    for (const subnet of expected.subnets) {
      const association = graph.edges.find(edge => edge.source === resource(subnet.name).id && edge.target === resource(subnet.routeTable).id && edge.data.relation === "routes_via");
      assert.equal(!!association, !!subnet.association);
    }
    for (const route of expected.routes) {
      const table = resource(route.routeTable);
      const rows = table.data.routes!.filter(row => row.destination === route.destination);
      if (route.target) {
        const target = route.target.kind === "VPC" ? graph.nodes.find(node => node.data.kind === "Local route destination" && node.parentId === resource(route.target!.name).id)! : resource(route.target.name);
        assert.ok(target, `${fixtureId}: explicit route destination missing`);
        const path = graph.edges.find(edge => edge.source === table.id && edge.target === target.id && edge.label === route.destination && edge.data.relation === "forwards_to");
        assert.ok(path, `${fixtureId}: explicit route target missing`);
        assert.equal(path.data.evidence, route.evidence);
        assert.ok(rows.some(row => row.targetId === target.id));
        assert.equal(path.data.routeIds?.length, 1);
      } else assert.ok(rows.some(row => row.targetKind === "Unresolved" && row.targetId === ""));
    }
    for (const edge of graph.edges) {
      assert.ok(nodes.has(edge.source)); assert.ok(nodes.has(edge.target));
      if ("relationship" in edge.data) {
        assert.ok(["contains", "associated-with", "routes-to", "attached-to", "uses-network"].includes(String(edge.data.relationship)));
        if (edge.data.relation === "routes_via") assert.equal(edge.data.relationship, "associated-with");
        if (edge.data.relation === "forwards_to") assert.equal(edge.data.relationship, "routes-to");
      }
    }
    assert.equal(toJsonString(NetworkSchema, n), before);
    const duplicated = clone(NetworkSchema, n);
    duplicated.status!.architecture!.resources.push(...clone(NetworkSchema, n).status!.architecture!.resources);
    duplicated.status!.architecture!.connections.push(...clone(NetworkSchema, n).status!.architecture!.connections);
    duplicated.status!.managedResources.push(...clone(NetworkSchema, n).status!.managedResources);
    duplicated.status!.architecture!.resources.reverse(); duplicated.status!.architecture!.connections.reverse(); duplicated.status!.managedResources.reverse();
    assert.deepEqual(buildNetworkArchitecture(duplicated), graph);
    const overview = buildNetworkArchitectureOverview(n);
    const localIds = new Set(graph.nodes.filter(node => node.data.kind === "Local route destination").map(node => node.id));
    assert.equal(localIds.size, 1, "one local destination context per explicitly referenced VPC");
    assert.equal(overview.nodes.some(node => localIds.has(node.id)), false);
    assert.equal(overview.edges.some(edge => localIds.has(edge.source) || localIds.has(edge.target)), false);
    assert.deepEqual(overview.edges, graph.edges.filter(edge => !localIds.has(edge.target)));
    assert.deepEqual(overview.nodes.map(node => [node.id, node.data]), graph.nodes.filter(node => !localIds.has(node.id)).map(node => [node.id, node.data]));
    assert.deepEqual(overview.relationships, graph.relationships, "presentation filtering retains semantic relationships");
    assert.deepEqual(overview.supportingResources, graph.supportingResources);
    assert.equal(overview.total, graph.total);
    assert.equal(overview.matched, graph.matched);
    for (const route of expected.routes.filter(route => route.target?.kind === "VPC")) {
      const table = overview.nodes.find(node => node.data.resourceId === route.routeTable)!;
      const fullTable = resource(route.routeTable);
      assert.deepEqual(table.data.routes, fullTable.data.routes);
      assert.ok(table.data.routes?.some(row => row.destination === route.destination && row.targetKind === "VPC" && localIds.has(row.targetId)));
      assert.ok(overview.relationships?.some(relationship => relationship.source === table.id && relationship.target === resource(route.target!.name).id && relationship.type === "routes-to"));
    }
    assert.deepEqual(buildNetworkArchitectureOverview(n, undefined, [], undefined, true), graph, "local-route toggle restores full graph");
  });
}
