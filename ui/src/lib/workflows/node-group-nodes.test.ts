import assert from "node:assert/strict";
import { test } from "node:test";
import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { ClusterSchema } from "../../gen/cloud/v1/cluster_pb";
import { NodeGroupNodeSchema, NodeGroupPhase, NodeGroupSpecSchema, NodeGroupStatusSchema } from "../../gen/cloud/v1/node_group_pb";
import { ListNodeGroupNodesResponseSchema, NodeStatusFilter } from "../../gen/cloud/v1/node_group_nodes_pb";
import { CapacityType } from "../../gen/cloud/v1/node_pool_pb";
import {
  NODE_PAGE_SIZE, capacityLabel, collectionState, groupDetail, nodeAge, nodeFilterOptions, nodeRow, nodeStatusFilter, pageRange, pagerBack, pagerForward, pagerStart, isTokenMismatch,
} from "./node-group-nodes";
import { ConnectError, Code } from "@connectrpc/connect";

const at = new Date("2026-11-19T10:00:00Z");
const node = (init: Parameters<typeof create<typeof NodeGroupNodeSchema>>[1] = {}) => create(NodeGroupNodeSchema, { name: "ip-10-0-1-1.ec2.internal", ready: true, ...init });

test("pages are of 100 nodes", () => assert.equal(NODE_PAGE_SIZE, 100));

test("the status filter maps to the API's enum, defaulting to all", () => {
  assert.equal(nodeStatusFilter("all"), NodeStatusFilter.ALL);
  assert.equal(nodeStatusFilter("ready"), NodeStatusFilter.READY);
  assert.equal(nodeStatusFilter("not-ready"), NodeStatusFilter.NOT_READY);
  assert.equal(nodeStatusFilter("surprise"), NodeStatusFilter.ALL);
  assert.deepEqual(nodeFilterOptions.map(option => option.label), ["All", "Ready", "Not ready"]);
});

test("a node's age is its largest whole unit", () => {
  const ago = (ms: number) => timestampFromDate(new Date(at.getTime() - ms));
  assert.equal(nodeAge(undefined, at), "—");
  assert.equal(nodeAge(ago(0), at), "0m");
  assert.equal(nodeAge(ago(59_000), at), "0m");
  assert.equal(nodeAge(ago(60_000), at), "1m");
  assert.equal(nodeAge(ago(59 * 60_000), at), "59m");
  assert.equal(nodeAge(ago(3_600_000), at), "1h");
  assert.equal(nodeAge(ago(47 * 3_600_000), at), "47h");
  assert.equal(nodeAge(ago(48 * 3_600_000), at), "2d");
  assert.equal(nodeAge(ago(400 * 86_400_000), at), "400d");
  assert.equal(nodeAge(timestampFromDate(new Date(at.getTime() + 5000)), at), "0m", "a clock ahead of the node");
});

test("capacity is named, and unknown values pass through", () => {
  assert.equal(capacityLabel("spot"), "Spot");
  assert.equal(capacityLabel("on-demand"), "On-demand");
  assert.equal(capacityLabel(""), "—");
  assert.equal(capacityLabel("reserved"), "reserved");
});

test("a ready node is Ready; a not ready one says why", () => {
  const ready = nodeRow(node(), at);
  assert.deepEqual([ready.statusLabel, ready.tone, ready.detail], ["Ready", "success", ""]);
  const bad = nodeRow(node({ ready: false, conditions: ["MemoryPressure", "NetworkUnavailable"] }), at);
  assert.deepEqual([bad.statusLabel, bad.tone, bad.detail], ["Not ready", "error", "MemoryPressure, NetworkUnavailable"]);
  const cordoned = nodeRow(node({ unschedulable: true, taintsCount: 2 }), at);
  assert.equal(cordoned.statusLabel, "Ready");
  assert.equal(cordoned.detail, "Cordoned · 2 taints");
  assert.equal(nodeRow(node({ taintsCount: 1 }), at).detail, "1 taint");
});

test("a row carries the node's columns and its NodeClaim", () => {
  const row = nodeRow(node({
    instanceType: "m7i.large", zone: "us-east-1a", capacityType: "spot", kubeletVersion: "v1.37.1-eks", createdAt: timestampFromDate(new Date(at.getTime() - 3 * 3_600_000)),
    nodeclaimName: "batch-abc", nodeclaimPhase: "Initialized", nodeclaimReady: true, providerId: "aws:///us-east-1a/i-0abc",
  }), at);
  assert.equal(row.id, "ip-10-0-1-1.ec2.internal");
  assert.deepEqual([row.instanceType, row.zone, row.capacity, row.kubeletVersion, row.age], ["m7i.large", "us-east-1a", "Spot", "v1.37.1-eks", "3h"]);
  assert.deepEqual([row.nodeClaim, row.nodeClaimTone], ["batch-abc", "success"]);
  assert.equal(row.nodeClaimDetail, "Initialized");
  const waiting = nodeRow(node({ nodeclaimName: "batch-def", nodeclaimPhase: "Registered" }), at);
  assert.deepEqual([waiting.nodeClaim, waiting.nodeClaimTone, waiting.nodeClaimDetail], ["batch-def", "warning", "Registered"]);
  const none = nodeRow(node(), at);
  assert.deepEqual([none.nodeClaim, none.nodeClaimTone, none.instanceType, none.zone, none.kubeletVersion], ["—", "neutral", "—", "—", "—"]);
});

test("the pager keeps the tokens of the pages it passed", () => {
  let pager = pagerStart();
  assert.deepEqual([pager.page, pager.token, pager.hasPrevious], [1, "", false]);
  pager = pagerForward(pager, "t2");
  assert.deepEqual([pager.page, pager.token, pager.hasPrevious], [2, "t2", true]);
  pager = pagerForward(pager, "t3");
  assert.deepEqual([pager.page, pager.token], [3, "t3"]);
  pager = pagerBack(pager);
  assert.deepEqual([pager.page, pager.token], [2, "t2"]);
  pager = pagerBack(pagerBack(pager));
  assert.deepEqual([pager.page, pager.token, pager.hasPrevious], [1, "", false], "never before the first page");
  assert.equal(pagerForward(pager, "").page, 1, "no next token: no next page");
});

test("the range reads as 'first–last of total'", () => {
  assert.equal(pageRange(1, 100, 100, 1000), "1–100 of 1,000");
  assert.equal(pageRange(10, 100, 100, 1000), "901–1,000 of 1,000");
  assert.equal(pageRange(3, 100, 37, 237), "201–237 of 237");
  assert.equal(pageRange(1, 100, 0, 0), "0 of 0");
});

test("a changed list is a token mismatch, other errors are not", () => {
  assert.equal(isTokenMismatch(new ConnectError("page_token does not match this result; restart the list", Code.InvalidArgument)), true);
  assert.equal(isTokenMismatch(new ConnectError("nope", Code.NotFound)), false);
  assert.equal(isTokenMismatch(new Error("x")), false);
});

test("collection state: never collected, collected, incomplete, with notices", () => {
  const never = collectionState(create(ListNodeGroupNodesResponseSchema, { notices: ["the node list has not been collected yet"] }));
  assert.deepEqual([never.collected, never.collectedLabel, never.incomplete, never.notices], [false, "Not collected yet", false, ["the node list has not been collected yet"]]);
  const ok = collectionState(create(ListNodeGroupNodesResponseSchema, { collectedAt: timestampFromDate(at), complete: true, total: 3 }));
  assert.deepEqual([ok.collected, ok.collectedLabel, ok.incomplete, ok.notices], [true, "2026-11-19 10:00:00 UTC", false, []]);
  const partial = collectionState(create(ListNodeGroupNodesResponseSchema, { collectedAt: timestampFromDate(at), complete: false, notices: ["incomplete"] }));
  assert.equal(partial.incomplete, true);
  // A notice about an unreadable bucket with no collected time is not "incomplete".
  const unreadable = collectionState(create(ListNodeGroupNodesResponseSchema, { notices: ["results unavailable: access to the results bucket was denied"] }));
  assert.equal(unreadable.incomplete, false);
});

test("a group's detail: user groups show their spec, platform groups say who manages them", () => {
  const cluster = create(ClusterSchema, {
    id: "c1", spec: { nodeGroups: [create(NodeGroupSpecSchema, { name: "gpu", instanceFamilies: ["g5"], capacityTypes: [CapacityType.SPOT] })], defaultNodePool: { instanceTypes: ["m6i.large"], scalingConfig: { minSize: 2, maxSize: 2 } } },
    status: { nodeGroups: [
      create(NodeGroupStatusSchema, { name: "platform", platformManaged: true, phase: NodeGroupPhase.READY, nodes: 2 }),
      create(NodeGroupStatusSchema, { name: "gpu", phase: NodeGroupPhase.FAILED, message: "No GPU capacity", nodes: 0 }),
      create(NodeGroupStatusSchema, { name: "system", platformManaged: true, phase: NodeGroupPhase.READY, nodes: 2 }),
    ] },
  });
  const gpu = groupDetail(cluster, "gpu");
  assert.ok(gpu);
  assert.deepEqual([gpu.kind, gpu.platformManaged, gpu.phaseLabel, gpu.tone, gpu.message, gpu.nodes], ["user", false, "Failed", "error", "No GPU capacity", 0]);
  assert.deepEqual(gpu.summary.map(item => item.label), ["Capacity", "Instances", "Architectures", "Labels", "Taints", "Limits"]);
  assert.equal(gpu.summary[0].value, "Spot");
  assert.equal(gpu.summary[1].value, "g5 family");

  const platform = groupDetail(cluster, "platform");
  assert.deepEqual([platform?.kind, platform?.platformManaged, platform?.phaseLabel, platform?.nodes], ["platform", true, "Ready", 2]);
  assert.match(String(platform?.summary[0].value), /Karpenter/);

  const system = groupDetail(cluster, "system");
  assert.deepEqual([system?.kind, system?.platformManaged], ["system", true]);
  assert.equal(system?.summary.find(item => item.label === "Instance types")?.value, "m6i.large");
  assert.equal(system?.summary.find(item => item.label === "Size")?.value, "2 nodes (fixed)");

  assert.equal(groupDetail(cluster, "ghost"), undefined);
});

test("system and platform exist before the controller reports them", () => {
  const cluster = create(ClusterSchema, { id: "c1", spec: { defaultNodePool: { instanceTypes: ["t3.medium"], scalingConfig: { minSize: 1, maxSize: 3 } } } });
  const system = groupDetail(cluster, "system");
  assert.deepEqual([system?.phaseLabel, system?.tone, system?.nodes], ["Not reported", "neutral", undefined]);
  assert.equal(system?.summary.find(item => item.label === "Size")?.value, "1–3 nodes");
  assert.equal(groupDetail(cluster, "platform")?.phaseLabel, "Pending");
});
