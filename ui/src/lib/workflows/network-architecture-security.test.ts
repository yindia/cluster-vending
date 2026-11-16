import assert from "node:assert/strict";
import test from "node:test";
import type { ArchitectureEdge, ArchitectureNode, NetworkArchitectureGraph, TopologyNodeData } from "./network-topology";
import { buildNetworkArchitecture, buildNetworkArchitectureOverview } from "./network-topology";
import { createNetworkTopologyFixture } from "./network-topology-fixtures";
import { architectureSecurityCoverage, inspectArchitectureSecurity } from "./network-architecture-security";

const data = (kind: string, name: string): TopologyNodeData => ({ kind, resourceId: name, graphId: name, title: name, cloudId: `cloud-${name}`, state: "Ready", tone: "success", paused: false, href: "", message: "" });
const node = (kind: string, name: string): ArchitectureNode => ({ id: name, type: "resource", position: { x: 0, y: 0 }, width: 288, height: 136, data: data(kind, name) });
const edge = (source: string, target: string, relation: string, evidence: "configured" | "observed" = "observed", label = ""): ArchitectureEdge => ({ id: JSON.stringify([source, target, relation, evidence]), source, target, label, data: { relation, evidence, derived: false } });
const graph = (nodes: ArchitectureNode[] = [], edges: ArchitectureEdge[] = []): NetworkArchitectureGraph => ({ nodes, edges, supportingResources: [], unresolved: [], total: nodes.length, matched: nodes.length });

test("IGW defaults describe route references, including state, without asserting public accessibility", () => {
  const table = node("RouteTable", "table");
  table.data.routes = [
    { id: "default", destination: "0.0.0.0/0", targetId: "igw", targetKind: "InternetGateway", targetName: "igw", evidence: "observed", message: "blackhole · CreateRoute" },
    { id: "nondefault", destination: "10.2.0.0/16", targetId: "igw", targetKind: "InternetGateway", targetName: "igw", evidence: "observed" },
    { id: "nat", destination: "::/0", targetId: "nat", targetKind: "NATGateway", targetName: "nat", evidence: "configured" },
  ];
  const input = graph([table, node("InternetGateway", "igw"), node("NATGateway", "nat")]);
  const findings = inspectArchitectureSecurity(input);
  assert.equal(findings.length, 1);
  assert.equal(findings[0].title, "Internet gateway default route");
  assert.equal(findings[0].level, "info");
  assert.match(findings[0].detail, /observed/);
  assert.match(findings[0].detail, /blackhole/);
  assert.match(findings[0].detail, /not proof of public exposure or reachability/);
  table.data.routes = [];
  input.edges = [edge("table", "igw", "forwards_to", "configured", "::/0")];
  assert.match(inspectArchitectureSecurity(input)[0].detail, /configured/);
});

test("public role, ready state, IP-looking names and absent data never imply accessibility or isolation", () => {
  const subnet = node("Subnet", "public-203.0.113.5");
  subnet.data.boundary = "public";
  const input = graph([subnet, node("InternetGateway", "igw"), node("NATGateway", "nat")]);
  const findings = inspectArchitectureSecurity(input);
  assert.equal(findings.length, 1);
  assert.equal(findings[0].title, "No default route reported");
  assert.equal(findings[0].level, "info");
  assert.match(findings[0].detail, /Incomplete observations/);
  assert.match(findings[0].detail, /absence and isolation are not established/);
  assert.equal(findings.some(finding => finding.title === "Public address association"), false);
  assert.deepEqual(inspectArchitectureSecurity(graph()), []);
});

test("compact and standalone EIP associations are deduplicated facts, not inbound access claims", () => {
  const nat = node("NATGateway", "nat");
  const allocation = data("EIP", "allocation");
  allocation.title = "203.0.113.9";
  nat.data.attachments = [allocation];
  const input = graph([nat]);
  input.supportingResources = [allocation];
  input.relationships = [{ id: "attachment", source: "nat", target: "allocation", type: "attached-to", evidence: "observed" }];
  let findings = inspectArchitectureSecurity(input);
  assert.equal(findings.length, 1);
  assert.equal(findings[0].nodeId, "nat");
  assert.equal(findings[0].title, "Public address association");
  assert.match(findings[0].detail, /203\.0\.113\.9/);
  assert.match(findings[0].detail, /alone does not establish public exposure, inbound access/);
  nat.data.attachments = [];
  input.nodes.push({ ...node("EIP", "allocation"), data: allocation });
  input.edges.push(edge("nat", "allocation", "uses_address"));
  findings = inspectArchitectureSecurity(input);
  assert.equal(findings.length, 1);
});

test("observed association conflicts differ from configured-versus-observed drift", () => {
  const input = graph([node("Subnet", "subnet"), node("RouteTable", "old"), node("RouteTable", "new")], [edge("subnet", "old", "routes_via"), edge("subnet", "new", "routes_via", "configured")]);
  let findings = inspectArchitectureSecurity(input);
  assert.ok(findings.some(finding => finding.title === "Configured and observed associations differ"));
  assert.equal(findings.some(finding => finding.title === "Ambiguous observed route-table associations"), false);
  assert.match(findings.find(finding => finding.title === "Configured and observed associations differ")!.detail, /not evidence that multiple route tables are currently effective/);
  input.edges.push(edge("subnet", "new", "routes_via"));
  findings = inspectArchitectureSecurity(input);
  assert.equal(findings.filter(finding => finding.title === "Ambiguous observed route-table associations").length, 1);
  assert.match(findings.find(finding => finding.title === "Ambiguous observed route-table associations")!.detail, /simultaneous use is not established/);
  input.edges = [edge("subnet", "old", "routes_via", "configured"), edge("subnet", "new", "routes_via", "configured")];
  findings = inspectArchitectureSecurity(input);
  assert.ok(findings.some(finding => finding.title === "Multiple configured route-table references"));
  assert.equal(findings.some(finding => finding.title === "Ambiguous observed route-table associations"), false);
});

test("association relationship metadata is honored without inventing proximity-based dependencies", () => {
  const table = node("RouteTable", "table");
  table.data.routes = [{ id: "nat-default", destination: "0.0.0.0/0", targetId: "nat", targetKind: "NATGateway", targetName: "nat", evidence: "configured" }];
  const subnet = node("Subnet", "subnet");
  const input = graph([subnet, table]);
  assert.ok(inspectArchitectureSecurity(input).some(finding => finding.title === "No default route reported"));
  input.relationships = [{ id: "association", source: "subnet", target: "table", type: "associated-with", evidence: "configured" }];
  assert.equal(inspectArchitectureSecurity(input).some(finding => finding.title === "No default route reported"), false);
});

test("cross-zone findings require the explicit crossZone flag", () => {
  const table = node("RouteTable", "table"), nat = node("NATGateway", "nat");
  table.data.zoneId = "zone-a"; nat.data.zoneId = "zone-b";
  const route = edge("table", "nat", "forwards_to", "observed", "0.0.0.0/0");
  const input = graph([table, nat], [route]);
  assert.equal(inspectArchitectureSecurity(input).some(finding => finding.title === "Cross-zone route reference"), false);
  route.data.crossZone = true;
  const finding = inspectArchitectureSecurity(input).find(finding => finding.title === "Cross-zone route reference")!;
  assert.ok(finding);
  assert.equal(finding.level, "warning");
  assert.match(finding.detail, /explicitly reported/);
});

test("SG/NACL coverage counts available inventory only and never claims rule or exposure evaluation", () => {
  const sg = node("SecurityGroup", "sg");
  const input = graph([sg]);
  input.supportingResources = [sg.data, data("NetworkACL", "acl"), data("SecurityGroupRule", "rule-inventory-only")];
  assert.deepEqual(architectureSecurityCoverage(input), { securityGroups: 1, networkAcls: 1, rulesReported: false, exposureEvaluated: false, message: "SG/NACL rules not reported; exposure not evaluated." });
  const findings = inspectArchitectureSecurity(input);
  assert.equal(findings.length, 2);
  assert.ok(findings.every(finding => finding.level === "info" && /rules not reported; exposure not evaluated/.test(finding.detail)));
  assert.equal(architectureSecurityCoverage(graph()).securityGroups, 0);
  assert.equal(architectureSecurityCoverage(graph()).exposureEvaluated, false);
});

test("real fixture inspection stays deterministic, immutable, and independent of local-route presentation", () => {
  const { network } = createNetworkTopologyFixture("E");
  const input = buildNetworkArchitecture(network);
  const before = structuredClone(input);
  const findings = inspectArchitectureSecurity(input);
  assert.ok(findings.some(finding => finding.title === "Cross-zone route reference"));
  assert.ok(findings.some(finding => finding.title === "Public address association"));
  assert.equal(findings.filter(finding => finding.title === "Internet gateway default route").length, 3);
  assert.equal(new Set(findings.map(finding => finding.id)).size, findings.length);
  assert.deepEqual(input, before);
  const reversed = structuredClone(input);
  reversed.nodes.reverse(); reversed.edges.reverse(); reversed.supportingResources.reverse(); reversed.relationships?.reverse();
  for (const node of reversed.nodes) { node.data.routes?.reverse(); node.data.attachments?.reverse(); }
  assert.deepEqual(inspectArchitectureSecurity(reversed), findings);
  assert.deepEqual(inspectArchitectureSecurity(buildNetworkArchitectureOverview(network)), findings);
  const isolated = inspectArchitectureSecurity(buildNetworkArchitecture(createNetworkTopologyFixture("F").network));
  assert.equal(isolated.filter(finding => finding.title === "No default route reported").length, 2);
  assert.equal(isolated.some(finding => finding.title === "Public address association"), false);
});
