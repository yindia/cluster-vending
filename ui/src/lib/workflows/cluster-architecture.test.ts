import assert from "node:assert/strict";
import test from "node:test";
import { create } from "@bufbuild/protobuf";
import { CloudAccountSchema } from "../../gen/cloud/v1/tenant_service_pb";
import { ClusterSchema, type Cluster } from "../../gen/cloud/v1/cluster_pb";
import { ConditionStatus } from "../../gen/cloud/v1/conditions_pb";
import { ManagedResourceState } from "../../gen/cloud/v1/managed_resource_pb";
import { NetworkArchitectureConnectionSchema, NetworkArchitectureResourceSchema, SubnetRole } from "../../gen/cloud/v1/network_pb";
import { createNetworkTopologyFixture } from "./network-topology-fixtures";
import { buildClusterArchitecture, requireClusterTopologyScope } from "./cluster-architecture";
import { absoluteArchitectureNodes, architectureObstacles, routeArchitectureEdges, segmentHitsRectangle } from "./network-edge-routing";

const fact = (kind: string, name: string) => create(NetworkArchitectureResourceSchema, { kind, name, cloudId: `${kind.toLowerCase()}-${name}` });
const connection = (sourceKind: string, sourceName: string, targetKind: string, targetName: string, relation: string, evidence = "observed", label = "") => create(NetworkArchitectureConnectionSchema, { sourceKind, sourceName, targetKind, targetName, relation, evidence, label });
function fixture(workers = 2): Cluster {
  const resources = [fact("Cluster", "control"), fact("SecurityGroup", "cluster-security"), fact("LaunchTemplate", "workers-template"),
    fact("Addon", "cni"), fact("Addon", "dns"), fact("Release", "metrics"), fact("Release", "policy")];
  const connections = [connection("Cluster", "control", "SecurityGroup", "cluster-security", "uses_security_group", "observed", "Security group"),
    ...["cni", "dns"].map(name => connection("Cluster", "control", "Addon", name, "has_addon", "observed", "Managed add-on")),
    ...["metrics", "policy"].map(name => connection("Cluster", "control", "Release", name, "has_component", "configured", "Helm component"))];
  for (let i = 0; i < 3; i++) {
    resources.push(create(NetworkArchitectureResourceSchema, { kind: "Subnet", name: `subnet-${i}`, cloudId: `subnet-${i}`, zoneId: `az-${i}`, subnetRole: SubnetRole.PRIVATE, cidrBlock: `10.0.${i}.0/24` }));
    connections.push(connection("Cluster", "control", "Subnet", `subnet-${i}`, "uses_subnet", "observed", "Control plane subnet"));
  }
  for (let i = 0; i < workers; i++) {
    resources.push(fact("NodeGroup", `workers-${i}`));
    connections.push(connection("Cluster", "control", "NodeGroup", `workers-${i}`, "has_node_group", "observed", "Node group"),
      connection("NodeGroup", `workers-${i}`, "LaunchTemplate", "workers-template", "uses_launch_template", "observed", "Launch template"),
      connection("NodeGroup", `workers-${i}`, "SecurityGroup", "cluster-security", "uses_security_group", "observed", "Security group"));
    for (let j = 0; j < 3; j++) connections.push(connection("NodeGroup", `workers-${i}`, "Subnet", `subnet-${j}`, "uses_subnet", "configured", "Node group subnet"));
  }
  return create(ClusterSchema, { id: "cluster-a", tenantId: "tenant-a", name: "application", spec: { region: "region-1", network: { networkId: "network-a" } }, status: {
    architecture: { resources, connections }, managedResources: resources.map(resource => ({ kind: resource.kind, name: resource.name, externalName: resource.cloudId,
      state: ManagedResourceState.READY, ready: ConditionStatus.TRUE, synced: ConditionStatus.TRUE, generation: 2n, observedGeneration: 2n })),
  } });
}
const nodeFor = (cluster: Cluster, name: string) => buildClusterArchitecture(cluster).inventoryNodes.find(node => node.data.title === name)!;

test("only actual facts and explicit relationships enter the cluster topology", () => {
  const cluster = fixture();
  cluster.status!.managedResources.push({ ...cluster.status!.managedResources[0], kind: "NodeGroup", name: "unrelated-inventory" });
  const graph = buildClusterArchitecture(cluster);
  assert.equal(graph.inventoryNodes.length, cluster.status!.architecture!.resources.length);
  assert.ok(graph.nodes.every(node => ["Cluster", "NodeGroup"].includes(node.data.kind)));
  assert.equal(graph.allEdges.length, cluster.status!.architecture!.connections.length);
  assert.ok(!graph.inventoryNodes.some(node => node.data.title === "unrelated-inventory"));
  assert.ok(graph.nodes.filter(node => node.type === "group").every(node => node.data.kind === "Visual category" && !node.data.resourceId && !node.data.cloudId));
  for (const worker of graph.resources.filter(node => node.data.kind === "NodeGroup")) {
    assert.equal(worker.parentId, undefined);
    assert.equal(graph.allEdges.filter(edge => edge.source === worker.id && edge.data.relation === "uses_subnet").length, 3);
  }
  assert.equal(graph.allEdges.filter(edge => edge.data.relation === "has_component").length, 2);
  assert.ok(graph.allEdges.filter(edge => edge.data.relation === "has_component").every(edge => edge.data.evidence === "configured" && !edge.data.derived));
});

test("missing architecture and empty architecture do not invent a control plane or membership", () => {
  assert.deepEqual(buildClusterArchitecture().nodes, []);
  const cluster = fixture();
  cluster.status!.architecture = undefined;
  assert.equal(buildClusterArchitecture(cluster).available, false);
  assert.equal(buildClusterArchitecture(cluster).nodes.length, 0);
  assert.match(buildClusterArchitecture(cluster).diagnostics.join(" "), /Inventory alone/);
  cluster.status!.architecture = { $typeName: "cloud.v1.ClusterArchitecture", resources: [], connections: [] };
  assert.equal(buildClusterArchitecture(cluster).available, true);
  assert.equal(buildClusterArchitecture(cluster).nodes.length, 0);
});

test("reference-only resources cannot inherit readiness, pause state or managed identity", () => {
  const cluster = fixture();
  const resource = cluster.status!.architecture!.resources.find(resource => resource.kind === "SecurityGroup")!;
  resource.referenceOnly = true;
  cluster.status!.managedResources.find(row => row.kind === "SecurityGroup")!.paused = true;
  const node = nodeFor(cluster, resource.name);
  assert.equal(node.data.state, "Referenced target");
  assert.equal(node.data.tone, "neutral");
  assert.equal(node.data.paused, false);
  assert.equal(node.data.resourceId, "");
  assert.equal(node.data.cloudId, resource.cloudId);
});

test("ready requires current observation, ready/synced conditions and unpaused reconciliation", () => {
  const cluster = fixture(), resource = cluster.status!.managedResources[0];
  assert.equal(nodeFor(cluster, "control").data.tone, "success");
  resource.observedGeneration = 1n;
  assert.equal(nodeFor(cluster, "control").data.state, "Reconciling");
  assert.equal(nodeFor(cluster, "control").data.tone, "warning");
  resource.observedGeneration = resource.generation;
  resource.ready = ConditionStatus.UNSPECIFIED;
  assert.equal(nodeFor(cluster, "control").data.state, "Readiness unconfirmed");
  assert.equal(nodeFor(cluster, "control").data.tone, "warning");
  resource.ready = ConditionStatus.TRUE; resource.synced = ConditionStatus.FALSE;
  assert.equal(nodeFor(cluster, "control").data.tone, "warning");
  resource.synced = ConditionStatus.TRUE; resource.paused = true;
  assert.equal(nodeFor(cluster, "control").data.tone, "warning");
  cluster.status!.managedResources = [];
  assert.equal(nodeFor(cluster, "control").data.state, "Not reported");
  assert.equal(nodeFor(cluster, "control").data.tone, "neutral");
});

test("observed duplicates coalesce but configured/observed target drift remains explicit", () => {
  const cluster = fixture(), architecture = cluster.status!.architecture!;
  const original = architecture.connections[0];
  architecture.connections.push({ ...original, evidence: "configured" });
  architecture.resources.push(fact("SecurityGroup", "previous-security"));
  architecture.connections.push({ ...original, targetName: "previous-security", evidence: "configured" });
  const graph = buildClusterArchitecture(cluster);
  const edges = graph.allEdges.filter(edge => edge.source === nodeFor(cluster, "control").id && edge.data.relation === "uses_security_group");
  assert.equal(edges.length, 2);
  assert.deepEqual(edges.map(edge => edge.data.evidence).sort(), ["configured", "observed"]);
});

test("missing endpoints and unsupported evidence produce diagnostics instead of invented links", () => {
  const cluster = fixture(), architecture = cluster.status!.architecture!;
  architecture.connections.push(connection("Cluster", "control", "Subnet", "missing", "uses_subnet"), connection("Cluster", "control", "Addon", "dns", "has_addon", "guessed"));
  const graph = buildClusterArchitecture(cluster);
  assert.equal(graph.allEdges.length, architecture.connections.length - 2);
  assert.match(graph.diagnostics.join(" "), /Unavailable endpoint/);
  assert.match(graph.diagnostics.join(" "), /Unsupported relationship/);
  assert.ok(!graph.inventoryNodes.some(node => node.data.title === "missing"));
});

test("cluster/tenant guards reject missing and foreign responses", () => {
  const cluster = fixture();
  assert.equal(requireClusterTopologyScope(cluster, "tenant-a", "cluster-a"), cluster);
  for (const [candidate, tenant, id] of [[undefined, "tenant-a", "cluster-a"], [cluster, "tenant-b", "cluster-a"], [cluster, "tenant-a", "cluster-b"], [cluster, "", "cluster-a"]] as const) {
    assert.throws(() => requireClusterTopologyScope(candidate, tenant, id), /did not match/);
  }
});

test("kind/name identities survive cloud-ID collisions and input reordering without mutation", () => {
  const cluster = fixture();
  cluster.status!.architecture!.resources.push({ ...fact("NodeGroup", "other-workers"), cloudId: "nodegroup-workers-0" });
  const before = structuredClone(cluster), graph = buildClusterArchitecture(cluster);
  assert.equal(new Set(graph.resources.map(node => node.id)).size, graph.resources.length);
  const reversed = structuredClone(cluster);
  reversed.status!.architecture!.resources.reverse(); reversed.status!.architecture!.connections.reverse(); reversed.status!.managedResources.reverse();
  assert.deepEqual(buildClusterArchitecture(reversed), graph);
  assert.deepEqual(cluster, before);
  const otherTenant = buildClusterArchitecture({ ...cluster, tenantId: "tenant-b" });
  assert.ok(otherTenant.resources.every(node => !graph.resources.some(other => other.id === node.id)));
});

type Rect = { x: number; y: number; width: number; height: number };
const overlaps = (a: Rect, b: Rect) => a.x < b.x + b.width && a.x + a.width > b.x && a.y < b.y + b.height && a.y + a.height > b.y;
function assertGeometry(graph: ReturnType<typeof buildClusterArchitecture>) {
  const boxes = absoluteArchitectureNodes(graph.nodes);
  for (const node of graph.nodes) {
    assert.ok(node.width > 0 && node.height > 0);
    if (node.parentId) {
      const parent = graph.nodes.find(parent => parent.id === node.parentId)!;
      assert.ok(node.position.y >= parent.headerHeight!);
      assert.ok(node.position.x >= 0 && node.position.x + node.width <= parent.width);
      assert.ok(node.position.y + node.height <= parent.height);
    }
  }
  const leaves = graph.nodes.filter(node => node.type !== "group");
  for (let i = 0; i < leaves.length; i++) for (const other of leaves.slice(i + 1)) assert.equal(overlaps(boxes.get(leaves[i].id)!, boxes.get(other.id)!), false);
  const routes = routeArchitectureEdges(graph.nodes, graph.edges);
  assert.equal(routes.length, graph.edges.length, "every recorded relationship must route");
  const labels: Rect[] = [];
  for (const edge of routes) {
    const { points, label } = edge.data.route;
    const obstacles = architectureObstacles(graph.nodes, edge);
    for (let i = 1; i < points.length; i++) {
      assert.ok(points[i].x === points[i - 1].x || points[i].y === points[i - 1].y, "orthogonal segments");
      for (const obstacle of obstacles) assert.equal(segmentHitsRectangle(points[i - 1], points[i], obstacle), false, `${edge.id} hits ${obstacle.nodeId}`);
    }
    if (!edge.label) continue;
    const rect = { ...label, x: label.x - label.width / 2, y: label.y - label.height / 2 };
    for (const obstacle of obstacles) assert.equal(overlaps(rect, obstacle), false, "label overlaps obstacle");
    for (const previous of labels) assert.equal(overlaps(rect, previous), false, "labels overlap");
    assert.ok(points.slice(1).some((p, i) => p.y === points[i].y && label.y === p.y && rect.x >= Math.min(p.x, points[i].x) && rect.x + rect.width <= Math.max(p.x, points[i].x)), "label stays on own horizontal segment");
    for (const other of routes) if (other.id !== edge.id) for (let i = 1; i < other.data.route.points.length; i++) assert.equal(segmentHitsRectangle(other.data.route.points[i - 1], other.data.route.points[i], { ...rect, nodeId: "label" }), false, "another edge crosses label");
    labels.push(rect);
  }
}
for (const workers of [1, 3]) test(`${workers} node groups spanning three subnets with SG, template, add-ons and components route safely`, () => {
  assertGeometry(buildClusterArchitecture(fixture(workers)));
});

test("measured 240-unit node-group cards wrap horizontally without collision", () => {
  const cluster = fixture(7), initial = buildClusterArchitecture(cluster);
  const dimensions = new Map(initial.nodes.map(node => [node.id, { width: 340, height: node.data.kind === "NodeGroup" ? 240 : 180 }]));
  const graph = buildClusterArchitecture(cluster, dimensions);
  const workers = graph.nodes.filter(node => node.data.kind === "NodeGroup");
  assert.equal(new Set(workers.map(node => node.position.y)).size, 3);
  assert.equal(new Set(workers.slice(0, 3).map(node => node.position.y)).size, 1);
  assert.ok(workers.every(node => node.height === 240));
  assertGeometry(graph);
});

function overlayFixture(id: Parameters<typeof createNetworkTopologyFixture>[0], workers = 2) {
  const cluster = fixture(workers), { network, expected } = createNetworkTopologyFixture(id);
  network.tenantId = cluster.tenantId;
  cluster.spec!.network!.networkId = network.id;
  const networkSubnets = network.status!.architecture!.resources.filter(resource => resource.kind === "Subnet" && resource.subnetRole === SubnetRole.PRIVATE);
  const refs = cluster.status!.architecture!.resources.filter(resource => resource.kind === "Subnet");
  refs.forEach((resource, index) => {
    const actual = networkSubnets[index % networkSubnets.length];
    resource.cloudId = actual.cloudId; resource.zoneId = actual.zoneId; resource.cidrBlock = actual.cidrBlock;
    resource.referenceOnly = true;
  });
  return { cluster, network, expected };
}

for (const count of [0, 1, 4]) test(`strict default with ${count} node groups has no network/add-on/dependency nodes`, () => {
  const cluster = fixture(count), graph = buildClusterArchitecture(cluster);
  assert.equal(graph.nodes.length, count + 1);
  assert.ok(graph.nodes.every(node => ["Cluster", "NodeGroup"].includes(node.data.kind)));
  assert.equal(graph.edges.length, count);
  assert.ok(graph.edges.every(edge => edge.label === ""));
  assert.ok(graph.inventoryNodes.some(node => node.data.kind === "Release"));
  assert.ok(graph.allEdges.some(edge => edge.data.relation === "uses_subnet"));
  assertGeometry(graph);
});

test("many hidden add-ons cannot change default compute spacing", () => {
  const cluster = fixture(3), before = buildClusterArchitecture(cluster);
  for (let i = 0; i < 100; i++) {
    cluster.status!.architecture!.resources.push(fact("Addon", `hidden-${i}`));
    cluster.status!.architecture!.connections.push(connection("Cluster", "control", "Addon", `hidden-${i}`, "has_addon"));
  }
  const after = buildClusterArchitecture(cluster);
  assert.deepEqual(after.nodes, before.nodes);
  assert.deepEqual(after.edges, before.edges);
});

test("network-off retains shared subnet and launch-template inspection metadata without affecting layout", () => {
  const { cluster, network } = overlayFixture("A");
  const off = buildClusterArchitecture(cluster), supplied = buildClusterArchitecture(cluster, undefined, { showNetwork: false, network });
  assert.deepEqual(supplied, off);
  for (const worker of off.resources.filter(node => node.data.kind === "NodeGroup")) {
    assert.equal(off.allEdges.filter(edge => edge.source === worker.id && edge.data.relation === "uses_subnet").length, 3);
    const template = off.allEdges.find(edge => edge.source === worker.id && edge.data.relation === "uses_launch_template")!;
    assert.equal(off.inventoryNodes.find(node => node.id === template.target)?.data.kind, "LaunchTemplate");
  }
});

for (const id of ["A", "D", "I"] as const) test(`overlay ${id}: shared NAT / AZ-local / partial facts preserve canonical nodes and safe routes`, () => {
  const { cluster, network, expected } = overlayFixture(id);
  const initial = buildClusterArchitecture(cluster);
  const dimensions = new Map(initial.nodes.map(node => [node.id, { width: 320, height: node.data.kind === "NodeGroup" ? 240 : 128 }]));
  const graph = buildClusterArchitecture(cluster, dimensions, { showNetwork: true, network });
  const control = graph.nodes.find(node => node.data.kind === "Cluster")!;
  const boxes = absoluteArchitectureNodes(graph.nodes);
  assert.equal(control.parentId, undefined);
  for (const vpc of graph.nodes.filter(node => node.data.kind === "VPC")) assert.ok(control.position.y + control.height < boxes.get(vpc.id)!.y);
  assert.equal(graph.nodes.filter(node => node.data.kind === "NodeGroup").length, 2, "one node group across all AZs");
  assert.equal(graph.nodes.filter(node => node.data.kind === "NATGateway").length, expected.natNames.length);
  assert.ok(graph.nodes.every(node => !["Addon", "Release", "LaunchTemplate"].includes(node.data.kind)));
  for (const subnet of network.status!.architecture!.resources.filter(resource => resource.kind === "Subnet")) assert.equal(graph.nodes.filter(node => node.data.kind === "Subnet" && node.data.cloudId === subnet.cloudId).length, 1);
  const links = graph.edges.filter(edge => edge.data.relation === "uses_subnet");
  for (const edge of links) {
    assert.equal(graph.nodes.find(node => node.id === edge.target)?.data.kind, "Subnet");
    const full = graph.allEdges.find(candidate => candidate.id === edge.id)!;
    assert.equal(edge.id, JSON.stringify([full.source, full.target, full.data.relation, full.label]));
  }
  const networkTop = Math.min(...graph.nodes.filter(node => !node.parentId && node.data.kind !== "Cluster" && node.data.kind !== "NodeGroup" && node.data.kind !== "SecurityGroup").map(node => node.position.y));
  for (const reference of graph.nodes.filter(node => node.data.kind === "SecurityGroup")) assert.ok(reference.position.y + reference.height < networkTop, "unplaced references stay above network context");
  for (const nat of graph.nodes.filter(node => node.data.kind === "NATGateway")) for (const attachment of nat.data.attachments ?? []) assert.ok(graph.inventoryNodes.some(node => node.id === attachment.graphId), "EIP badges resolve in inspector");
  assertGeometry(graph);
});

test("missing or foreign network context cannot leak into cluster visualization", () => {
  const { cluster, network } = overlayFixture("A");
  for (const supplied of [undefined, { ...network, tenantId: "other" }, { ...network, id: "other" }]) {
    const graph = buildClusterArchitecture(cluster, undefined, { showNetwork: true, network: supplied });
    assert.ok(graph.nodes.every(node => ["Cluster", "NodeGroup"].includes(node.data.kind)));
    assert.match(graph.diagnostics.join(" "), /Network (architecture|overlay)/);
    assertGeometry(graph);
  }
});

test("same-name cloud-ID drift is not merged into a different physical subnet", () => {
  const { cluster, network } = overlayFixture("A");
  const reference = cluster.status!.architecture!.resources.find(resource => resource.kind === "Subnet")!;
  const oldName = reference.name;
  reference.name = network.status!.architecture!.resources.find(resource => resource.kind === "Subnet")!.name;
  reference.cloudId = "subnet-different-physical-target";
  for (const edge of cluster.status!.architecture!.connections) if (edge.targetKind === "Subnet" && edge.targetName === oldName) edge.targetName = reference.name;
  const graph = buildClusterArchitecture(cluster, undefined, { showNetwork: true, network });
  assert.ok(graph.nodes.some(node => node.data.cloudId === reference.cloudId));
  assert.match(graph.diagnostics.join(" "), /cloud identity differs/);
  assertGeometry(graph);
});

test("actual worker facts use explicit node-group membership and subnet placement only", () => {
  const { cluster, network } = overlayFixture("A");
  const architecture = cluster.status!.architecture!;
  architecture.resources.push(fact("Instance", "actual-worker"), fact("WorkerNode", "self-managed-worker"));
  architecture.connections.push(connection("NodeGroup", "workers-0", "Instance", "actual-worker", "has_node"), connection("Instance", "actual-worker", "Subnet", "subnet-0", "placed_in"));
  const off = buildClusterArchitecture(cluster);
  const nodeGroup = off.nodes.find(node => node.data.title === "workers-0")!;
  const actual = off.nodes.find(node => node.data.title === "actual-worker")!;
  assert.ok(actual.position.y > nodeGroup.position.y + nodeGroup.height);
  assert.equal(off.nodes.filter(node => node.data.kind === "NodeGroup").length, 2);
  assert.equal(off.nodes.find(node => node.data.title === "self-managed-worker")?.parentId, undefined);
  assertGeometry(off);
  const on = buildClusterArchitecture(cluster, undefined, { showNetwork: true, network });
  const placed = on.nodes.find(node => node.data.title === "actual-worker")!;
  assert.equal(on.nodes.find(node => node.id === placed.parentId)?.data.kind, "Subnet");
  assert.ok(on.nodes.filter(node => node.data.kind === "NodeGroup").every(node => !node.parentId));
  assertGeometry(on);
});

test("all supported actual worker kinds appear, while reference-only workers and desired counts do not", () => {
  const cluster = fixture(0);
  for (const kind of ["Node", "Instance", "EC2Instance", "KarpenterNode", "WorkerNode"]) cluster.status!.architecture!.resources.push(fact(kind, `actual-${kind}`));
  cluster.status!.architecture!.resources.push(create(NetworkArchitectureResourceSchema, { kind: "Node", name: "unobserved", referenceOnly: true, cloudId: "unobserved" }));
  const graph = buildClusterArchitecture(cluster);
  assert.equal(graph.nodes.length, 6);
  assert.ok(!graph.nodes.some(node => node.data.title === "unobserved"));
  assert.equal(graph.nodes.filter(node => node.data.kind === "NodeGroup").length, 0);
  assertGeometry(graph);
});


test("ambiguous known/unknown worker placement stays outside subnet boundaries", () => {
  const { cluster, network } = overlayFixture("A");
  cluster.status!.architecture!.resources.push(fact("Instance", "actual"), create(NetworkArchitectureResourceSchema, { kind: "Subnet", name: "unknown-subnet", cloudId: "unknown-subnet", referenceOnly: true }));
  cluster.status!.architecture!.connections.push(connection("Instance", "actual", "Subnet", "subnet-0", "placed_in", "observed"), connection("Instance", "actual", "Subnet", "unknown-subnet", "uses_subnet", "configured"));
  const graph = buildClusterArchitecture(cluster, undefined, { showNetwork: true, network });
  assert.equal(graph.nodes.find(node => node.data.title === "actual")!.parentId, undefined);
  assert.match(graph.diagnostics.join(" "), /Ambiguous subnet placement/);
  assertGeometry(graph);
});

test("account overlay is scoped and keeps the control plane outside every container", () => {
  const { cluster, network } = overlayFixture("A");
  network.spec!.bootstrapId = "account-a";
  const account = create(CloudAccountSchema, { id: "account-a", tenantId: cluster.tenantId, accountName: "Actual account" });
  const graph = buildClusterArchitecture(cluster, undefined, { showNetwork: true, network, account });
  assert.ok(graph.nodes.some(node => node.data.boundary === "account"));
  assert.equal(graph.nodes.find(node => node.data.kind === "Cluster")!.parentId, undefined);
  assertGeometry(graph);
  const foreign = buildClusterArchitecture(cluster, undefined, { showNetwork: true, network, account: { ...account, tenantId: "other" } });
  assert.ok(!foreign.nodes.some(node => node.data.boundary === "account"));
  assert.match(foreign.diagnostics.join(" "), /Cloud account context did not match/);
});
