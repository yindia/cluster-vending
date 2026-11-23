import assert from "node:assert/strict";
import { test } from "node:test";
import { create } from "@bufbuild/protobuf";
import { ClusterStatusSchema } from "../../gen/cloud/v1/cluster_pb";
import { NodeGroupStatusSchema } from "../../gen/cloud/v1/node_group_pb";
import { ConditionStatus } from "../../gen/cloud/v1/conditions_pb";
import { clusterSummaryView, nodeGroupTotals } from "./cluster-summary";

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
  assert.equal(nodes.note, "Collected 4 min ago");
  assert.equal(nodes.old, false);
  assert.equal(violations.value, "5");
  assert.equal(violations.note, "high 5 · Collected 40 min ago · old");
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
  assert.deepEqual(totals, { cpu: "900m / 4 (22%)", memory: "2.0 GiB / 8.0 GiB (25%)", pods: "7" });
});
