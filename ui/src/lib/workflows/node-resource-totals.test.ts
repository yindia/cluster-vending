import assert from "node:assert/strict";
import test from "node:test";
import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { LiveReadErrorSchema } from "../../gen/cloud/v1/live_pb";
import { GetNodeResourcesResponseSchema } from "../../gen/cloud/v1/node_resources_pb";
import { LiveUnavailableDetailSchema, LiveUnavailableReason } from "../../gen/cloud/v1/sort_pb";
import { LIVE_READER_OFF, nodeTotalsFailure, nodeTotalsOf } from "./node-resource-totals";

const GiB = 2n ** 30n;
const response = create(GetNodeResourcesResponseSchema, {
  liveAt: timestampFromDate(new Date("2026-11-27T10:00:00Z")), complete: true, notices: ["node usage unavailable: no metrics API"],
  groups: [{ nodeGroup: "system", nodes: 3, readyNodes: 2, allocatableCpuMillis: 6000n, requestedCpuMillis: 1500n, allocatableMemoryBytes: 12n * GiB, requestedMemoryBytes: 3n * GiB, allocatablePods: 90n, podCount: 30n, podsWithoutRequests: 4n, usedCpuMillis: 600n }],
});

test("a group's live totals are shares of what its nodes allocate", () => {
  const state = nodeTotalsOf(response, "system");
  assert.equal(state.kind, "ready");
  if (state.kind !== "ready") return;
  assert.equal(state.liveAt, "2026-11-27 10:00:00 UTC");
  assert.deepEqual([state.totals.nodes, state.totals.ready, state.totals.notReady], [3, 2, 1]);
  assert.equal(state.totals.cpu?.label, "1.5 of 6 cores"); assert.equal(state.totals.cpu?.percent, 25);
  assert.equal(state.totals.memory?.label, "3 of 12 GiB");
  assert.equal(state.totals.pods?.label, "30 of 90 pods"); assert.equal(state.totals.podsWithoutRequests, 4);
  assert.equal(state.totals.usedCpu?.percent, 10); assert.equal(state.totals.usedMemory, undefined);
  assert.deepEqual(state.notices, ["node usage unavailable: no metrics API"]);
});

test("a group the live read does not list has no nodes now", () => {
  assert.deepEqual(nodeTotalsOf(response, "workers"), { kind: "empty", liveAt: "2026-11-27 10:00:00 UTC" });
});

test("no live reader, or an unreadable cluster, explains why", () => {
  assert.equal(nodeTotalsFailure(new ConnectError("not yet", Code.Unimplemented)), LIVE_READER_OFF);
  const unavailable = new ConnectError("down", Code.Unavailable, undefined, [{ desc: LiveUnavailableDetailSchema, value: { reason: LiveUnavailableReason.CLUSTER_UNREACHABLE, message: "the cluster could not be reached" } }]);
  assert.equal(nodeTotalsFailure(unavailable), "Live totals are unavailable right now: the cluster could not be reached.");
  const reader = new ConnectError("down", Code.Unavailable, undefined, [{ desc: LiveReadErrorSchema, value: { reason: "LIVE_READER_UNAVAILABLE" } }]);
  assert.match(nodeTotalsFailure(reader) ?? "", /live reader cannot be reached/);
  const busy = new ConnectError("busy", Code.Unavailable, undefined, [{ desc: LiveReadErrorSchema, value: { reason: "BUSY", message: "too many reads" } }]);
  assert.equal(nodeTotalsFailure(busy), "The workload cluster cannot be read live right now (too many reads).");
  assert.equal(nodeTotalsFailure(new ConnectError("denied", Code.PermissionDenied)), undefined);
  assert.equal(nodeTotalsFailure(new Error("plain")), undefined);
});
