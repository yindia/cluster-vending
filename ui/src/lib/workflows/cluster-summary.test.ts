import assert from "node:assert/strict";
import { test } from "node:test";
import { create } from "@bufbuild/protobuf";
import { ClusterStatusSchema } from "../../gen/cloud/v1/cluster_pb";
import { NodeGroupPhase, NodeGroupStatusSchema } from "../../gen/cloud/v1/node_group_pb";
import { ConditionStatus } from "../../gen/cloud/v1/conditions_pb";
import {
  clusterCapacity, clusterSummaryView, formatCpu, formatMemory, nodeGroupSummaryRows, nodeGroupTotals, reachabilityView, severityTone, share, utilizationTone,
} from "./cluster-summary";

const now = Date.UTC(2026, 10, 20, 12, 0, 0);
const ago = (minutes: number) => ({ seconds: BigInt(Math.floor((now - minutes * 60_000) / 1000)), nanos: 0 });

test("a cluster no pass has read shows not read yet, never zeros", () => {
  const view = clusterSummaryView(create(ClusterStatusSchema, {}), now);
  assert.equal(view.state, "not-read");
  assert.equal(view.notice, undefined);
  assert.deepEqual(view.items.map(item => item.value), ["Not read yet", "Not read yet", "Not read yet"]);
});

test("a read cluster shows its counts with their ages and marks old ones", () => {
  const status = create(ClusterStatusSchema, {
    nodes: { total: 3, ready: 2, notReady: 1, nodesCollectedAt: ago(4) },
    violations: { total: 5, bySeverity: [{ name: "high", count: 5 }], policyCollectedAt: ago(40) },
    violationEvents: { total: 1, byReason: [{ name: "PolicyViolation", count: 1 }], policyCollectedAt: ago(20) },
    reachable: { status: ConditionStatus.TRUE, lastSuccessfulAt: ago(4) },
  });
  const view = clusterSummaryView(status, now);
  assert.equal(view.state, "read");
  assert.equal(view.notice, undefined);
  const [nodes, violations, events] = view.items;
  assert.equal(nodes.value, "2 / 3 ready");
  assert.equal(nodes.note, "1 not ready · Collected 4 min ago");
  assert.equal(nodes.old, false);
  assert.equal(violations.value, "5");
  assert.equal(violations.note, "Collected 40 min ago · old");
  assert.deepEqual(violations.breakdown?.map(c => [c.name, c.count]), [["high", 5]]);
  assert.equal(violations.old, true);
  assert.equal(events.old, false);
});

test("an unreachable cluster keeps the last counts, marks them old and says why", () => {
  const status = create(ClusterStatusSchema, {
    nodes: { total: 3, ready: 3, nodesCollectedAt: ago(16) },
    reachable: { status: ConditionStatus.FALSE, reason: "CREDENTIALS_REJECTED", lastSuccessfulAt: ago(16) },
  });
  const view = clusterSummaryView(status, now);
  assert.equal(view.state, "unreachable");
  assert.equal(view.items[0].value, "3 / 3 ready");
  assert.equal(view.items[0].old, true);
  assert.match(view.notice ?? "", /cannot be reached right now \(CREDENTIALS_REJECTED\)\. Showing the counts from the last successful controller pass, 2026-11-20T11:44:00\.000Z\./);
});

test("node group totals are absent until the node pass read the group", () => {
  assert.equal(nodeGroupTotals(create(NodeGroupStatusSchema, { name: "batch" })), undefined);
  const totals = nodeGroupTotals(create(NodeGroupStatusSchema, {
    name: "system", readyNodes: 2, allocatableCpuMillis: 4000n, requestedCpuMillis: 900n,
    allocatableMemoryBytes: 8n * 2n ** 30n, requestedMemoryBytes: 2n * 2n ** 30n, podCount: 7,
  }));
  assert.deepEqual(totals, { cpu: "900m / 4 cores (22%)", memory: "2.0 GiB / 8.0 GiB (25%)", pods: "7" });
});

const GiB = 2n ** 30n;
const systemGroup = () => create(NodeGroupStatusSchema, {
  name: "system", platformManaged: true, phase: NodeGroupPhase.READY, readyNodes: 2, notReadyNodes: 1,
  allocatableCpuMillis: 4000n, requestedCpuMillis: 3000n, allocatableMemoryBytes: 8n * GiB, requestedMemoryBytes: 2n * GiB,
  allocatablePods: 58n, podCount: 20, podsWithoutRequests: 2,
});
const appsGroup = () => create(NodeGroupStatusSchema, {
  name: "apps", phase: NodeGroupPhase.READY, readyNodes: 1,
  allocatableCpuMillis: 4000n, requestedCpuMillis: 1000n, allocatableMemoryBytes: 8n * GiB, requestedMemoryBytes: 6n * GiB,
  allocatablePods: 58n, podCount: 9, podsWithoutRequests: 1,
});

test("cluster capacity sums the read node groups with percentages", () => {
  const status = create(ClusterStatusSchema, {
    nodes: { total: 4, ready: 3, notReady: 1, nodesCollectedAt: ago(2) },
    nodeGroups: [systemGroup(), appsGroup(), create(NodeGroupStatusSchema, { name: "batch", phase: NodeGroupPhase.PENDING })],
  });
  const capacity = clusterCapacity(status, now);
  assert.ok(capacity);
  assert.equal(capacity.groups, 3);
  assert.equal(capacity.readGroups, 2);
  assert.equal(capacity.platformGroups, 1);
  assert.deepEqual(capacity.cpu, { requested: "4", allocatable: "8 cores", percent: 50, text: "4 / 8 cores (50%)" });
  assert.equal(capacity.memory?.text, "8.0 GiB / 16.0 GiB (50%)");
  assert.equal(capacity.pods?.text, "29 / 116 (25%)");
  assert.equal(capacity.podCount, 29);
  assert.equal(capacity.podsWithoutRequests, 3);
  assert.equal(capacity.old, false);
});

test("cluster capacity is unknown, not zero, until a group is read, and old with the node counts", () => {
  assert.equal(clusterCapacity(undefined, now), undefined);
  assert.equal(clusterCapacity(create(ClusterStatusSchema, { nodeGroups: [create(NodeGroupStatusSchema, { name: "batch" })] }), now), undefined);
  const old = clusterCapacity(create(ClusterStatusSchema, { nodes: { total: 1, ready: 1, nodesCollectedAt: ago(20) }, nodeGroups: [appsGroup()] }), now);
  assert.equal(old?.old, true);
  const noAllocatable = clusterCapacity(create(ClusterStatusSchema, { nodeGroups: [create(NodeGroupStatusSchema, { name: "x", readyNodes: 1, podCount: 3 })] }), now);
  assert.equal(noAllocatable?.cpu, undefined);
  assert.equal(noAllocatable?.memory, undefined);
  assert.equal(noAllocatable?.pods, undefined);
  assert.equal(noAllocatable?.podCount, 3);
});

test("shares, formats and meter tones", () => {
  assert.equal(share(5n, 0n, String), undefined);
  assert.equal(share(1n, 3n, String)?.percent, 33);
  assert.equal(formatCpu(250n), "250m");
  assert.equal(formatCpu(3500n), "3.5");
  assert.equal(formatCpu(16000n), "16");
  assert.equal(formatMemory(512n * 2n ** 20n), "512 MiB");
  assert.equal(formatMemory(3n * GiB / 2n), "1.5 GiB");
  assert.equal(formatMemory(2048n * GiB), "2.0 TiB");
  assert.equal(utilizationTone(10), "accent");
  assert.equal(utilizationTone(75), "warning");
  assert.equal(utilizationTone(90), "error");
  assert.equal(severityTone("Critical"), "error");
  assert.equal(severityTone("medium"), "warning");
  assert.equal(severityTone("low"), "neutral");
});

test("node group rows put platform groups first and leave unread ones without resources", () => {
  const rows = nodeGroupSummaryRows([appsGroup(), create(NodeGroupStatusSchema, { name: "batch", phase: NodeGroupPhase.PENDING, nodes: 2 }), systemGroup()]);
  assert.deepEqual(rows.map(row => row.name), ["system", "apps", "batch"]);
  const [system, , batch] = rows;
  assert.equal(system.nodes, "2 / 3 ready");
  assert.equal(system.notReady, 1);
  assert.equal(system.phaseLabel, "Ready");
  assert.equal(system.tone, "success");
  assert.equal(system.cpu?.percent, 75);
  assert.equal(system.memory?.percent, 25);
  assert.equal(system.pods?.allocatable, "58");
  assert.equal(system.podsWithoutRequests, 2);
  assert.equal(batch.read, false);
  assert.equal(batch.nodes, "2 nodes");
  assert.equal(batch.cpu, undefined);
  assert.equal(batch.phaseLabel, "Pending");
});

test("reachability says reachable, unreachable with the reason, or not read yet", () => {
  assert.deepEqual(reachabilityView(undefined, now), { value: "Not read yet", note: "Checked on every controller pass", tone: "neutral" });
  assert.deepEqual(reachabilityView(create(ClusterStatusSchema, { reachable: { status: ConditionStatus.TRUE, lastSuccessfulAt: ago(3) } }), now),
    { value: "Reachable", note: "Last read 3 min ago", tone: "success" });
  assert.deepEqual(reachabilityView(create(ClusterStatusSchema, { reachable: { status: ConditionStatus.TRUE, lastSuccessfulAt: ago(30) } }), now),
    { value: "Reachable", note: "Last read 30 min ago · old", tone: "warning" });
  assert.deepEqual(reachabilityView(create(ClusterStatusSchema, { reachable: { status: ConditionStatus.FALSE, reason: "CLUSTER_UNREACHABLE", lastSuccessfulAt: ago(120) } }), now),
    { value: "Unreachable", note: "CLUSTER_UNREACHABLE · Last read 2 h ago", tone: "error" });
});

test("breakdowns list the largest counts first", () => {
  const view = clusterSummaryView(create(ClusterStatusSchema, {
    violationEvents: { total: 4, byReason: [{ name: "FailedAdmission", count: 1 }, { name: "PolicyViolation", count: 3 }], policyCollectedAt: ago(1) },
  }), now);
  assert.deepEqual(view.items[2].breakdown?.map(c => c.name), ["PolicyViolation", "FailedAdmission"]);
});
