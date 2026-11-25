import assert from "node:assert/strict";
import test from "node:test";
import { create } from "@bufbuild/protobuf";
import { ClusterSchema } from "../../gen/cloud/v1/cluster_pb";
import { PLATFORM_NODE_SOURCE, clusterComputeDetails } from "./cluster-topology-details";
import type { TopologyNodeData } from "./network-topology";
const arn = "arn:aws:eks:us-west-2:123456789012:nodegroup/prod/workers/uuid";
const node: TopologyNodeData = { kind: "NodeGroup", resourceId: "managed-workers", cloudId: arn, title: "Workers", state: "Ready", tone: "success", paused: false, href: "", message: "" };
const fixture = () => create(ClusterSchema, { status: { provider: { case: "aws", value: { nodeGroupArn: arn } }, architecture: { resources: [{ kind: "NodeGroup", name: node.resourceId, cloudId: arn }] } } });
test("node groups and Karpenter pools say the platform manages them, with no settings to show", () => {
  for (const kind of ["NodeGroup", "NodePool"]) {
    assert.deepEqual(clusterComputeDetails(fixture(), { ...node, kind }), [{ label: "Managed by", value: "The platform", source: PLATFORM_NODE_SOURCE }]);
  }
});
test("worker metadata is not inherited from the cluster", () => {
  const rows = clusterComputeDetails(fixture(), { ...node, kind: "Node", zoneId: "usw2-az1" });
  assert.equal(rows.find(row => row.label === "Instance type")!.value, "Not reported");
  assert.equal(rows.find(row => row.label === "Availability Zone")!.value, "usw2-az1");
});
