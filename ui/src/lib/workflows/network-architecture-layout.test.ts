import assert from "node:assert/strict";
import test from "node:test";
import { create } from "@bufbuild/protobuf";
import { NetworkArchitectureConnectionSchema, NetworkArchitectureResourceSchema, SubnetRole } from "../../gen/cloud/v1/network_pb";
import { createNetworkTopologyFixture, networkTopologyFixtureIds } from "./network-topology-fixtures";
import { buildNetworkArchitectureOverview, type ArchitectureNode } from "./network-topology";

function assertPacking(nodes: ArchitectureNode[]) {
  const map = new Map(nodes.map(n => [n.id, n]));
  for (const node of nodes) {
    if (!node.parentId) continue;
    const parent = map.get(node.parentId)!;
    assert.ok(node.position.x >= 0 && node.position.y >= (parent.headerHeight ?? 0));
    assert.ok(node.position.x + node.width <= parent.width, `${node.id} exceeds width`);
    assert.ok(node.position.y + node.height <= parent.height, `${node.id} exceeds height`);
  }
  for (let i = 0; i < nodes.length; i++) for (let j = i + 1; j < nodes.length; j++) {
    const a = nodes[i], b = nodes[j];
    if (a.parentId !== b.parentId) continue;
    const intersects = a.position.x < b.position.x + b.width && a.position.x + a.width > b.position.x && a.position.y < b.position.y + b.height && a.position.y + a.height > b.position.y;
    assert.equal(intersects, false, `${a.id} intersects ${b.id}`);
  }
}
for (const id of networkTopologyFixtureIds) test(`measured content grows containers without sibling collisions in ${id}`, () => {
  const network = createNetworkTopologyFixture(id).network;
  const initial = buildNetworkArchitectureOverview(network);
  const measurements = new Map(initial.nodes.map((n, i) => [n.id, { width: n.type === "group" ? n.width : 380 + i % 3 * 70, height: n.type === "group" ? n.height : 180 + i % 4 * 40, headerHeight: 140 }]));
  const expanded = buildNetworkArchitectureOverview(network, undefined, [], measurements);
  assertPacking(expanded.nodes);
  for (const node of expanded.nodes.filter(n => n.type !== "group")) {
    assert.ok(node.width >= measurements.get(node.id)!.width);
    assert.ok(node.height >= measurements.get(node.id)!.height);
  }
  assert.deepEqual(buildNetworkArchitectureOverview(network, undefined, [], measurements), expanded);
  assert.deepEqual(expanded.edges, initial.edges, "measurements must not alter relationships");
});
test("AZ packing supports one and five zones with multiple public subnets", () => {
  for (const count of [1, 5]) {
    const network = createNetworkTopologyFixture("A").network;
    const facts = network.status!.architecture!;
    const vpc = facts.resources.find(r => r.kind === "VPC")!;
    const table = facts.resources.find(r => r.kind === "RouteTable")!;
    for (const subnet of facts.resources.filter(r => r.kind === "Subnet")) subnet.zoneId = "zone-1";
    for (let i = 2; i <= count; i++) for (let ordinal = 0; ordinal < 2; ordinal++) {
      const name = `zone-${i}-public-${ordinal}`;
      facts.resources.push(create(NetworkArchitectureResourceSchema, { kind: "Subnet", name, cloudId: `subnet-${i}-${ordinal}`, zoneId: `zone-${i}`, subnetRole: SubnetRole.PUBLIC, cidrBlock: `10.${i}.${ordinal}.0/24` }));
      facts.connections.push(create(NetworkArchitectureConnectionSchema, { sourceKind: "Subnet", sourceName: name, targetKind: "VPC", targetName: vpc.name, relation: "contained_in", evidence: "observed" }), create(NetworkArchitectureConnectionSchema, { sourceKind: "Subnet", sourceName: name, targetKind: "RouteTable", targetName: table.name, relation: "routes_via", evidence: "observed" }));
    }
    const graph = buildNetworkArchitectureOverview(network);
    assert.equal(graph.nodes.filter(n => n.data.boundary === "zone").length, count);
    assertPacking(graph.nodes);
    const nat = graph.nodes.find(n => n.data.kind === "NATGateway")!;
    assert.equal(graph.nodes.find(n => n.id === nat.parentId)!.data.zoneId, "zone-1");
  }
});

test("AWS account wraps the region using scoped ownership without altering routes", async () => {
  const { CloudAccountSchema } = await import("../../gen/cloud/v1/tenant_service_pb");
  const network = createNetworkTopologyFixture("C").network;
  const account = create(CloudAccountSchema, {
    id: network.spec!.bootstrapId, tenantId: network.tenantId,
    accountName: "Production", provider: { case: "aws", value: { accountId: "123456789012" } },
  });
  const baseline = buildNetworkArchitectureOverview(network);
  const graph = buildNetworkArchitectureOverview(network, account);
  const owner = graph.nodes.find(n => n.data.boundary === "account")!;
  assert.ok(owner);
  assert.equal(owner.data.title, "Production");
  assert.equal(owner.data.cloudId, "123456789012");
  assert.equal(graph.nodes.find(n => n.data.boundary === "region")!.parentId, owner.id);
  assert.deepEqual(graph.edges, baseline.edges);
  assert.equal(graph.total, baseline.total, "account context must not inflate provisioned resources");
  assertPacking(graph.nodes);
  for (const rejected of [{ ...account, tenantId: "other" }, { ...account, id: "other" }]) {
    assert.equal(buildNetworkArchitectureOverview(network, rejected).nodes.some(n => n.data.boundary === "account"), false);
  }
});

test("account ownership does not invent placement or ownership of external targets", async () => {
  const { CloudAccountSchema } = await import("../../gen/cloud/v1/tenant_service_pb");
  const fixture = createNetworkTopologyFixture("I"), network = fixture.network;
  network.status!.architecture!.resources.push(create(NetworkArchitectureResourceSchema, {
    kind: "TransitGateway", name: "external", cloudId: "tgw-0123456789abcdef0", referenceOnly: true,
  }));
  const account = create(CloudAccountSchema, { id: network.spec!.bootstrapId, tenantId: network.tenantId,
    provider: { case: "aws", value: { accountId: "123456789012" } } });
  const graph = buildNetworkArchitectureOverview(network, account);
  const owner = graph.nodes.find(n => n.data.boundary === "account")!;
  for (const name of fixture.expected.unplacedNatNames) {
    assert.equal(graph.nodes.find(n => n.data.resourceId === name)!.parentId, owner.id);
  }
  assert.equal(graph.nodes.find(n => n.data.referenceOnly)!.parentId, undefined);
  assertPacking(graph.nodes);
});
