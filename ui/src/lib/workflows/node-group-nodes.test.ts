import assert from "node:assert/strict";
import { test } from "node:test";
import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { ClusterSchema } from "../../gen/cloud/v1/cluster_pb";
import { NodeGroupNodeSchema, NodeGroupPhase, NodeGroupStatusSchema } from "../../gen/cloud/v1/node_group_pb";
import { ListNodeGroupNodesResponseSchema, NodeStatusFilter } from "../../gen/cloud/v1/node_group_nodes_pb";
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

test("a group's detail is read-only: the platform's groups say who manages them, without settings", () => {
  const cluster = create(ClusterSchema, {
    id: "c1",
    status: { nodeGroups: [
      create(NodeGroupStatusSchema, { name: "platform-pool", platformManaged: true, phase: NodeGroupPhase.FAILED, message: "No capacity", nodes: 0 }),
      create(NodeGroupStatusSchema, { name: "system", platformManaged: true, phase: NodeGroupPhase.READY, nodes: 2 }),
      create(NodeGroupStatusSchema, { name: "batch", phase: NodeGroupPhase.DELETING, message: "draining", nodes: 1 }),
    ] },
  });
  const pool = groupDetail(cluster, "platform-pool");
  assert.ok(pool);
  assert.deepEqual([pool.kind, pool.platformManaged, pool.phaseLabel, pool.tone, pool.message, pool.nodes], ["pool", true, "Failed", "error", "No capacity", 0]);
  assert.match(pool.about, /Karpenter NodePool the platform creates/);
  assert.equal("summary" in pool, false, "no settings to show");

  const system = groupDetail(cluster, "system");
  assert.deepEqual([system?.kind, system?.platformManaged, system?.phaseLabel, system?.nodes], ["system", true, "Ready", 2]);
  assert.match(system?.about ?? "", /Karpenter and the EKS add-ons/);
  assert.match(system?.about ?? "", /static size/);
  assert.match(system?.about ?? "", /platform's configuration/);
  assert.doesNotMatch(system?.about ?? "", /cluster autoscaler|autoscaled by/);
  assert.equal(system?.observed?.nodes, 2);

  // A group an earlier layout left behind is shown while it drains, without a platform explanation.
  const batch = groupDetail(cluster, "batch");
  assert.deepEqual([batch?.kind, batch?.platformManaged, batch?.phaseLabel, batch?.about], ["other", false, "Deleting", ""]);

  // "platform" is no node group any more: it is reserved, and unknown unless a status still says so.
  assert.equal(groupDetail(cluster, "platform"), undefined);
  assert.equal(groupDetail(cluster, "ghost"), undefined);
});

test("system exists before the controller reports it", () => {
  const cluster = create(ClusterSchema, { id: "c1" });
  const system = groupDetail(cluster, "system");
  assert.deepEqual([system?.phaseLabel, system?.tone, system?.nodes], ["Not reported", "neutral", undefined]);
  assert.equal(system?.observed, undefined, "no report before the controller makes one");
  assert.equal(groupDetail(cluster, "platform-pool"), undefined, "the platform pool resolves once the controller reports it");
});
