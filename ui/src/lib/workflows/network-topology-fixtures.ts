/** Synthetic protobuf fixtures for topology tests/previews; never live API data. */
import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { ConditionStatus } from "../../gen/cloud/v1/conditions_pb";
import { ManagedResourceSchema, ManagedResourceState } from "../../gen/cloud/v1/managed_resource_pb";
import { NetworkArchitectureConnectionSchema, NetworkArchitectureResourceSchema, NetworkArchitectureSchema, NetworkSchema, NetworkState, SubnetRole, type Network } from "../../gen/cloud/v1/network_pb";

export const networkTopologyFixtureIds = ["A", "B", "C", "D", "E", "F", "G", "H", "I"] as const;
export type NetworkTopologyFixtureId = typeof networkTopologyFixtureIds[number];
type Evidence = "observed" | "configured";
type Target = { kind: "NATGateway" | "InternetGateway" | "VPC"; name: string };
export type TopologyFixtureSubnet = { name: string; zoneId: string; role: "public" | "private"; cidr: string; routeTable: string; association?: string };
export type TopologyFixtureRoute = { name: string; sourceKind: "Route" | "RouteTable"; routeTable: string; destination: string; target?: Target; evidence: Evidence };
export type NetworkTopologyFixture = {
  network: Network;
  expected: {
    id: NetworkTopologyFixtureId; title: string; zoneIds: string[];
    subnets: TopologyFixtureSubnet[];
    natPlacements: { name: string; subnet: string; zoneId: string; address: string }[];
    natNames: string[];
    routes: TopologyFixtureRoute[];
    defaultEgress: { subnet: string; target: Target; evidence: Evidence }[];
    incompleteSubnets: string[]; unplacedNatNames: string[];
    resourceCounts: Record<string, number>;
  };
};

const scenarios: Record<NetworkTopologyFixtureId, { zones: number; nats: number; privatePerZone?: number; sharedTable?: boolean; partial?: boolean; title: string }> = {
  A: { zones: 2, nats: 1, title: "Two zones, one shared NAT" },
  B: { zones: 2, nats: 2, title: "Two zones, one NAT per zone" },
  C: { zones: 3, nats: 1, title: "Three zones, one shared NAT" },
  D: { zones: 3, nats: 3, title: "Three zones, one NAT per zone" },
  E: { zones: 3, nats: 2, title: "Three zones, two NATs with local and cross-zone egress" },
  F: { zones: 2, nats: 0, title: "Two zones, isolated private subnets without NAT" },
  G: { zones: 3, nats: 3, privatePerZone: 2, title: "Three zones, two private subnets per zone" },
  H: { zones: 3, nats: 1, sharedTable: true, title: "Three zones, shared private route table and NAT" },
  I: { zones: 3, nats: 2, partial: true, title: "Three zones, partially observed placement and routes" },
};

/** Each call returns independent protobuf messages and independent expected facts.
 * Local routes are observed inline RouteTable -> VPC entries, without an
 * invented Route CR. Partial I omits references rather than dangling endpoints. */
export function createNetworkTopologyFixture(id: NetworkTopologyFixtureId): NetworkTopologyFixture {
  const scenario = scenarios[id];
  const prefix = `fixture-${id.toLowerCase()}`;
  const resources: MessageInitShape<typeof NetworkArchitectureResourceSchema>[] = [];
  const connections: MessageInitShape<typeof NetworkArchitectureConnectionSchema>[] = [];
  const expected: NetworkTopologyFixture["expected"] = {
    id, title: scenario.title, zoneIds: Array.from({ length: scenario.zones }, (_, index) => `usw2-az${index + 1}`),
    subnets: [], natPlacements: [], natNames: [], routes: [], defaultEgress: [], incompleteSubnets: [], unplacedNatNames: [], resourceCounts: {},
  };
  const name = (suffix: string) => `${prefix}-${suffix}`;
  const fact = (kind: string, suffix: string, extra: MessageInitShape<typeof NetworkArchitectureResourceSchema> = {}) => {
    const resourceName = name(suffix);
    resources.push({ kind, name: resourceName, cloudId: `${kind.toLowerCase()}-${id.toLowerCase()}-${resources.length + 1}`, ...extra });
    return resourceName;
  };
  const link = (sourceKind: string, sourceName: string, targetKind: string, targetName: string, relation: string, label: string, evidence: Evidence = "observed") => {
    connections.push({ sourceKind, sourceName, targetKind, targetName, relation, label, evidence });
  };
  const vpc = fact("VPC", "vpc", { cidrBlock: "10.0.0.0/16" });
  const contained = (kind: string, resource: string) => link(kind, resource, "VPC", vpc, "contained_in", "VPC");
  const igw = fact("InternetGateway", "igw"); contained("InternetGateway", igw);
  const tables = new Set<string>();
  const table = (suffix: string) => {
    const tableName = name(suffix);
    if (!tables.has(tableName)) { fact("RouteTable", suffix); contained("RouteTable", tableName); tables.add(tableName); }
    return tableName;
  };
  const route = (suffix: string, routeTable: string, destination: string, target?: Target, evidence: Evidence = "observed") => {
    const routeName = fact("Route", suffix, { destinationCidrBlock: destination });
    link("Route", routeName, "RouteTable", routeTable, "contained_in", "Route table", evidence);
    if (target) link("Route", routeName, target.kind, target.name, "forwards_to", destination, evidence);
    expected.routes.push({ name: routeName, sourceKind: "Route", routeTable, destination, target, evidence });
  };
  for (let zone = 0; zone < scenario.zones; zone++) {
    for (let ordinal = 0; ordinal <= (scenario.privatePerZone ?? 1); ordinal++) {
      const isPublic = ordinal === 0;
      const suffix = `${isPublic ? "public" : "private"}-${zone + 1}${!isPublic && scenario.privatePerZone ? `-${ordinal}` : ""}`;
      const cidr = `10.0.${zone * 16 + ordinal}.0/24`;
      const subnetName = fact("Subnet", suffix, { zoneId: expected.zoneIds[zone], subnetRole: isPublic ? SubnetRole.PUBLIC : SubnetRole.PRIVATE, cidrBlock: cidr });
      contained("Subnet", subnetName);
      const routeTable = table(scenario.sharedTable && !isPublic ? "private-shared-table" : `${suffix}-table`);
      const incomplete = !!scenario.partial && !isPublic && zone === scenario.zones - 1;
      let association: string | undefined;
      if (!incomplete) {
        association = fact("RouteTableAssociation", `${suffix}-association`);
        link("RouteTableAssociation", association, "Subnet", subnetName, "associated_with", "Subnet");
        link("RouteTableAssociation", association, "RouteTable", routeTable, "associated_with", "Route table");
        link("Subnet", subnetName, "RouteTable", routeTable, "routes_via", "Route table association");
      } else expected.incompleteSubnets.push(subnetName);
      expected.subnets.push({ name: subnetName, zoneId: expected.zoneIds[zone], role: isPublic ? "public" : "private", cidr, routeTable, association });
    }
  }
  for (let index = 0; index < scenario.nats; index++) {
    const partial = !!scenario.partial && index === scenario.nats - 1;
    const nat = fact("NATGateway", `nat-${index + 1}`, partial ? { cloudId: "" } : {});
    const address = fact("EIP", `eip-${index + 1}`, { publicIp: partial ? "" : `203.0.113.${10 + index}` });
    expected.natNames.push(nat);
    contained("NATGateway", nat);
    link("NATGateway", nat, "EIP", address, "uses_address", "Egress address", partial ? "configured" : "observed");
    const subnet = expected.subnets.find(subnet => subnet.role === "public" && subnet.zoneId === expected.zoneIds[index])!;
    if (partial) expected.unplacedNatNames.push(nat);
    else {
      link("NATGateway", nat, "Subnet", subnet.name, "placed_in", "NAT placement");
      expected.natPlacements.push({ name: nat, subnet: subnet.name, zoneId: subnet.zoneId, address });
    }
  }
  for (const routeTable of tables) {
    const subnets = expected.subnets.filter(subnet => subnet.routeTable === routeTable);
    const first = subnets[0];
    const zoneIndex = expected.zoneIds.indexOf(first.zoneId);
    const suffix = routeTable.slice(prefix.length + 1);
    connections.push({ sourceKind: "RouteTable", sourceName: routeTable, targetKind: "VPC", targetName: vpc, relation: "forwards_to", label: "10.0.0.0/16", evidence: "observed", routeState: "active", routeOrigin: "CreateRouteTable" });
    expected.routes.push({ name: routeTable, sourceKind: "RouteTable", routeTable, destination: "10.0.0.0/16", target: { kind: "VPC", name: vpc }, evidence: "observed" });
    let target: Target | undefined;
    const evidence: Evidence = scenario.partial && first.role === "private" && zoneIndex === 1 ? "configured" : "observed";
    if (first.role === "public") target = { kind: "InternetGateway", name: igw };
    else if (scenario.nats) target = { kind: "NATGateway", name: expected.natNames[zoneIndex % scenario.nats] };
    const incomplete = !!scenario.partial && first.role === "private" && zoneIndex === scenario.zones - 1;
    if (target || incomplete) route(`${suffix}-default`, routeTable, "0.0.0.0/0", incomplete ? undefined : target, evidence);
    if (target && !incomplete) {
      for (const subnet of subnets.filter(subnet => subnet.association)) expected.defaultEgress.push({ subnet: subnet.name, target, evidence });
      // One additional explicit non-default route exercises labels/filtering.
      if (first.role === "private") route(`${suffix}-service`, routeTable, "10.240.0.0/16", target, evidence);
    }
  }
  const architecture = create(NetworkArchitectureSchema, { resources, connections });
  for (const resource of architecture.resources) expected.resourceCounts[resource.kind] = (expected.resourceCounts[resource.kind] ?? 0) + 1;
  const network = create(NetworkSchema, {
    id: `00000000-0000-4000-8000-${String(networkTopologyFixtureIds.indexOf(id) + 1).padStart(12, "0")}`,
    tenantId: "topology-fixture", name: prefix, displayName: scenario.title, generation: 1n,
    spec: { region: "us-west-2", bootstrapId: "00000000-0000-4000-8000-000000000100" },
    status: { state: scenario.partial ? NetworkState.PROVISIONING : NetworkState.READY, observedGeneration: 1n, architecture,
      managedResources: architecture.resources.map(resource => {
        const pending = scenario.partial && expected.unplacedNatNames.includes(resource.name);
        return create(ManagedResourceSchema, { kind: resource.kind, name: resource.name, externalName: resource.cloudId, generation: 1n, observedGeneration: pending ? 0n : 1n,
          ready: pending ? ConditionStatus.FALSE : ConditionStatus.TRUE, synced: pending ? ConditionStatus.FALSE : ConditionStatus.TRUE,
          state: pending ? ManagedResourceState.CREATING : ManagedResourceState.READY, message: pending ? "Waiting for NAT placement observation" : "" });
      }),
    },
  });
  return { network, expected };
}
