import assert from "node:assert/strict";
import { test } from "node:test";
import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { ClusterStatusSchema } from "../../gen/cloud/v1/cluster_pb";
import { LiveReadErrorSchema } from "../../gen/cloud/v1/live_pb";
import { ListNodeGroupNodesResponseSchema } from "../../gen/cloud/v1/node_group_nodes_pb";
import { collectionState, isTokenMismatch, liveReadFailure } from "./node-group-nodes";

const at = new Date("2026-11-19T10:00:00Z");

const liveError = (reason: string, message: string, code = Code.Unavailable) =>
  new ConnectError(message, code, undefined, [{ desc: LiveReadErrorSchema, value: { reason, message } }]);

test("a live list says when it was read, not collected", () => {
  const live = collectionState(create(ListNodeGroupNodesResponseSchema, { liveAt: timestampFromDate(at), complete: true, total: 2 }));
  assert.deepEqual([live.live, live.collected, live.collectedLabel, live.incomplete], [true, true, "2026-11-19 10:00:00 UTC", false]);
  const stored = collectionState(create(ListNodeGroupNodesResponseSchema, { collectedAt: timestampFromDate(at), complete: true }));
  assert.equal(stored.live, false);
});

test("a gone live view restarts the list", () => {
  assert.equal(isTokenMismatch(new ConnectError("restart the list", Code.InvalidArgument)), true);
});

test("a failed live read shows its reason and the summary's counts, never rows", () => {
  const status = create(ClusterStatusSchema, { nodes: { total: 3, ready: 2, nodesCollectedAt: timestampFromDate(at) } });
  assert.equal(
    liveReadFailure(liveError("CREDENTIALS_REJECTED", "the platform cannot read this cluster"), status),
    "The workload cluster cannot be reached right now (the platform cannot read this cluster). Showing the counts from the last successful controller pass, 2026-11-19 10:00:00 UTC. Nodes: 2 of 3 ready.",
  );
  assert.match(liveReadFailure(liveError("NO_LIVE_IDENTITY", "the platform has no read-only identity for this cluster"), undefined) ?? "",
    /no read-only identity.*No controller pass has read the cluster yet\./);
  assert.match(liveReadFailure(liveError("AUTHENTICATION_REQUIRED", "live reads need authentication", Code.FailedPrecondition), status) ?? "",
    /^Live reads need authentication\. Showing the counts/);
  assert.equal(liveReadFailure(new ConnectError("boom", Code.Internal), status), undefined);
  assert.equal(liveReadFailure(new Error("x"), status), undefined);
});
