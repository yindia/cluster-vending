import assert from "node:assert/strict";
import { test } from "node:test";
import { clone, create, fromJson, toJson } from "@bufbuild/protobuf";
import { AccessSpecSchema } from "../../gen/cloud/v1/access_pb";
import { ClusterSchema, ClusterSpecSchema, ClusterState, UpgradeMode } from "../../gen/cloud/v1/cluster_pb";
import { NetworkSchema } from "../../gen/cloud/v1/network_pb";
import { CloudAccountSchema } from "../../gen/cloud/v1/tenant_service_pb";
import { PlanSchema } from "../../gen/cloud/v1/plan_pb";
import {
  applyReviewedClusterPlan, bindClusterPlan, buildClusterUpdate, buildCreateCluster,
  clusterForm, clusterIsConverging, clusterPlanApplicable, clusterProvider,
  createRequestIdentity, emptyClusterForm, changedClusterFields, clusterReviewStep, clusterWizardSteps,
  validateClusterForm, validateClusterFormStep, buildImportCluster,
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
    provider: { case: "aws", value: { irsa: true, addons: [{ name: "coredns", version: "v1.0.0" }] } },
  },
});

test("fallback resolved spec updates contain only touched fields, with no defaults or immutable paths", () => {
  const form = { ...clusterForm(resolved), displayName: "Renamed" };
  const request = buildClusterUpdate("tenant-a", resolved, form);
  assert.deepEqual(request.updateMask?.paths, ["display_name"]);
  assert.deepEqual(toJson(ClusterSchema, request.cluster!), { id: "cluster-1", etag: "version-one", displayName: "Renamed" });
  assert.equal(resolved.spec?.controlPlane?.kubernetesVersion, "1.33");
  assert.equal(request.validateOnly, true);
  assert.equal(request.tenantId, "tenant-a");
  assert.equal(request.planId, "");
});

test("leaf masks preserve false, zero, empty lists and clearing optional IRSA without replacing parent messages", () => {
  const baseline = clone(ClusterSchema, resolved);
  baseline.spec!.access = { $typeName: "cloud.v1.AccessSpec", adminPrincipals: ["arn:aws:iam::123456789012:role/admin"], accessEntries: [] };
  const form = { ...clusterForm(baseline), publicAccess: false, adminPrincipals: "", irsa: "default" };
  const request = buildClusterUpdate("tenant-a", baseline, form);
  assert.deepEqual(request.updateMask?.paths, [
    "spec.access.admin_principals", "spec.aws.irsa", "spec.control_plane.endpoint_access.endpoint_public_access",
  ]);
  assert.equal(request.cluster?.spec?.controlPlane?.endpointAccess?.endpointPublicAccess, false);
  assert.deepEqual(request.cluster?.spec?.access?.adminPrincipals, []);
  assert.equal(request.cluster?.spec?.provider.case, "aws");
  if (request.cluster?.spec?.provider.case === "aws") assert.equal(request.cluster.spec.provider.value.irsa, undefined);
  assert.equal(request.cluster?.spec?.controlPlane?.kubernetesVersion, "");
});

test("rejects edits to immutable name/network and unsupported masks including region and tier", () => {
  const form = clusterForm(resolved);
  for (const field of ["name", "networkId"]) {
    assert.throws(() => buildClusterUpdate("tenant-a", resolved, { ...form, displayName: "Renamed", [field]: "changed" }), /immutable/);
  }
  assert.throws(() => buildClusterUpdate("tenant-a", resolved, { ...form, zoneIds: "apne1-az1, apne1-az2" }), /immutable/);
  assert.throws(() => buildClusterUpdate("tenant-a", { ...resolved, etag: "" }, { ...form, displayName: "Renamed" }), /etag/);
  assert.throws(() => buildClusterUpdate("tenant-a", resolved, form), /at least one/);
});

test("switching to automatic mode never converts a resolved or previously edited version into a pin", () => {
  const form = { ...clusterForm(resolved), upgradeMode: "2", kubernetesVersion: "1.34" };
  const request = buildClusterUpdate("tenant-a", resolved, form);
  assert.deepEqual(request.updateMask?.paths, ["spec.upgrade_mode"]);
  assert.equal(request.cluster?.spec?.controlPlane, undefined);
  assert.equal(request.cluster?.spec?.upgradeMode, UpgradeMode.AUTOMATIC);
  const unrelated = buildClusterUpdate("tenant-a", resolved, { ...clusterForm(resolved), displayName: "Renamed" });
  assert.equal(unrelated.cluster?.spec, undefined);
});

test("manual version is masked only after explicit editing; unedited resolved version stays absent", () => {
  const request = buildClusterUpdate("tenant-a", resolved, { ...clusterForm(resolved), naas: true });
  assert.deepEqual(request.updateMask?.paths, ["spec.naas"]);
  assert.equal(request.cluster?.spec?.controlPlane, undefined);
  const pin = buildClusterUpdate("tenant-a", resolved, { ...clusterForm(resolved), kubernetesVersion: "1.34" });
  assert.deepEqual(pin.updateMask?.paths, ["spec.control_plane.kubernetes_version"]);
  assert.equal(pin.cluster?.spec?.controlPlane?.kubernetesVersion, "1.34");
});

test("create derives immutable region and provider from existing network/account and never submits cluster tier", () => {
  const form = { ...emptyClusterForm, networkId: network.id, name: "new-cluster", publicCIDRs: "203.0.113.0/24" };
  const request = buildCreateCluster("tenant-a", form, network, accounts, "stable-uuid");
  const json = toJson(ClusterSchema, request.cluster!);
  assert.equal(request.cluster?.spec?.region, network.spec?.region);
  assert.equal(request.cluster?.spec?.network?.networkId, network.id);
  assert.equal(request.cluster?.spec?.provider.case, "aws");
  assert.equal(request.requestId, "stable-uuid");
  assert.equal(JSON.stringify(json).includes('"tier"'), false);
  assert.equal(JSON.stringify(json).includes('"kubernetesVersion"'), false);
  assert.equal(JSON.stringify(json).includes("NodePool"), false, "the platform creates the node groups");
  assert.equal(JSON.stringify(json).includes("nodeGroups"), false);
  if (request.cluster?.spec?.provider.case === "aws") assert.equal(request.cluster.spec.provider.value.irsa, undefined);
  const automatic = buildCreateCluster("tenant-a", { ...form, upgradeMode: "2", kubernetesVersion: "1.34" }, network, accounts, "stable-uuid");
  assert.equal(automatic.cluster?.spec?.controlPlane?.kubernetesVersion, "");
  assert.throws(() => buildCreateCluster("tenant-a", { ...form, networkId: "different" }, network, accounts, "id"), /existing network/);
  assert.throws(() => clusterProvider(network, []), /no supported cluster provider/);
  assert.throws(() => clusterProvider(network, [create(CloudAccountSchema, { id: "account-1" })]), /no supported cluster provider/);
});

test("validate only touched inputs on updates, and validate endpoint dependencies on submit", () => {
  const form = clusterForm(resolved);
  const odd = { ...form, awsLogRetentionDays: "broken" };
  assert.doesNotThrow(() => buildClusterUpdate("tenant-a", resolved, { ...odd, displayName: "Renamed" }, odd));
  assert.throws(() => buildClusterUpdate("tenant-a", resolved, { ...form, awsLogRetentionDays: "-1" }), /whole number/);
  assert.throws(() => buildClusterUpdate("tenant-a", resolved, { ...form, publicCIDRs: "" }), /requires at least one/);
  assert.throws(() => buildClusterUpdate("tenant-a", resolved, { ...form, publicAccess: false, privateAccess: false }), /at least one API endpoint/);
  assert.throws(() => buildClusterUpdate("tenant-a", resolved, { ...form, publicCIDRs: "999.1.1.1/33" }), /IPv4 CIDRs/);
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
    const request = buildClusterUpdate("tenant-a", baseline, form);
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
    const changed = buildClusterUpdate("tenant-a", baseline, { ...form, displayName: "Other" });
    assert.throws(() => applyReviewedClusterPlan(review, changed), /Inputs or etag changed/);
    const unrelated = buildClusterUpdate("tenant-a", baseline, { ...clusterForm(baseline), displayName: "Renamed" });
    assert.deepEqual(unrelated.updateMask?.paths, ["display_name"]);
    assert.equal(unrelated.cluster?.spec, undefined);
  }
});

test("applying uses exact reviewed payload, tenant, mask and etag with explicit plan ID", () => {
  const form = { ...clusterForm(resolved), maintenanceStart: "02:00", maintenanceDuration: "4h" };
  const request = buildClusterUpdate("tenant-a", resolved, form);
  const plan = create(PlanSchema, { planId: "plan-1", applicable: true, changes: [{ action: 2, name: "nodegroup" }] });
  const review = bindClusterPlan(request, plan);
  const apply = applyReviewedClusterPlan(review, request);
  assert.equal(apply.planId, "plan-1");
  assert.equal(apply.validateOnly, false);
  assert.deepEqual(apply.cluster, request.cluster);
  assert.deepEqual(apply.updateMask, request.updateMask);
  assert.equal(request.validateOnly, true);
  assert.equal(request.planId, "");
  const changed = buildClusterUpdate("tenant-a", resolved, { ...form, maintenanceDuration: "5h" });
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
  const request = buildClusterUpdate("tenant-a", resolved, { ...clusterForm(resolved), displayName: "Renamed" });
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
  const draft = { ...emptyClusterForm, name: "new-cluster", networkId: network.id, addonsYaml: "- unexpected: true", publicCIDRs: "" };
  assert.doesNotThrow(() => validateClusterFormStep(draft, 0));
  assert.throws(() => validateClusterFormStep(draft, 1), /requires at least one/);
  assert.throws(() => validateClusterFormStep(draft, 2));
  assert.throws(() => validateClusterFormStep({ ...draft, name: "Bad_Name" }, 0), /DNS label/);
  assert.doesNotThrow(() => validateClusterFormStep({ ...draft, upgradeMode: "2", kubernetesVersion: "not-a-version" }, 0));
  assert.doesNotThrow(() => validateClusterFormStep(draft, 1, draft));
  assert.doesNotThrow(() => validateClusterFormStep(draft, 2, draft));
  assert.throws(() => validateClusterFormStep(draft, 1, { ...draft, publicCIDRs: "203.0.113.0/24" }), /requires at least one/);
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

test("guided create covers tags, maintenance, access and AWS settings", () => {
  const form = {
    ...emptyClusterForm, name: "complete", networkId: network.id, publicCIDRs: "203.0.113.0/24",
    labels: '{"cost-center":"42"}',
    maintenanceDays: "Monday, Saturday", maintenanceStart: "23:30", maintenanceDuration: "90m",
    zoneIds: "apne1-az1, apne1-az2", serviceCidr: "172.20.0.0/16", encryptionEnabled: "true",
    encryptionKeyArn: "arn:aws:kms:ap-northeast-1:123456789012:key/key-123",
    awsAdditionalSecurityGroups: "sg-12345678", awsAuthenticationMode: "2", awsSupportType: "1",
    awsLogGroupEnabled: "true", awsLogRetentionDays: "90", awsLogTypes: "api, audit",
    oidcYaml: 'name: corp\nissuerUrl: https://id.example.com\nclientId: platform\ngroupsClaim: groups\ngroupsPrefix: "oidc:"',
    accessEntriesYaml: '- principalArn: arn:aws:iam::123456789012:role/reader\n  policies:\n    - policyArn: arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy\n      scope: 2\n      namespaces: [payments]',
    addonsYaml: '- name: coredns\n  version: "v1.11.4-eksbuild.2"\n  configurationValues: \'{"replicaCount":3}\'',
  };
  const created = buildCreateCluster("tenant-a", form, network, accounts, "same-request").cluster!;
  assert.deepEqual(created.labels, { "cost-center": "42" });
  assert.deepEqual(created.spec?.network?.zoneIds, ["apne1-az1", "apne1-az2"]);
  assert.equal(created.spec?.network?.serviceCidr, "172.20.0.0/16");
  assert.deepEqual(created.spec?.maintenanceWindow?.days, [1, 6]);
  assert.equal(created.spec?.maintenanceWindow?.duration, "90m");
  assert.equal(created.spec?.controlPlane?.secretsEncryption?.kmsKeyArn, form.encryptionKeyArn);
  assert.equal(created.spec?.access?.oidc?.groupsPrefix, "oidc:");
  assert.deepEqual(created.spec?.access?.accessEntries[0].policies[0].namespaces, ["payments"]);
  assert.equal(created.spec?.provider.case, "aws");
  if (created.spec?.provider.case === "aws") {
    const aws = created.spec.provider.value;
    assert.deepEqual(aws.additionalSecurityGroupIds, ["sg-12345678"]);
    assert.equal(aws.accessConfig?.authenticationMode, 2);
    assert.equal(aws.upgradePolicy?.supportType, 1);
    assert.equal(aws.controlPlaneLogGroup?.enabled, true);
    assert.deepEqual(aws.enabledClusterLogTypes, [1, 2]);
    assert.equal(aws.addons[0].configurationValues, '{"replicaCount":3}');
  }
  const hydrated = clusterForm(created);
  const roundtrip = buildCreateCluster("tenant-a", hydrated, network, accounts, "same-request").cluster!;
  assert.deepEqual(toJson(ClusterSchema, roundtrip), toJson(ClusterSchema, created));
});

test("cloud tag updates are a sparse map and preserve all unshown settings", () => {
  const unshown = { ...clusterForm(resolved), oidcYaml: "malformed: [" };
  const request = buildClusterUpdate("tenant-a", resolved, { ...unshown, labels: '{"team":"platform"}' }, unshown);
  assert.deepEqual(request.updateMask?.paths, ["labels"]);
  assert.deepEqual(request.cluster?.labels, { team: "platform" });
  assert.equal(request.cluster?.spec?.provider.case, undefined);
  assert.equal(request.cluster?.spec?.access, undefined);
  assert.deepEqual(resolved.labels, { environment: "production" });
});

test("cloud tags follow AWS tag rules", () => {
  const form = clusterForm(resolved);
  assert.throws(() => buildClusterUpdate("tenant-a", resolved, { ...form, labels: '{"aws:reserved":"x"}' }), /AWS tag/);
});

test("immutable create-only fields cannot leak into updates", () => {
  const form = clusterForm(resolved);
  for (const [field, value] of [["zoneIds", "apne1-az1, apne1-az2"], ["serviceCidr", "172.20.0.0/16"], ["awsAdditionalSecurityGroups", "sg-12345678"], ["awsAuthenticationMode", "2"]]) {
    assert.throws(() => buildClusterUpdate("tenant-a", resolved, { ...form, [field]: value }), /immutable/);
  }
});

test("atomic grouped masks preserve untouched siblings and clear only on explicit edits", () => {
  const baseline = clone(ClusterSchema, resolved);
  baseline.spec!.maintenanceWindow = { $typeName: "cloud.v1.MaintenanceWindow", days: [1, 3], start: "02:00", duration: "4h" };
  if (baseline.spec!.provider.case === "aws") baseline.spec!.provider.value.controlPlaneLogGroup = { $typeName: "cloud.v1.AwsControlPlaneLogGroup", enabled: true, retentionDays: 90 };
  const form = { ...clusterForm(baseline), maintenanceStart: "03:00", awsLogRetentionDays: "30" };
  const update = buildClusterUpdate("tenant-a", baseline, form);
  assert.deepEqual(update.updateMask?.paths, ["spec.aws.control_plane_log_group", "spec.maintenance_window"]);
  assert.deepEqual(update.cluster?.spec?.maintenanceWindow?.days, [1, 3]);
  assert.equal(update.cluster?.spec?.maintenanceWindow?.duration, "4h");
  if (update.cluster?.spec?.provider.case === "aws") {
    assert.equal(update.cluster.spec.provider.value.controlPlaneLogGroup?.enabled, true);
    assert.equal(update.cluster.spec.provider.value.controlPlaneLogGroup?.retentionDays, 30);
  }
  const clear = buildClusterUpdate("tenant-a", baseline, { ...clusterForm(baseline), maintenanceDays: "", maintenanceStart: "", maintenanceDuration: "" });
  assert.deepEqual(clear.updateMask?.paths, ["spec.maintenance_window"]);
  assert.equal(clear.cluster?.spec?.maintenanceWindow, undefined);
  assert.throws(() => buildClusterUpdate("tenant-a", baseline, { ...form, maintenanceDuration: "30m" }));
  assert.throws(() => buildClusterUpdate("tenant-a", baseline, { ...form, maintenanceStart: "25:00" }));
});

test("encryption transitions keep the key and cannot disable existing encryption", () => {
  const baseline = clone(ClusterSchema, resolved);
  baseline.spec!.controlPlane!.secretsEncryption = { $typeName: "cloud.v1.SecretsEncryption", enabled: true, kmsKeyArn: "arn:aws:kms:ap-northeast-1:123456789012:key/original" };
  const form = clusterForm(baseline);
  assert.throws(() => buildClusterUpdate("tenant-a", baseline, { ...form, encryptionEnabled: "false", encryptionKeyArn: "" }), /cannot be disabled/);
  assert.throws(() => buildClusterUpdate("tenant-a", baseline, { ...form, encryptionKeyArn: "arn:aws:kms:ap-northeast-1:123456789012:key/other" }), /another key/);
  // Restating the stored setting is not a change: nothing about encryption is sent.
  const update = buildClusterUpdate("tenant-a", baseline, { ...form, encryptionEnabled: "true", displayName: "Renamed" });
  assert.deepEqual(update.updateMask?.paths, ["display_name"]);
  assert.equal(update.cluster?.spec?.controlPlane?.secretsEncryption, undefined);
});

test("typed bounded nested YAML handles add-ons and exact plan binding", () => {
  const form = { ...clusterForm(resolved), addonsYaml: "- name: coredns" };
  const request = buildClusterUpdate("tenant-a", resolved, form);
  assert.deepEqual(request.updateMask?.paths, ["spec.aws.addons"]);
  if (request.cluster?.spec?.provider.case === "aws") {
    assert.equal(request.cluster.spec.provider.value.addons[0].version, ""); // no fabricated channel pin
  }
  const review = bindClusterPlan(request, create(PlanSchema, { planId: "advanced", applicable: true }));
  assert.equal(applyReviewedClusterPlan(review, request).planId, "advanced");
  const changed = buildClusterUpdate("tenant-a", resolved, { ...form, addonsYaml: "- name: coredns\n  version: v1.2.3" });
  assert.throws(() => applyReviewedClusterPlan(review, changed), /Inputs or etag changed/);
  for (const oidcYaml of ["unexpected: true", "name: corp\nname: other", "a: &a\n  b: *a", `name: ${"a".repeat(65537)}`]) {
    assert.throws(() => buildClusterUpdate("tenant-a", resolved, { ...form, oidcYaml }));
  }
});

test("nested message clears and access changes retain sparse masks and sibling defaults", () => {
  const configured = clone(ClusterSchema, resolved);
  configured.spec!.access = fromJson(AccessSpecSchema, { oidc: { name: "corp", issuerUrl: "https://id.example.com", clientId: "platform" }, accessEntries: [{ principalArn: "arn:aws:iam::123456789012:role/reader" }] });
  const form = { ...clusterForm(configured), oidcYaml: "", accessEntriesYaml: "", addonsYaml: "" };
  const request = buildClusterUpdate("tenant-a", configured, form);
  assert.deepEqual(request.updateMask?.paths, ["spec.access.access_entries", "spec.access.oidc", "spec.aws.addons"]);
  assert.equal(request.cluster?.spec?.access?.oidc, undefined);
  assert.deepEqual(request.cluster?.spec?.access?.accessEntries, []);
  if (request.cluster?.spec?.provider.case === "aws") {
    assert.deepEqual(request.cluster.spec.provider.value.addons, []);
    assert.equal(request.cluster.spec.provider.value.irsa, undefined);
  }
  assert.throws(() => buildClusterUpdate("tenant-a", configured, { ...form, adminPrincipals: "not-an-arn" }), /IAM role/);
  assert.throws(() => buildClusterUpdate("tenant-a", configured, { ...form, irsa: "typo" }), /true, false/);
  assert.throws(() => buildClusterUpdate("tenant-a", configured, { ...form, awsLogRetentionDays: "4" }));
  assert.throws(() => buildClusterUpdate("tenant-a", configured, { ...form, addonsYaml: "- name: coredns\n- name: coredns" }));
  const baseline = clone(ClusterSchema, resolved);
  baseline.spec!.access = { $typeName: "cloud.v1.AccessSpec", adminPrincipals: ["arn:aws:iam::123456789012:role/admin"], accessEntries: [] };
  assert.throws(() => buildClusterUpdate("tenant-a", baseline, { ...clusterForm(baseline), accessEntriesYaml: "- principalArn: arn:aws:iam::123456789012:role/admin" }), /cannot be both/);
});

test("advanced network and addon validation belongs to the rendered Access step", () => {
  const base = { ...emptyClusterForm, name: "wizard", networkId: network.id, publicCIDRs: "203.0.113.0/24" };
  for (const [step, invalid] of [[1, { zoneIds: "invalid-zone" }], [1, { serviceCidr: "172.20.0.0/25" }], [2, { addonsYaml: "- unexpected: true" }]] as const) {
    const form = { ...base, ...invalid };
    for (const other of [0, 1, 2].filter((index) => index !== step)) {
      assert.doesNotThrow(() => validateClusterFormStep(form, other));
      assert.doesNotThrow(() => validateClusterFormStep(form, other, base));
    }
    assert.throws(() => validateClusterFormStep(form, step));
    assert.throws(() => validateClusterFormStep(form, step, base));
  }
});

test("advanced AWS leaf edits never replace unrelated provider settings", () => {
  for (const [field, value, path] of [
    ["awsSupportType", "2", "spec.aws.upgrade_policy.support_type"],
    ["awsLogTypes", "1,5", "spec.aws.enabled_cluster_log_types"],
  ] as const) {
    const request = buildClusterUpdate("tenant-a", resolved, { ...clusterForm(resolved), [field]: value });
    assert.deepEqual(request.updateMask?.paths, [path]);
    assert.equal(request.cluster?.spec?.provider.case, "aws");
    if (request.cluster?.spec?.provider.case === "aws") {
      const aws = request.cluster.spec.provider.value;
      assert.equal(aws.irsa, undefined);
      assert.deepEqual(aws.addons, []);
      assert.equal(aws.controlPlaneLogGroup, undefined);
    }
  }
});

test("node groups are not part of the cluster form: the platform creates them", () => {
  assert.deepEqual([...clusterWizardSteps], ["Cluster settings", "Access", "Add-ons and logging", "Review"]);
  for (const field of ["instanceTypes", "minSize", "maxSize", "desiredSize", "diskSize", "capacityType", "nodeLabels", "nodeTaints", "awsAmiType", "launchTemplateYaml", "nodeGroups"]) {
    assert.equal(Object.hasOwn(emptyClusterForm, field), false, field);
    assert.equal(Object.hasOwn(clusterForm(resolved), field), false, field);
  }
});

// GetCluster(as_requested) of a platform-created cluster: the system group is the platform's, so the stored input has none.
const requested = create(ClusterSchema, {
  id: "cluster-2", name: "requested", etag: "etag-2", labels: { team: "payments" },
  spec: {
    region: "ap-northeast-1", network: { networkId: network.id }, upgradeMode: UpgradeMode.MANUAL,
    controlPlane: { endpointAccess: { endpointPublicAccess: true, publicAccessCidrs: ["203.0.113.0/24"] } },
    provider: { case: "aws", value: {} },
  },
});
const running = clone(ClusterSchema, requested);
running.spec = create(ClusterSpecSchema, {
  region: "ap-northeast-1", upgradeMode: UpgradeMode.MANUAL,
  network: { networkId: network.id, zoneIds: ["apne1-az1", "apne1-az2"], serviceCidr: "172.20.0.0/16" },
  controlPlane: { kubernetesVersion: "1.33", endpointAccess: { endpointPublicAccess: true, endpointPrivateAccess: true, publicAccessCidrs: ["203.0.113.0/24"] } },
  provider: { case: "aws", value: { accessConfig: { authenticationMode: 1 } } },
});

test("an edit form shows the create-time settings the cluster runs from the resolved spec", () => {
  const form = clusterForm(requested, running);
  assert.equal(form.awsAuthenticationMode, "1");
  assert.equal(form.zoneIds, "apne1-az1, apne1-az2");
  assert.equal(form.serviceCidr, "172.20.0.0/16");
  assert.equal(form.kubernetesVersion, "", "an unpinned version stays the server default");
  const bare = clusterForm(requested);
  assert.equal(bare.zoneIds, "");
  assert.equal(bare.awsAuthenticationMode, "");
});

test("an untouched edit passes every step and has nothing to preview", () => {
  const baseline = clusterForm(requested, running);
  assert.doesNotThrow(() => validateClusterForm(baseline, baseline));
  for (let step = 0; step < clusterReviewStep; step++) assert.doesNotThrow(() => validateClusterFormStep(baseline, step, baseline));
  assert.throws(() => buildClusterUpdate("tenant-a", requested, baseline, baseline), /No changes/);
  const request = buildClusterUpdate("tenant-a", requested, { ...baseline, displayName: "Payments" }, baseline);
  assert.deepEqual(request.updateMask?.paths, ["display_name"]);
  assert.equal(request.cluster?.spec, undefined);
});

test("the update mask is the diff against the form as it opened: reformatting and reverting are not changes", () => {
  const baseline = clusterForm(requested, running);
  assert.deepEqual([...changedClusterFields(baseline, { ...baseline, publicCIDRs: " 203.0.113.0/24 ", labels: '{ "team": "payments" }' })], []);
  assert.deepEqual([...changedClusterFields(baseline, { ...baseline, displayName: "Payments" })], ["displayName"]);
  assert.deepEqual([...changedClusterFields(baseline, { ...baseline, displayName: "" })], [], "an edit back to the opened value is no change");
  const edited = { ...baseline, naas: true, maintenanceStart: "02:00", maintenanceDuration: "4h" };
  const request = buildClusterUpdate("tenant-a", requested, edited, baseline);
  assert.deepEqual(request.updateMask?.paths, ["spec.maintenance_window", "spec.naas"]);
  assert.throws(() => buildClusterUpdate("tenant-a", requested, { ...baseline, serviceCidr: "172.21.0.0/16" }, baseline), /immutable/);
});

test("create never sends node groups or a default node pool: the server's platform defaults apply", () => {
  const draft = { ...emptyClusterForm, name: "fresh", networkId: network.id, publicCIDRs: "203.0.113.0/24" };
  const json = toJson(ClusterSchema, buildCreateCluster("tenant-a", draft, network, accounts, "r1").cluster!) as { spec: Record<string, unknown> };
  assert.equal(json.spec.defaultNodePool, undefined);
  assert.equal(json.spec.nodeGroups, undefined);
  assert.equal((json.spec.aws as Record<string, unknown> | undefined)?.defaultNodePool, undefined);
});
