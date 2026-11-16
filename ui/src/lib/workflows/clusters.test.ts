import assert from "node:assert/strict";
import { test } from "node:test";
import { clone, create, toJson } from "@bufbuild/protobuf";
import { ClusterSchema, ClusterState, UpgradeMode } from "../../gen/cloud/v1/cluster_pb";
import { NetworkSchema } from "../../gen/cloud/v1/network_pb";
import { CloudAccountSchema } from "../../gen/cloud/v1/tenant_service_pb";
import { PlanSchema } from "../../gen/cloud/v1/plan_pb";
import {
  applyReviewedClusterPlan, bindClusterPlan, buildClusterUpdate, buildCreateCluster,
  clusterForm, clusterIsConverging, clusterPlanApplicable, clusterProvider,
  createRequestIdentity, emptyClusterForm,
  validateClusterFormStep, buildImportCluster,
} from "./clusters";

const network = create(NetworkSchema, {
  id: "00000000-0000-4000-8000-000000000001", name: "shared",
  spec: { region: "ap-northeast-1", bootstrapId: "account-1", tier: 3 },
});
const accounts = [create(CloudAccountSchema, { id: "account-1", provider: { case: "aws", value: { accountId: "123456789012" } } })];
const resolved = create(ClusterSchema, {
  id: "cluster-1", name: "example", displayName: "Example", etag: "version-one", generation: 4n,
  labels: { environment: "production" },
  spec: {
    region: "ap-northeast-1", network: { networkId: network.id }, upgradeMode: UpgradeMode.MANUAL,
    controlPlane: { kubernetesVersion: "1.33", endpointAccess: { endpointPublicAccess: true, endpointPrivateAccess: true, publicAccessCidrs: ["203.0.113.0/24"] } },
    defaultNodePool: { instanceTypes: ["m6i.large"], capacityType: 1, diskSizeGb: 20, scalingConfig: { minSize: 1, maxSize: 5, desiredSize: 3 } },
    provider: { case: "aws", value: { irsa: true, addons: [{ name: "coredns", version: "v1.0.0" }] } },
  },
});

test("fallback resolved spec updates contain only touched fields, with no defaults or immutable paths", () => {
  const form = { ...clusterForm(resolved), displayName: "Renamed" };
  const request = buildClusterUpdate("tenant-a", resolved, form, new Set(["displayName"]));
  assert.deepEqual(request.updateMask?.paths, ["display_name"]);
  assert.deepEqual(toJson(ClusterSchema, request.cluster!), { id: "cluster-1", etag: "version-one", displayName: "Renamed" });
  assert.equal(resolved.spec?.controlPlane?.kubernetesVersion, "1.33");
  assert.equal(request.validateOnly, true);
  assert.equal(request.tenantId, "tenant-a");
  assert.equal(request.planId, "");
});

test("leaf masks preserve false, zero, empty lists and clearing optional IRSA without replacing parent messages", () => {
  const form = { ...clusterForm(resolved), publicAccess: false, minSize: "0", adminPrincipals: "", irsa: "default" };
  const request = buildClusterUpdate("tenant-a", resolved, form, new Set(["publicAccess", "minSize", "adminPrincipals", "irsa"]));
  assert.deepEqual(request.updateMask?.paths, [
    "spec.access.admin_principals", "spec.aws.irsa", "spec.control_plane.endpoint_access.endpoint_public_access", "spec.default_node_pool.scaling_config.min_size",
  ]);
  assert.equal(request.cluster?.spec?.controlPlane?.endpointAccess?.endpointPublicAccess, false);
  assert.equal(request.cluster?.spec?.defaultNodePool?.scalingConfig?.minSize, 0);
  assert.deepEqual(request.cluster?.spec?.access?.adminPrincipals, []);
  assert.equal(request.cluster?.spec?.provider.case, "aws");
  if (request.cluster?.spec?.provider.case === "aws") assert.equal(request.cluster.spec.provider.value.irsa, undefined);
  assert.equal(request.cluster?.spec?.defaultNodePool?.scalingConfig?.desiredSize, undefined);
  assert.equal(request.cluster?.spec?.controlPlane?.kubernetesVersion, "");
});

test("rejects edits to immutable name/network and unsupported masks including region, tier and desired size", () => {
  const form = clusterForm(resolved);
  for (const field of ["name", "networkId"]) {
    assert.throws(() => buildClusterUpdate("tenant-a", resolved, { ...form, [field]: "changed" }, new Set(["displayName"])), /immutable/);
  }
  for (const path of ["region", "spec.region", "tier", "spec.tier", "spec.network", "desiredSize", "defaultNodePool"]) {
    assert.throws(() => buildClusterUpdate("tenant-a", resolved, form, new Set([path])), /immutable or unsupported/);
  }
  assert.throws(() => buildClusterUpdate("tenant-a", { ...resolved, etag: "" }, form, new Set(["displayName"])), /etag/);
  assert.throws(() => buildClusterUpdate("tenant-a", resolved, form, new Set()), /at least one/);
});

test("switching to automatic mode never converts a resolved or previously edited version into a pin", () => {
  const form = { ...clusterForm(resolved), upgradeMode: "2", kubernetesVersion: "1.34" };
  const request = buildClusterUpdate("tenant-a", resolved, form, new Set(["upgradeMode", "kubernetesVersion"]));
  assert.deepEqual(request.updateMask?.paths, ["spec.upgrade_mode"]);
  assert.equal(request.cluster?.spec?.controlPlane, undefined);
  assert.equal(request.cluster?.spec?.upgradeMode, UpgradeMode.AUTOMATIC);
  const unrelated = buildClusterUpdate("tenant-a", resolved, form, new Set(["displayName"]));
  assert.equal(unrelated.cluster?.spec, undefined);
});

test("manual version is masked only after explicit editing; unedited resolved version stays absent", () => {
  const form = { ...clusterForm(resolved), kubernetesVersion: "1.34", maxSize: "7" };
  const request = buildClusterUpdate("tenant-a", resolved, form, new Set(["maxSize"]));
  assert.deepEqual(request.updateMask?.paths, ["spec.default_node_pool.scaling_config.max_size"]);
  assert.equal(request.cluster?.spec?.controlPlane, undefined);
  const pin = buildClusterUpdate("tenant-a", resolved, form, new Set(["kubernetesVersion"]));
  assert.deepEqual(pin.updateMask?.paths, ["spec.control_plane.kubernetes_version"]);
  assert.equal(pin.cluster?.spec?.controlPlane?.kubernetesVersion, "1.34");
});

test("create derives immutable region and provider from existing network/account and never submits cluster tier", () => {
  const form = { ...emptyClusterForm, networkId: network.id, name: "new-cluster", instanceTypes: "m6i.large, m6a.large", publicCIDRs: "203.0.113.0/24" };
  const request = buildCreateCluster("tenant-a", form, network, accounts, "stable-uuid");
  const json = toJson(ClusterSchema, request.cluster!);
  assert.equal(request.cluster?.spec?.region, network.spec?.region);
  assert.equal(request.cluster?.spec?.network?.networkId, network.id);
  assert.equal(request.cluster?.spec?.provider.case, "aws");
  assert.equal(request.requestId, "stable-uuid");
  assert.equal(JSON.stringify(json).includes('"tier"'), false);
  assert.equal(JSON.stringify(json).includes('"kubernetesVersion"'), false);
  if (request.cluster?.spec?.provider.case === "aws") assert.equal(request.cluster.spec.provider.value.irsa, undefined);
  const automatic = buildCreateCluster("tenant-a", { ...form, upgradeMode: "2", kubernetesVersion: "1.34" }, network, accounts, "stable-uuid");
  assert.equal(automatic.cluster?.spec?.controlPlane?.kubernetesVersion, "");
  assert.throws(() => buildCreateCluster("tenant-a", { ...form, networkId: "different" }, network, accounts, "id"), /existing network/);
  assert.throws(() => clusterProvider(network, []), /no supported cluster provider/);
  assert.throws(() => clusterProvider(network, [create(CloudAccountSchema, { id: "account-1" })]), /no supported cluster provider/);
});

test("validate only touched inputs on updates, and validate endpoint and scaling dependencies on submit", () => {
  const form = clusterForm(resolved);
  assert.doesNotThrow(() => buildClusterUpdate("tenant-a", resolved, { ...form, instanceTypes: "", minSize: "broken" }, new Set(["displayName"])));
  assert.throws(() => buildClusterUpdate("tenant-a", resolved, { ...form, minSize: "6" }, new Set(["minSize"])), /Minimum nodes/);
  assert.throws(() => buildClusterUpdate("tenant-a", resolved, { ...form, minSize: "-1" }, new Set(["minSize"])), /whole number/);
  assert.throws(() => buildClusterUpdate("tenant-a", resolved, { ...form, publicCIDRs: "" }, new Set(["publicCIDRs"])), /requires at least one/);
  assert.throws(() => buildClusterUpdate("tenant-a", resolved, { ...form, publicAccess: false, privateAccess: false }, new Set(["publicAccess", "privateAccess"])), /at least one API endpoint/);
  assert.throws(() => buildClusterUpdate("tenant-a", resolved, { ...form, publicCIDRs: "999.1.1.1/33" }, new Set(["publicCIDRs"])), /IPv4 CIDRs/);
});

test("NaaS defaults off, hydrates saved values and is included in guided creation", () => {
  assert.equal(emptyClusterForm.naas, false);
  assert.equal(clusterForm(resolved).naas, false);
  const enabled = clone(ClusterSchema, resolved);
  enabled.spec!.naas = true;
  assert.equal(clusterForm(enabled).naas, true);
  const draft = { ...emptyClusterForm, name: "new-cluster", networkId: network.id, publicCIDRs: "203.0.113.0/24" };
  for (const naas of [false, true]) {
    const request = buildCreateCluster("tenant-a", { ...draft, naas }, network, accounts, "stable-uuid");
    assert.equal(request.cluster?.spec?.naas, naas);
    assert.equal(request.requestId, "stable-uuid");
  }
});

test("NaaS updates use a sparse leaf mask and preserve disabling false through reviewed apply", () => {
  for (const naas of [false, true]) {
    const baseline = clone(ClusterSchema, resolved);
    baseline.spec!.naas = !naas;
    const form = { ...clusterForm(baseline), naas };
    const request = buildClusterUpdate("tenant-a", baseline, form, new Set(["naas"]));
    assert.deepEqual(request.updateMask?.paths, ["spec.naas"]);
    assert.equal(request.cluster?.spec?.naas, naas);
    assert.equal(request.cluster?.spec?.network, undefined);
    assert.equal(request.cluster?.spec?.controlPlane, undefined);
    assert.equal(request.cluster?.etag, baseline.etag);
    const review = bindClusterPlan(request, create(PlanSchema, { planId: "naas-plan", applicable: true, changes: [{ action: 2 }] }));
    const apply = applyReviewedClusterPlan(review, request);
    assert.equal(apply.cluster?.spec?.naas, naas);
    assert.deepEqual(apply.updateMask?.paths, ["spec.naas"]);
    assert.equal(apply.validateOnly, false);
    assert.equal(apply.planId, "naas-plan");
    const changed = buildClusterUpdate("tenant-a", baseline, { ...form, naas: !naas }, new Set(["naas"]));
    assert.throws(() => applyReviewedClusterPlan(review, changed), /Inputs or etag changed/);
    const unrelated = buildClusterUpdate("tenant-a", baseline, { ...form, displayName: "Renamed" }, new Set(["displayName"]));
    assert.deepEqual(unrelated.updateMask?.paths, ["display_name"]);
    assert.equal(unrelated.cluster?.spec, undefined);
  }
});

test("applying uses exact reviewed payload, tenant, mask and etag with explicit plan ID", () => {
  const form = { ...clusterForm(resolved), maxSize: "7" };
  const request = buildClusterUpdate("tenant-a", resolved, form, new Set(["maxSize"]));
  const plan = create(PlanSchema, { planId: "plan-1", applicable: true, changes: [{ action: 2, name: "nodegroup" }] });
  const review = bindClusterPlan(request, plan);
  const apply = applyReviewedClusterPlan(review, request);
  assert.equal(apply.planId, "plan-1");
  assert.equal(apply.validateOnly, false);
  assert.deepEqual(apply.cluster, request.cluster);
  assert.deepEqual(apply.updateMask, request.updateMask);
  assert.equal(request.validateOnly, true);
  assert.equal(request.planId, "");
  const changed = buildClusterUpdate("tenant-a", resolved, { ...form, maxSize: "8" }, new Set(["maxSize"]));
  assert.throws(() => applyReviewedClusterPlan(review, changed), /Inputs or etag changed/);
  assert.throws(() => applyReviewedClusterPlan(review, { ...request, tenantId: "tenant-b" }), /Inputs or etag changed/);
  assert.throws(() => applyReviewedClusterPlan(review, { ...request, cluster: { ...request.cluster!, etag: "new-etag" } }), /Inputs or etag changed/);
  assert.throws(() => applyReviewedClusterPlan(review, { ...request, updateMask: { ...request.updateMask!, paths: ["spec.region"] } }), /Inputs or etag changed/);
  // Neither the RPC's response nor its outgoing request can mutate the stored review.
  plan.planId = "changed-response";
  request.cluster!.etag = "changed-request";
  assert.equal(review.plan.planId, "plan-1");
  assert.equal(review.request.cluster?.etag, "version-one");
});

test("missing IDs, rejected and unknown actions and non-applicable plans fail closed", () => {
  const request = buildClusterUpdate("tenant-a", resolved, clusterForm(resolved), new Set(["displayName"]));
  for (const plan of [
    create(PlanSchema, { applicable: true }),
    create(PlanSchema, { planId: " ", applicable: true }),
    create(PlanSchema, { planId: "plan", applicable: false }),
    ...[0, 5, 99].map((action) => create(PlanSchema, { planId: "plan", applicable: true, changes: [{ action }] })),
  ]) {
    assert.equal(clusterPlanApplicable(plan), false);
    assert.throws(() => applyReviewedClusterPlan(bindClusterPlan(request, plan), request), /cannot be applied/);
  }
  assert.equal(clusterPlanApplicable(create(PlanSchema, { planId: "plan", applicable: true, changes: [{ action: 4, retainsCloudResource: false }] })), true);
});

test("create retries reuse UUID until an input edit invalidates it", () => {
  let sequence = 0;
  const identity = createRequestIdentity(() => `uuid-${++sequence}`);
  assert.equal(identity.get(), "uuid-1");
  assert.equal(identity.get(), "uuid-1");
  identity.invalidate();
  assert.equal(identity.get(), "uuid-2");
  identity.invalidate();
  assert.equal(identity.get(), "uuid-3");
});

test("asynchronous acknowledgement is distinct from observed readiness", () => {
  assert.equal(clusterIsConverging({ ...resolved, status: undefined }), true);
  for (const state of [ClusterState.PROVISIONING, ClusterState.UPDATING, ClusterState.DELETING]) {
    assert.equal(clusterIsConverging(create(ClusterSchema, { generation: 4n, status: { state, observedGeneration: 4n } })), true);
  }
  assert.equal(clusterIsConverging(create(ClusterSchema, { generation: 4n, status: { state: ClusterState.RUNNING, observedGeneration: 3n } })), true);
  assert.equal(clusterIsConverging(create(ClusterSchema, { generation: 4n, status: { state: ClusterState.RUNNING, observedGeneration: 4n } })), false);
  assert.equal(clusterIsConverging(create(ClusterSchema, { generation: 4n, status: { state: ClusterState.FAILED, observedGeneration: 3n } })), false);
});

test("wizard Next validates only the active step; editing ignores unedited resolved defaults", () => {
  const draft = { ...emptyClusterForm, name: "new-cluster", networkId: network.id, instanceTypes: "invalid", publicCIDRs: "" };
  assert.doesNotThrow(() => validateClusterFormStep(draft, 0));
  assert.throws(() => validateClusterFormStep(draft, 1), /instance types/);
  assert.throws(() => validateClusterFormStep(draft, 2), /requires at least one/);
  assert.throws(() => validateClusterFormStep({ ...draft, name: "Bad_Name" }, 0), /DNS label/);
  assert.doesNotThrow(() => validateClusterFormStep({ ...draft, upgradeMode: "2", kubernetesVersion: "not-a-version" }, 0));
  assert.doesNotThrow(() => validateClusterFormStep(draft, 1, new Set(["displayName"])));
  assert.doesNotThrow(() => validateClusterFormStep(draft, 2, new Set(["maxSize"])));
});

test("import maps only supported AWS network and node group inputs into a read-only import request", () => {
  const form = { name: "existing", displayName: "Imported production", networkId: network.id, defaultNodeGroupName: "workers_01" };
  const request = buildImportCluster("tenant-a", form, network, accounts, "retry-id");
  assert.equal(request.tenantId, "tenant-a");
  assert.equal(request.networkId, network.id);
  assert.equal(request.requestId, "retry-id");
  assert.equal(request.name, "existing");
  assert.equal(request.provider.case, "aws");
  if (request.provider.case === "aws") assert.equal(request.provider.value.defaultNodeGroupName, "workers_01");
  assert.throws(() => buildImportCluster("tenant-a", form, network, [], "retry-id"), /supported cluster provider/);
  assert.throws(() => buildImportCluster("tenant-a", { ...form, networkId: "different" }, network, accounts, "id"), /existing network/);
  assert.throws(() => buildImportCluster("tenant-a", { ...form, defaultNodeGroupName: "bad/name" }, network, accounts, "id"), /node group name/);
});
