import assert from "node:assert/strict";
import test from "node:test";
import { create } from "@bufbuild/protobuf";
import { ClusterSchema, ClusterState } from "../../gen/cloud/v1/cluster_pb";
import { clusterIsConverging } from "./clusters";

test("asynchronous acknowledgement is distinct from observed readiness", () => {
  assert.equal(clusterIsConverging(create(ClusterSchema, { generation: 4n })), true);
  for (const state of [ClusterState.PROVISIONING, ClusterState.UPDATING, ClusterState.DELETING]) {
    assert.equal(clusterIsConverging(create(ClusterSchema, { generation: 4n, status: { state, observedGeneration: 4n } })), true);
  }
  assert.equal(clusterIsConverging(create(ClusterSchema, { generation: 4n, status: { state: ClusterState.RUNNING, observedGeneration: 3n } })), true);
  assert.equal(clusterIsConverging(create(ClusterSchema, { generation: 4n, status: { state: ClusterState.RUNNING, observedGeneration: 4n } })), false);
  assert.equal(clusterIsConverging(create(ClusterSchema, { generation: 4n, status: { state: ClusterState.FAILED, observedGeneration: 3n } })), false);
});
