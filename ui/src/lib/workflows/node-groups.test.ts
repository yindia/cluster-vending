import assert from "node:assert/strict";
import { test } from "node:test";
import { create } from "@bufbuild/protobuf";
import { NodeGroupPhase, NodeGroupStatusSchema } from "../../gen/cloud/v1/node_group_pb";
import { NodeGroupManifestSchema } from "../../gen/cloud/v1/node_group_manifest_pb";
import { PLATFORM_POOL, SYSTEM_GROUP, hasManifestYaml, nodeGroupRows, phaseDisplay } from "./node-groups";

const status = (name: string, phase: NodeGroupPhase, extra = {}) => create(NodeGroupStatusSchema, { name, phase, ...extra });
const platform = (name: string, phase: NodeGroupPhase, extra = {}) => status(name, phase, { platformManaged: true, ...extra });
const manifest = (name: string, platformManaged = false) => create(NodeGroupManifestSchema, { name, platformManaged, ec2NodeClassYaml: `kind: EC2NodeClass\nname: ${name}\n`, nodePoolYaml: `kind: NodePool\nname: ${name}\n` });

test("rows list the system group, then the Karpenter platform pool, whatever order the status reports them in", () => {
  const rows = nodeGroupRows([platform(PLATFORM_POOL, NodeGroupPhase.READY, { nodes: 4 }), platform(SYSTEM_GROUP, NodeGroupPhase.READY, { nodes: 2 })]);
  assert.deepEqual(rows.map(row => [row.name, row.platformManaged, row.phaseLabel, row.nodes]), [[SYSTEM_GROUP, true, "Ready", 2], [PLATFORM_POOL, true, "Ready", 4]]);
  assert.equal(rows[1].tone, "success");
  assert.equal(rows[0].observed, true);
});

test("rows are read-only: they carry no spec and no edit affordance", () => {
  const [row] = nodeGroupRows([platform(SYSTEM_GROUP, NodeGroupPhase.READY)]);
  assert.equal("spec" in row, false);
  assert.equal("editable" in row, false);
});

test("the system row is always there until the manifests say the platform manages no node groups", () => {
  // Nothing reported and nothing known yet: the system group is Pending, waiting for the controller.
  const rows = nodeGroupRows([]);
  assert.deepEqual(rows.map(row => [row.name, row.platformManaged, row.phaseLabel]), [[SYSTEM_GROUP, true, "Pending"]]);
  assert.match(rows[0].message, /Pending — waiting for the controller/);
  assert.equal(rows[0].observed, false);
  // An imported cluster: the manifests are known and have no platform entry.
  assert.deepEqual(nodeGroupRows([], []), []);
});

test("a platform pool in the manifests that the status does not list yet is a Pending platform row with its YAML", () => {
  const rows = nodeGroupRows([platform(SYSTEM_GROUP, NodeGroupPhase.READY)], [create(NodeGroupManifestSchema, { name: SYSTEM_GROUP, platformManaged: true }), manifest(PLATFORM_POOL, true)]);
  assert.deepEqual(rows.map(row => [row.name, row.platformManaged, row.phaseLabel, row.manifest?.name]), [[SYSTEM_GROUP, true, "Ready", undefined], [PLATFORM_POOL, true, "Pending", PLATFORM_POOL]]);
  assert.match(rows[1].manifest!.nodePoolYaml, /kind: NodePool/);
});

test("a platform group an earlier controller recorded as \"platform\" is not a row", () => {
  const rows = nodeGroupRows([platform("platform", NodeGroupPhase.READY, { nodes: 1 }), platform(SYSTEM_GROUP, NodeGroupPhase.READY, { nodes: 2 })]);
  assert.deepEqual(rows.map(row => row.name), [SYSTEM_GROUP]);
});

test("a group an earlier layout left behind stays visible after the platform's groups while it drains", () => {
  const rows = nodeGroupRows([status("batch", NodeGroupPhase.DELETING, { message: "draining" }), platform(SYSTEM_GROUP, NodeGroupPhase.READY)]);
  assert.deepEqual(rows.map(row => [row.name, row.platformManaged, row.phaseLabel, row.message]), [[SYSTEM_GROUP, true, "Ready", ""], ["batch", false, "Deleting", "draining"]]);
});

test("status reporting tolerates unknown phases, repeated names and a missing node count", () => {
  const [unknown] = nodeGroupRows([platform(SYSTEM_GROUP, 99 as NodeGroupPhase, { message: "from a newer controller" })]);
  assert.deepEqual([unknown.phaseLabel, unknown.tone, unknown.message], ["Unknown (99)", "neutral", "from a newer controller"]);
  const [unspecified] = nodeGroupRows([platform(SYSTEM_GROUP, NodeGroupPhase.UNSPECIFIED)]);
  assert.deepEqual([unspecified.phaseLabel, unspecified.observed], ["Not reported", true]);
  const repeated = nodeGroupRows([platform(SYSTEM_GROUP, NodeGroupPhase.READY, { nodes: 1 }), platform(SYSTEM_GROUP, NodeGroupPhase.FAILED)]);
  assert.deepEqual(repeated.map(row => [row.name, row.phaseLabel]), [[SYSTEM_GROUP, "Ready"]]);
  assert.deepEqual(phaseDisplay(NodeGroupPhase.FAILED), ["Failed", "error"]);
});

test("only manifests with YAML are shown: the system group's entry has none", () => {
  assert.equal(hasManifestYaml(create(NodeGroupManifestSchema, { name: SYSTEM_GROUP, platformManaged: true })), false);
  assert.equal(hasManifestYaml(manifest(PLATFORM_POOL, true)), true);
  assert.equal(hasManifestYaml(undefined), false);
});
