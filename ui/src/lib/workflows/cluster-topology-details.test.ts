import assert from "node:assert/strict";
import test from "node:test";
import { create } from "@bufbuild/protobuf";
import { ClusterSchema } from "../../gen/cloud/v1/cluster_pb";
import { CapacityType } from "../../gen/cloud/v1/node_pool_pb";
import { clusterComputeDetails, configuredNodePool } from "./cluster-topology-details";
import type { TopologyNodeData } from "./network-topology";
const arn = "arn:aws:eks:us-west-2:123456789012:nodegroup/prod/workers/uuid";
const node: TopologyNodeData = { kind: "NodeGroup", resourceId: "managed-workers", cloudId: arn, title: "Workers", state: "Ready", tone: "success", paused: false, href: "", message: "" };
const fixture = () => create(ClusterSchema, { spec: { defaultNodePool: { instanceTypes: ["m6i.large"], capacityType: CapacityType.SPOT, scalingConfig: { minSize: 0, maxSize: 5, desiredSize: 2 } } }, status: { provider: { case: "aws", value: { nodeGroupArn: arn } }, architecture: { resources: [{ kind: "NodeGroup", name: node.resourceId, cloudId: arn }] } } });
test("default-pool configuration requires an unambiguous observed identity", () => {
  const cluster = fixture();
  assert.equal(configuredNodePool(cluster, node), cluster.spec!.defaultNodePool);
  assert.equal(configuredNodePool(cluster, { ...node, cloudId: "workers" }), cluster.spec!.defaultNodePool);
  for (const changed of [{ ...node, cloudId: "other" }, { ...node, referenceOnly: true }, { ...node, resourceId: "other" }]) assert.equal(configuredNodePool(cluster, changed), undefined);
  cluster.status!.architecture!.resources.push({ ...cluster.status!.architecture!.resources[0], name: "other" });
  assert.equal(configuredNodePool(cluster, node), undefined);
});
test("initial desired size must never masquerade as the live autoscaler target or node count", () => {
  const rows = clusterComputeDetails(fixture(), node);
  assert.equal(rows.find(row => row.label === "Initial desired nodes")!.value, "2");
  assert.equal(rows.find(row => row.label === "Desired nodes")!.value, "Not reported");
  assert.equal(rows.find(row => row.label === "Current nodes")!.value, "Not reported");
  assert.equal(rows.find(row => row.label === "Minimum nodes")!.value, "0");
  assert.equal(rows.find(row => row.label === "Capacity")!.value, "Spot");
});
test("worker metadata is not inherited from the cluster or default node pool", () => {
  const rows = clusterComputeDetails(fixture(), { ...node, kind: "Node", zoneId: "usw2-az1" });
  assert.equal(rows.find(row => row.label === "Instance type")!.value, "Not reported");
  assert.equal(rows.find(row => row.label === "Availability Zone")!.value, "usw2-az1");
});
