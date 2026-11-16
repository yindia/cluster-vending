import assert from "node:assert/strict";
import test from "node:test";
import { fromJsonString, toJsonString } from "@bufbuild/protobuf";
import { ConditionStatus } from "../../gen/cloud/v1/conditions_pb";
import { NetworkSchema, NetworkState, SubnetRole } from "../../gen/cloud/v1/network_pb";
import { createNetworkTopologyFixture, networkTopologyFixtureIds, type NetworkTopologyFixtureId } from "./network-topology-fixtures";

const counts: Record<NetworkTopologyFixtureId, { zones: number; nats: number; subnets: number; tables: number; routes: number; associations: number; defaultEgress: number }> = {
  A: { zones: 2, nats: 1, subnets: 4, tables: 4, routes: 10, associations: 4, defaultEgress: 4 },
  B: { zones: 2, nats: 2, subnets: 4, tables: 4, routes: 10, associations: 4, defaultEgress: 4 },
  C: { zones: 3, nats: 1, subnets: 6, tables: 6, routes: 15, associations: 6, defaultEgress: 6 },
  D: { zones: 3, nats: 3, subnets: 6, tables: 6, routes: 15, associations: 6, defaultEgress: 6 },
  E: { zones: 3, nats: 2, subnets: 6, tables: 6, routes: 15, associations: 6, defaultEgress: 6 },
  F: { zones: 2, nats: 0, subnets: 4, tables: 4, routes: 6, associations: 4, defaultEgress: 2 },
  G: { zones: 3, nats: 3, subnets: 9, tables: 9, routes: 24, associations: 9, defaultEgress: 9 },
  H: { zones: 3, nats: 1, subnets: 6, tables: 4, routes: 9, associations: 6, defaultEgress: 6 },
  I: { zones: 3, nats: 2, subnets: 6, tables: 6, routes: 14, associations: 5, defaultEgress: 5 },
};

for (const id of networkTopologyFixtureIds) {
  test(`fixture ${id} has independently specified scenario counts and complete reference endpoints`, () => {
    const { network, expected } = createNetworkTopologyFixture(id);
    const scenario = counts[id];
    const architecture = network.status!.architecture!;
    assert.equal(expected.zoneIds.length, scenario.zones);
    assert.equal(expected.natNames.length, scenario.nats);
    assert.equal(expected.subnets.length, scenario.subnets);
    assert.equal(expected.routes.length, scenario.routes);
    assert.equal(expected.defaultEgress.length, scenario.defaultEgress);
    assert.deepEqual(expected.resourceCounts, { VPC: 1, InternetGateway: 1, Subnet: scenario.subnets, RouteTable: scenario.tables, RouteTableAssociation: scenario.associations, ...(scenario.nats ? { NATGateway: scenario.nats, EIP: scenario.nats } : {}), Route: scenario.routes - scenario.tables });
    const identities = new Set(architecture.resources.map(resource => `${resource.kind}/${resource.name}`));
    assert.equal(identities.size, architecture.resources.length);
    const edgeIds = new Set<string>();
    for (const connection of architecture.connections) {
      assert.ok(identities.has(`${connection.sourceKind}/${connection.sourceName}`));
      assert.ok(identities.has(`${connection.targetKind}/${connection.targetName}`));
      assert.ok(["observed", "configured"].includes(connection.evidence));
      edgeIds.add(JSON.stringify([connection.sourceKind, connection.sourceName, connection.targetKind, connection.targetName, connection.relation, connection.label, connection.evidence]));
    }
    assert.equal(edgeIds.size, architecture.connections.length);
    assert.equal(network.status!.managedResources.length, architecture.resources.length);
    assert.deepEqual(fromJsonString(NetworkSchema, toJsonString(NetworkSchema, network)), network);
  });
}

test("expected placement, association and egress facts are backed by actual protobuf connections", () => {
  for (const id of networkTopologyFixtureIds) {
    const { network, expected } = createNetworkTopologyFixture(id);
    const { resources, connections } = network.status!.architecture!;
    const has = (source: string, target: string, relation: string) => connections.some(edge => edge.sourceName === source && edge.targetName === target && edge.relation === relation);
    for (const subnet of expected.subnets) {
      const fact = resources.find(resource => resource.name === subnet.name)!;
      assert.equal(fact.zoneId, subnet.zoneId);
      assert.equal(fact.cidrBlock, subnet.cidr);
      assert.equal(fact.subnetRole, subnet.role === "public" ? SubnetRole.PUBLIC : SubnetRole.PRIVATE);
      assert.equal(has(subnet.name, subnet.routeTable, "routes_via"), !!subnet.association);
      if (subnet.association) {
        assert.ok(has(subnet.association, subnet.name, "associated_with"));
        assert.ok(has(subnet.association, subnet.routeTable, "associated_with"));
      }
    }
    for (const placement of expected.natPlacements) {
      assert.ok(has(placement.name, placement.subnet, "placed_in"));
      assert.ok(has(placement.name, placement.address, "uses_address"));
      assert.equal(expected.subnets.find(subnet => subnet.name === placement.subnet)?.zoneId, placement.zoneId);
      assert.equal(expected.subnets.find(subnet => subnet.name === placement.subnet)?.role, "public");
    }
    for (const route of expected.routes) {
      if (route.sourceKind === "Route") assert.ok(has(route.name, route.routeTable, "contained_in"));
      const forward = connections.find(edge => edge.sourceKind === route.sourceKind && edge.sourceName === route.name && edge.relation === "forwards_to" && edge.label === route.destination);
      assert.equal(forward?.targetName, route.target?.name);
      if (forward) { assert.equal(forward.label, route.destination); assert.equal(forward.evidence, route.evidence); }
      if (route.destination === "10.0.0.0/16") {
        assert.equal(route.sourceKind, "RouteTable");
        assert.equal(forward?.targetKind, "VPC", "local route must explicitly reference the actual VPC");
        assert.equal(forward?.routeState, "active");
        assert.equal(forward?.routeOrigin, "CreateRouteTable");
      }
    }
    for (const egress of expected.defaultEgress) {
      const subnet = expected.subnets.find(subnet => subnet.name === egress.subnet)!;
      const route = expected.routes.find(route => route.routeTable === subnet.routeTable && route.destination === "0.0.0.0/0" && route.target?.name === egress.target.name);
      assert.ok(subnet.association && route, `${id}: default egress requires an association and actual default route`);
    }
  }
});

test("mixed, shared-table, multi-private and zero-NAT scenarios exercise distinct structures", () => {
  const mixed = createNetworkTopologyFixture("E").expected;
  const privatePaths = mixed.defaultEgress.filter(path => mixed.subnets.find(subnet => subnet.name === path.subnet)?.role === "private");
  const crossesZone = (subnetName: string, nat: string) => mixed.subnets.find(subnet => subnet.name === subnetName)!.zoneId !== mixed.natPlacements.find(placement => placement.name === nat)!.zoneId;
  assert.equal(privatePaths.filter(path => crossesZone(path.subnet, path.target.name)).length, 1);
  assert.equal(privatePaths.filter(path => !crossesZone(path.subnet, path.target.name)).length, 2);
  const shared = createNetworkTopologyFixture("H").expected.subnets.filter(subnet => subnet.role === "private");
  assert.equal(new Set(shared.map(subnet => subnet.routeTable)).size, 1);
  assert.equal(new Set(shared.map(subnet => subnet.association)).size, 3);
  const multiple = createNetworkTopologyFixture("G").expected;
  for (const zone of multiple.zoneIds) assert.equal(multiple.subnets.filter(subnet => subnet.zoneId === zone && subnet.role === "private").length, 2);
  const isolated = createNetworkTopologyFixture("F").expected;
  assert.equal(isolated.defaultEgress.some(path => isolated.subnets.find(subnet => subnet.name === path.subnet)?.role === "private"), false);
});

test("partial scenario omits unknown relationships and marks unobserved NAT honestly", () => {
  const { network, expected } = createNetworkTopologyFixture("I");
  const architecture = network.status!.architecture!;
  assert.equal(expected.incompleteSubnets.length, 1);
  assert.equal(expected.unplacedNatNames.length, 1);
  assert.equal(network.status!.state, NetworkState.PROVISIONING);
  for (const nat of expected.unplacedNatNames) {
    assert.equal(architecture.connections.some(edge => edge.sourceName === nat && edge.relation === "placed_in"), false);
    assert.equal(architecture.resources.find(resource => resource.name === nat)?.cloudId, "");
    assert.equal(network.status!.managedResources.find(resource => resource.name === nat)?.ready, ConditionStatus.FALSE);
  }
  for (const subnet of expected.incompleteSubnets) {
    assert.equal(architecture.connections.some(edge => edge.sourceName === subnet && edge.relation === "routes_via"), false);
    assert.equal(expected.defaultEgress.some(path => path.subnet === subnet), false);
  }
  assert.ok(expected.defaultEgress.some(path => path.evidence === "configured"));
});

test("factory output is deterministic and independent between calls", () => {
  const first = createNetworkTopologyFixture("A");
  const second = createNetworkTopologyFixture("A");
  assert.deepEqual(first, second);
  first.network.status!.architecture!.resources.length = 0;
  first.expected.subnets[0].name = "changed";
  first.expected.zoneIds.push("changed");
  assert.deepEqual(second, createNetworkTopologyFixture("A"));
  assert.equal(new Set(networkTopologyFixtureIds.map(id => createNetworkTopologyFixture(id).network.id)).size, 9);
});
