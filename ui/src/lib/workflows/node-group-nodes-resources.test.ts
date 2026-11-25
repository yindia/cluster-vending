import assert from "node:assert/strict";
import { test } from "node:test";
import { create } from "@bufbuild/protobuf";
import { NodeGroupNodeSchema, NodeGroupStatusSchema } from "../../gen/cloud/v1/node_group_pb";
import { cores, cpuShare, gibibytes, groupResources, isInfoNotice, memoryShare, nodeCount, nodeRow, podShare } from "./node-group-nodes";

const at = new Date("2026-11-19T10:00:00Z");
const GiB = 2 ** 30;
const node = (init: Parameters<typeof create<typeof NodeGroupNodeSchema>>[1] = {}) => create(NodeGroupNodeSchema, { name: "n1", ready: true, ...init });

test("cores and GiB drop trailing zeros", () => {
  assert.equal(cores(1930n), "1.93");
  assert.equal(cores(2000n), "2");
  assert.equal(cores(1250), "1.25");
  assert.equal(gibibytes(BigInt(3 * GiB)), "3");
  assert.equal(gibibytes(3452448768n), "3.2");
});

test("a share is requested of allocatable, and nothing when nothing is allocatable", () => {
  assert.deepEqual(cpuShare(1250n, 1930n), { requested: 1250, allocatable: 1930, percent: 65, label: "1.25 of 1.93 cores", short: "1.25 / 1.93 cores" });
  assert.equal(memoryShare(BigInt(GiB), BigInt(4 * GiB))?.label, "1 of 4 GiB");
  assert.equal(podShare(16, 17n)?.label, "16 of 17 pods");
  assert.equal(cpuShare(100n, 0n), undefined);
});

test("a row shows CPU, memory and pods with their detail", () => {
  const row = nodeRow(node({
    architecture: "amd64", allocatableCpuMillis: 1930n, requestedCpuMillis: 1250n, allocatableMemoryBytes: BigInt(4 * GiB), requestedMemoryBytes: BigInt(GiB),
    allocatablePods: 17, podCount: 16, podsWithoutRequests: 6,
  }), at);
  assert.deepEqual([row.cpu, row.cpuDetail, row.memory, row.memoryDetail], ["65%", "1.25 / 1.93 cores", "25%", "1 / 4 GiB"]);
  assert.deepEqual([row.pods, row.podsDetail, row.architecture], ["16 of 17", "6 without requests", "amd64"]);
  const used = nodeRow(node({ allocatableCpuMillis: 2000n, requestedCpuMillis: 500n, usedCpuMillis: 300n, allocatableMemoryBytes: BigInt(2 * GiB), usedMemoryBytes: BigInt(GiB) }), at);
  assert.equal(used.cpuDetail, "0.5 / 2 cores · 0.3 used");
  assert.equal(used.memoryDetail, "0 / 2 GiB · 1 GiB used");
  const bare = nodeRow(node(), at);
  assert.deepEqual([bare.cpu, bare.cpuDetail, bare.memory, bare.pods, bare.podsDetail], ["—", "", "—", "—", ""]);
});

test("the node count is the unfiltered list's total, else the controller's", () => {
  assert.equal(nodeCount(5, true, false, 0), 5, "the live list wins over a stale report of 0");
  assert.equal(nodeCount(1, true, true, 5), 5, "a filtered total is not the group's");
  assert.equal(nodeCount(undefined, false, false, 3), 3);
  assert.equal(nodeCount(0, false, false, undefined), undefined, "not collected and not reported");
});

test("group resources: summed from the whole group, else the controller's report, else nothing", () => {
  const nodes = [
    node({ ready: true, allocatableCpuMillis: 2000n, requestedCpuMillis: 1000n, allocatableMemoryBytes: BigInt(4 * GiB), requestedMemoryBytes: BigInt(GiB), allocatablePods: 17, podCount: 10, podsWithoutRequests: 2 }),
    node({ name: "n2", ready: false, allocatableCpuMillis: 2000n, requestedCpuMillis: 500n, allocatableMemoryBytes: BigInt(4 * GiB), requestedMemoryBytes: BigInt(GiB), allocatablePods: 17, podCount: 6, podsWithoutRequests: 1 }),
  ];
  const status = create(NodeGroupStatusSchema, { name: "g", readyNodes: 3, notReadyNodes: 0, allocatableCpuMillis: 6000n, requestedCpuMillis: 3000n, allocatablePods: 51n, podCount: 20, podsWithoutRequests: 4 });
  const live = groupResources(status, nodes);
  assert.deepEqual([live?.source, live?.ready, live?.notReady, live?.podCount, live?.podsWithoutRequests], ["live", 1, 1, 16, 3]);
  assert.deepEqual([live?.cpu?.label, live?.cpu?.percent, live?.memory?.label, live?.pods?.label], ["1.5 of 4 cores", 38, "2 of 8 GiB", "16 of 34 pods"]);

  const reported = groupResources(status, undefined);
  assert.deepEqual([reported?.source, reported?.ready, reported?.cpu?.label, reported?.memory, reported?.pods?.label], ["reported", 3, "3 of 6 cores", undefined, "20 of 51 pods"]);
  assert.equal(groupResources(create(NodeGroupStatusSchema, { name: "g" }), []), undefined, "an empty report");
  assert.equal(groupResources(undefined, undefined), undefined);
});

test("a usage notice is informational", () => {
  assert.equal(isInfoNotice("node usage unavailable: metrics.k8s.io is not served"), true);
  assert.equal(isInfoNotice("the node list is incomplete"), false);
});
