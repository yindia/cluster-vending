import assert from "node:assert/strict";
import { test } from "node:test";
import { create, toJson, type MessageInitShape } from "@bufbuild/protobuf";
import { ClusterSchema } from "../../gen/cloud/v1/cluster_pb";
import { Architecture, NodeGroupPhase, NodeGroupSpecSchema, NodeGroupStatusSchema } from "../../gen/cloud/v1/node_group_pb";
import { GetNodeGroupOptionsResponseSchema, NodeGroupManifestSchema } from "../../gen/cloud/v1/node_group_manifest_pb";
import { CapacityType, NodePoolSpecSchema, TaintEffect } from "../../gen/cloud/v1/node_pool_pb";
import { clusterPlanApplicable } from "./clusters";
import {
  MAX_NODE_GROUPS, applyNodeGroupChange, buildNodeGroupSpec, buildNodeGroupsUpdate, emptyNodeGroupForm, nodeGroupForm,
  nodeGroupAllowlist, nodeGroupIssues, nodeGroupRows, summarizeNodeGroup, validateNodeGroupStep,
} from "./node-groups";

const spec = (name: string, extra: MessageInitShape<typeof NodeGroupSpecSchema> = {}) => create(NodeGroupSpecSchema, { name, instanceFamilies: ["m7i"], architectures: [Architecture.AMD64], capacityTypes: [CapacityType.ON_DEMAND], ...extra });
const valid = (extra = {}) => ({ ...emptyNodeGroupForm(), name: "batch", instanceFamilies: ["m7i"], ...extra });
const messages = (form: ReturnType<typeof valid>, others: string[] = []) => nodeGroupIssues(form, others).map(issue => `${issue.field}: ${issue.message}`);

test("a minimal form is valid and defaults to AMD64 on-demand", () => {
  assert.deepEqual(nodeGroupIssues(valid(), []), []);
  const built = buildNodeGroupSpec(valid(), []);
  assert.deepEqual(toJson(NodeGroupSpecSchema, built), { name: "batch", instanceFamilies: ["m7i"], architectures: ["ARCHITECTURE_AMD64"], capacityTypes: ["CAPACITY_TYPE_ON_DEMAND"] });
});

test("names must be DNS labels of at most 40 characters, not reserved, and unique", () => {
  for (const name of ["", "Batch", "1batch", "batch-", "a_b", "x".repeat(41)]) assert.ok(nodeGroupIssues(valid({ name }), []).some(issue => issue.field === "name"), name);
  for (const name of ["platform", "default", "system"]) assert.match(messages(valid({ name })).join(), /reserved/);
  assert.match(messages(valid({ name: "batch" }), ["batch"]).join(), /already exists/);
  assert.deepEqual(nodeGroupIssues(valid({ name: "x".repeat(40) }), []), []);
  assert.deepEqual(nodeGroupIssues(valid({ name: "a" }), []), []);
});

test("instance types and families: at least one, valid, unique, bounded", () => {
  assert.match(messages(valid({ instanceFamilies: [] })).join(), /instanceTypes: .*instance type or family/);
  assert.match(messages(valid({ instanceTypes: ["m7i"], instanceFamilies: [] })).join(), /instanceTypes: .*like m7i\.large/);
  assert.match(messages(valid({ instanceFamilies: ["m7i.large"] })).join(), /instanceFamilies: .*like m7i/);
  assert.match(messages(valid({ instanceFamilies: ["m7i", "m7i"] })).join(), /unique/);
  assert.match(messages(valid({ instanceFamilies: Array.from({ length: 21 }, (_, index) => `f${index}`) })).join(), /at most 20/);
  assert.deepEqual(nodeGroupIssues(valid({ instanceTypes: ["m7i.large", "c7g.2xlarge"], instanceFamilies: [] }), []), []);
});

test("architectures and capacity types need one choice each and reject unspecified values", () => {
  assert.match(messages(valid({ architectures: [] })).join(), /architectures/);
  assert.match(messages(valid({ capacityTypes: [] })).join(), /capacityTypes/);
  assert.match(messages(valid({ architectures: [Architecture.UNSPECIFIED] })).join(), /architectures/);
  assert.deepEqual(nodeGroupIssues(valid({ architectures: [Architecture.AMD64, Architecture.ARM64], capacityTypes: [CapacityType.ON_DEMAND, CapacityType.SPOT] }), []), []);
});

test("labels and taints follow Kubernetes rules and size limits", () => {
  assert.match(messages(valid({ labels: [{ key: "bad key", value: "x" }] })).join(), /labels: .*Label 1/);
  assert.match(messages(valid({ labels: [{ key: "a", value: "x" }, { key: "a", value: "y" }] })).join(), /already appears/);
  assert.match(messages(valid({ labels: [{ key: "a", value: "bad value" }] })).join(), /labels/);
  assert.match(messages(valid({ labels: Array.from({ length: 65 }, (_, index) => ({ key: `k${index}`, value: "" })) })).join(), /at most 64/);
  assert.match(messages(valid({ taints: [{ key: "dedicated", value: "x", effect: TaintEffect.UNSPECIFIED }] })).join(), /taints: .*effect/);
  assert.match(messages(valid({ taints: [{ key: "d", value: "", effect: TaintEffect.NO_SCHEDULE }, { key: "d", value: "", effect: TaintEffect.NO_SCHEDULE }] })).join(), /already appears/);
  assert.match(messages(valid({ taints: Array.from({ length: 51 }, (_, index) => ({ key: `t${index}`, value: "", effect: TaintEffect.NO_SCHEDULE })) })).join(), /at most 50/);
  assert.deepEqual(nodeGroupIssues(valid({ taints: [{ key: "d", value: "", effect: TaintEffect.NO_SCHEDULE }, { key: "d", value: "", effect: TaintEffect.NO_EXECUTE }], labels: [{ key: "example.com/pool", value: "gpu" }] }), []), []);
});

test("limits are optional positive whole numbers", () => {
  assert.deepEqual(nodeGroupIssues(valid({ cpu: "", memoryGib: "" }), []), []);
  for (const cpu of ["0", "-1", "1.5", "abc", "99999999999"]) assert.match(messages(valid({ cpu })).join(), /cpu/, cpu);
  assert.match(messages(valid({ memoryGib: "0" })).join(), /memoryGib/);
  const built = buildNodeGroupSpec(valid({ cpu: "64", memoryGib: "256" }), []);
  assert.deepEqual(built.limits, { $typeName: "cloud.v1.NodeGroupLimits", cpu: 64, memoryGib: 256 });
  assert.equal(buildNodeGroupSpec(valid({ memoryGib: "8" }), []).limits?.cpu, undefined);
  assert.equal(buildNodeGroupSpec(valid(), []).limits, undefined);
});

test("step validation throws the first issue of that step only", () => {
  const form = valid({ name: "Bad", cpu: "0" });
  assert.throws(() => validateNodeGroupStep(form, [], 0), /Name/);
  assert.throws(() => validateNodeGroupStep(valid({ cpu: "0" }), [], 1), /CPU/);
  assert.doesNotThrow(() => validateNodeGroupStep(valid({ cpu: "0" }), [], 0));
  assert.throws(() => validateNodeGroupStep(valid(), [], 5), /Unknown/);
});

test("buildNodeGroupSpec throws for an invalid form", () => {
  assert.throws(() => buildNodeGroupSpec(valid({ name: "" }), []), /Name/);
});

test("form round-trips a spec, filling server defaults for empty choices", () => {
  const original = spec("gpu", { instanceTypes: ["g5.xlarge"], instanceFamilies: [], architectures: [], capacityTypes: [], labels: { team: "ml" }, taints: [{ $typeName: "cloud.v1.Taint", key: "gpu", value: "true", effect: TaintEffect.NO_SCHEDULE }], limits: { $typeName: "cloud.v1.NodeGroupLimits", cpu: 32 } });
  const form = nodeGroupForm(original);
  assert.deepEqual(form.architectures, [Architecture.AMD64]);
  assert.deepEqual(form.capacityTypes, [CapacityType.ON_DEMAND]);
  assert.equal(form.cpu, "32"); assert.equal(form.memoryGib, "");
  const rebuilt = buildNodeGroupSpec(form, []);
  assert.deepEqual(rebuilt.labels, { team: "ml" });
  assert.equal(rebuilt.taints[0].effect, TaintEffect.NO_SCHEDULE);
  assert.deepEqual(rebuilt.instanceTypes, ["g5.xlarge"]);
});

test("add, update and remove build new lists without mutating the input", () => {
  const current = Object.freeze([spec("a"), spec("b")]);
  const added = applyNodeGroupChange(current, { kind: "add", group: spec("c") });
  assert.deepEqual(added.map(group => group.name), ["a", "b", "c"]);
  assert.equal(current.length, 2);
  const updated = applyNodeGroupChange(current, { kind: "update", name: "b", group: spec("b", { instanceFamilies: ["c7g"] }) });
  assert.deepEqual(updated.map(group => group.instanceFamilies), [["m7i"], ["c7g"]]);
  assert.equal(current[1].instanceFamilies[0], "m7i");
  assert.notEqual(updated[0], current[0]);
  const renamed = applyNodeGroupChange(current, { kind: "update", name: "a", group: spec("z") });
  assert.deepEqual(renamed.map(group => group.name), ["z", "b"]);
  assert.deepEqual(applyNodeGroupChange(current, { kind: "remove", name: "a" }).map(group => group.name), ["b"]);
  assert.deepEqual(applyNodeGroupChange([spec("a")], { kind: "remove", name: "a" }), []);
});

test("changes reject duplicates, unknown names, reserved names and more than 20 groups", () => {
  const current = [spec("a"), spec("b")];
  assert.throws(() => applyNodeGroupChange(current, { kind: "add", group: spec("a") }), /already exists/);
  assert.throws(() => applyNodeGroupChange(current, { kind: "update", name: "missing", group: spec("x") }), /not found/);
  assert.throws(() => applyNodeGroupChange(current, { kind: "update", name: "a", group: spec("b") }), /already exists/);
  assert.throws(() => applyNodeGroupChange(current, { kind: "remove", name: "missing" }), /not found/);
  assert.throws(() => applyNodeGroupChange(current, { kind: "add", group: spec("platform") }), /reserved/);
  const full = Array.from({ length: MAX_NODE_GROUPS }, (_, index) => spec(`g${index}`));
  assert.throws(() => applyNodeGroupChange(full, { kind: "add", group: spec("extra") }), /at most 20/);
  assert.equal(applyNodeGroupChange(full, { kind: "remove", name: "g0" }).length, 19);
});

const baseline = create(ClusterSchema, { id: "cluster-1", name: "prod", etag: "etag-7", spec: { region: "us-west-2", nodeGroups: [spec("a")] } });

test("the update sends the whole list under one mask path with the cluster etag, validate-only", () => {
  const request = buildNodeGroupsUpdate("tenant-a", baseline, [spec("a"), spec("b", { capacityTypes: [CapacityType.SPOT] })]);
  assert.deepEqual(request.updateMask?.paths, ["spec.node_groups"]);
  assert.equal(request.validateOnly, true);
  assert.equal(request.tenantId, "tenant-a");
  assert.equal(request.planId, "");
  const json = toJson(ClusterSchema, request.cluster!) as { id: string; etag: string; spec: { nodeGroups: { name: string }[] } };
  assert.equal(json.id, "cluster-1"); assert.equal(json.etag, "etag-7");
  assert.deepEqual(Object.keys(json.spec), ["nodeGroups"]);
  assert.deepEqual(json.spec.nodeGroups.map(group => group.name), ["a", "b"]);
});

test("removing the last group still sends the mask with an empty list", () => {
  const request = buildNodeGroupsUpdate("tenant-a", baseline, []);
  assert.deepEqual(request.updateMask?.paths, ["spec.node_groups"]);
  assert.deepEqual(request.cluster?.spec?.nodeGroups, []);
});

test("the update requires an etag and a valid list", () => {
  assert.throws(() => buildNodeGroupsUpdate("t", create(ClusterSchema, { id: "c" }), []), /etag/);
  assert.throws(() => buildNodeGroupsUpdate("t", baseline, [spec("a"), spec("a")]), /unique|already exists/);
  assert.throws(() => buildNodeGroupsUpdate("t", baseline, [spec("default")]), /reserved/);
  assert.throws(() => buildNodeGroupsUpdate("t", baseline, Array.from({ length: 21 }, (_, index) => spec(`g${index}`))), /at most 20/);
});

test("plans with NodePool changes are applicable through the shared cluster plan check", () => {
  const plan = { $typeName: "cloud.v1.Plan", planId: "p1", applicable: true, warnings: [], changes: [{ action: 1, kind: "NodePool", name: "b" }, { action: 4, kind: "NodePool", name: "a" }] };
  assert.equal(clusterPlanApplicable(plan as never), true);
});

const status = (name: string, phase: NodeGroupPhase, extra = {}) => create(NodeGroupStatusSchema, { name, phase, ...extra });

test("rows list the system group first, then user groups, with spec-only groups pending", () => {
  const rows = nodeGroupRows([spec("a"), spec("fresh")], [status("a", NodeGroupPhase.READY, { nodes: 3 }), status("system", NodeGroupPhase.READY, { platformManaged: true, nodes: 2 })]);
  assert.deepEqual(rows.map(row => row.name), ["system", "a", "fresh"]);
  assert.equal(rows[0].platformManaged, true);
  assert.equal(rows[0].spec, undefined);
  assert.equal(rows[1].phaseLabel, "Ready"); assert.equal(rows[1].nodes, 3); assert.equal(rows[1].tone, "success");
  assert.equal(rows[2].phaseLabel, "Pending"); assert.equal(rows[2].observed, false); assert.equal(rows[2].nodes, undefined);
  assert.ok(rows[2].message);
});

test("status-only user groups remain visible while deleting, and failed groups are flagged", () => {
  const rows = nodeGroupRows([spec("bad")], [status("system", NodeGroupPhase.READY, { platformManaged: true }), status("gone", NodeGroupPhase.DELETING, { message: "draining" }), status("bad", NodeGroupPhase.FAILED, { message: "no capacity" })]);
  assert.deepEqual(rows.map(row => row.name), ["system", "bad", "gone"]);
  assert.equal(rows[1].tone, "error"); assert.equal(rows[1].message, "no capacity");
  assert.equal(rows[2].phaseLabel, "Deleting"); assert.equal(rows[2].spec, undefined); assert.equal(rows[2].editable, false);
  assert.equal(rows[1].editable, true); assert.equal(rows[0].editable, false);
});

test("the system row is always there until the manifests say the platform manages no node groups", () => {
  // Nothing reported and nothing known yet: the system group is Pending, waiting for the controller.
  const rows = nodeGroupRows([], []);
  assert.deepEqual(rows.map(row => [row.name, row.platformManaged, row.phaseLabel, row.editable]), [["system", true, "Pending", false]]);
  assert.match(rows[0].message, /Pending — waiting for the controller/);
  assert.equal(rows[0].observed, false);
  // Its phase comes from the status once there is one.
  const ready = nodeGroupRows([], [status("system", NodeGroupPhase.READY, { platformManaged: true, nodes: 2 })]);
  assert.deepEqual([ready[0].phaseLabel, ready[0].nodes, ready[0].observed], ["Ready", 2, true]);
  // An imported cluster: the manifests are known and empty.
  assert.deepEqual(nodeGroupRows([], [], []), []);
  assert.deepEqual(nodeGroupRows([spec("a")], [], []).map(row => row.name), ["a"]);
});

test("a platform group an earlier controller recorded is not a row: the system group runs the platform", () => {
  const rows = nodeGroupRows([spec("a")], [status("platform", NodeGroupPhase.READY, { platformManaged: true, nodes: 1 }), status("system", NodeGroupPhase.READY, { platformManaged: true, nodes: 2 })]);
  assert.deepEqual(rows.map(row => row.name), ["system", "a"]);
});

test("the system row carries the default node group's instance type, capacity and fixed size", () => {
  const pool = create(NodePoolSpecSchema, { instanceTypes: ["t3.large"], capacityType: CapacityType.ON_DEMAND, scalingConfig: { minSize: 2, maxSize: 2 } });
  const [system, user] = nodeGroupRows([spec("a")], [status("system", NodeGroupPhase.READY, { platformManaged: true, nodes: 2 })], undefined, pool);
  assert.deepEqual(system.system, { instanceTypes: "t3.large", capacity: "On-demand", size: "2 nodes (fixed)" });
  assert.equal(user.system, undefined);
  assert.equal(nodeGroupRows([], [], undefined, undefined)[0].system, undefined, "no default pool: no summary");
  const spot = create(NodePoolSpecSchema, { instanceTypes: ["t3.large", "t3.xlarge"], capacityType: CapacityType.SPOT, scalingConfig: { minSize: 3, maxSize: 3 } });
  assert.deepEqual(nodeGroupRows([], [], undefined, spot)[0].system, { instanceTypes: "t3.large, t3.xlarge", capacity: "Spot", size: "3 nodes (fixed)" });
});

const manifest = (name: string, platformManaged = false) => create(NodeGroupManifestSchema, { name, platformManaged, ec2NodeClassYaml: `kind: EC2NodeClass\nname: ${name}\n`, nodePoolYaml: `kind: NodePool\nname: ${name}\n` });

test("manifests are merged into the rows by name; the system group has none to show", () => {
  const systemEntry = create(NodeGroupManifestSchema, { name: "system", platformManaged: true });
  const rows = nodeGroupRows([spec("a"), spec("b")], [status("a", NodeGroupPhase.READY)], [systemEntry, manifest("a")]);
  assert.deepEqual(rows.map(row => [row.name, row.manifest?.name]), [["system", undefined], ["a", "a"], ["b", undefined]]);
  assert.match(rows[1].manifest!.nodePoolYaml, /kind: NodePool/);
  // The system row exists from the manifests alone, with the waiting message.
  assert.equal(rows[0].phaseLabel, "Pending"); assert.match(rows[0].message, /waiting for the controller/);
  // A deleting group has no manifest any more.
  const deleting = nodeGroupRows([], [status("system", NodeGroupPhase.READY, { platformManaged: true }), status("gone", NodeGroupPhase.DELETING)], [systemEntry]);
  assert.equal(deleting.find(row => row.name === "gone")?.manifest, undefined);
});

const allow = (instanceFamilies: string[], instanceTypes: string[] = []) => nodeGroupAllowlist(create(GetNodeGroupOptionsResponseSchema, { instanceFamilies, instanceTypes }));

test("the platform administrator's allow-list is checked like the server does, naming the value and the allowed list", () => {
  assert.deepEqual(nodeGroupIssues(valid({ instanceFamilies: ["zz9"] }), [], allow([])), [], "an empty allow-list restricts nothing");
  assert.deepEqual(nodeGroupIssues(valid({ instanceFamilies: ["m7i"] }), [], allow(["m7i", "c7i"])), []);
  const family = nodeGroupIssues(valid({ instanceFamilies: ["m7i", "p5"] }), [], allow(["m7i", "c7i"]));
  assert.deepEqual(family.map(issue => issue.field), ["instanceFamilies"]);
  assert.match(family[0].message, /"p5".*not allowed.*m7i, c7i/);
  // A type is allowed when listed, or when its family is.
  assert.deepEqual(nodeGroupIssues(valid({ instanceFamilies: [], instanceTypes: ["m7i.large", "r7i.large"] }), [], allow(["m7i"], ["r7i.large"])), []);
  const type = nodeGroupIssues(valid({ instanceFamilies: [], instanceTypes: ["r7i.xlarge"] }), [], allow(["m7i"], ["r7i.large"]));
  assert.deepEqual(type.map(issue => issue.field), ["instanceTypes"]);
  assert.match(type[0].message, /"r7i\.xlarge".*not allowed.*m7i.*r7i\.large/);
  // Only types listed: families are not restricted, types are.
  assert.deepEqual(nodeGroupIssues(valid({ instanceFamilies: ["anything"] }), [], allow([], ["r7i.large"])), []);
  // The build refuses what validation refuses.
  assert.throws(() => buildNodeGroupSpec(valid({ instanceFamilies: ["p5"] }), [], allow(["m7i"])), /"p5"/);
  assert.throws(() => validateNodeGroupStep(valid({ instanceFamilies: ["p5"] }), [], 0, allow(["m7i"])), /"p5"/);
});

test("the allow-list reads the options response", () => {
  assert.deepEqual(nodeGroupAllowlist(undefined), { instanceFamilies: [], instanceTypes: [] });
  assert.deepEqual(allow(["m7i"], ["r7i.large"]), { instanceFamilies: ["m7i"], instanceTypes: ["r7i.large"] });
});

test("summaries describe capacity, instances, architectures, labels, taints and limits", () => {
  const summary = summarizeNodeGroup(spec("a", { instanceTypes: ["m7i.large"], instanceFamilies: ["c7g"], architectures: [Architecture.ARM64, Architecture.AMD64], capacityTypes: [CapacityType.ON_DEMAND, CapacityType.SPOT], labels: { team: "ml", tier: "1" }, taints: [{ $typeName: "cloud.v1.Taint", key: "gpu", value: "", effect: TaintEffect.NO_SCHEDULE }], limits: { $typeName: "cloud.v1.NodeGroupLimits", cpu: 64, memoryGib: 256 } }));
  assert.equal(summary.capacity, "On-demand, Spot");
  assert.equal(summary.instances, "m7i.large, c7g family");
  assert.equal(summary.architectures, "AMD64, ARM64");
  assert.equal(summary.labels, "team=ml, tier=1");
  assert.equal(summary.taints, "gpu:NoSchedule");
  assert.equal(summary.limits, "64 vCPU · 256 GiB");
  const bare = summarizeNodeGroup(spec("b", { instanceFamilies: [], architectures: [], capacityTypes: [] }));
  assert.deepEqual([bare.capacity, bare.instances, bare.architectures, bare.labels, bare.taints, bare.limits], ["On-demand", "Any", "AMD64", "None", "None", "No limits"]);
  assert.equal(summarizeNodeGroup(spec("c", { limits: { $typeName: "cloud.v1.NodeGroupLimits", memoryGib: 8 } })).limits, "8 GiB");
});

test("status reporting tolerates unknown phases, repeated names, an unreported system group and a missing node count", () => {
  const userRows = (rows: ReturnType<typeof nodeGroupRows>) => rows.filter(row => !row.platformManaged);
  const unknown = userRows(nodeGroupRows([spec("a")], [status("a", 99 as NodeGroupPhase, { message: "from a newer controller" })]));
  assert.equal(unknown[0].phaseLabel, "Unknown (99)"); assert.equal(unknown[0].tone, "neutral"); assert.equal(unknown[0].message, "from a newer controller");
  assert.equal(unknown[0].editable, true);
  const unspecified = userRows(nodeGroupRows([spec("a")], [status("a", NodeGroupPhase.UNSPECIFIED)]));
  assert.equal(unspecified[0].phaseLabel, "Not reported"); assert.equal(unspecified[0].observed, true);
  const repeated = userRows(nodeGroupRows([spec("a")], [status("a", NodeGroupPhase.READY, { nodes: 1 }), status("a", NodeGroupPhase.FAILED)]));
  assert.deepEqual(repeated.map(row => row.name), ["a"]);
  const noSystem = nodeGroupRows([spec("a")], []);
  assert.deepEqual(noSystem.map(row => [row.name, row.platformManaged, row.phaseLabel]), [["system", true, "Pending"], ["a", false, "Pending"]]);
  const specAlias = nodeGroupRows([spec("system")], [status("system", NodeGroupPhase.READY, { platformManaged: true })]);
  assert.equal(specAlias.filter(row => row.platformManaged).length, 1);
  assert.equal(specAlias.find(row => row.platformManaged)?.editable, false);
});

test("without the allow-list from the server, families are not checked client-side: the server decides", () => {
  assert.deepEqual(nodeGroupIssues(valid({ instanceFamilies: ["zz9", "x2iedn"] }), []), []);
});
