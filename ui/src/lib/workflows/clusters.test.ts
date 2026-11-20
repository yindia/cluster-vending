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
  const form = { ...clusterForm(resolved), publicAccess: false, diskSize: "", adminPrincipals: "", irsa: "default" };
  const request = buildClusterUpdate("tenant-a", resolved, form, new Set(["publicAccess", "diskSize", "adminPrincipals", "irsa"]));
  assert.deepEqual(request.updateMask?.paths, [
    "spec.access.admin_principals", "spec.aws.irsa", "spec.control_plane.endpoint_access.endpoint_public_access", "spec.default_node_pool.disk_size_gb",
  ]);
  assert.equal(request.cluster?.spec?.controlPlane?.endpointAccess?.endpointPublicAccess, false);
  assert.equal(request.cluster?.spec?.defaultNodePool?.diskSizeGb, 0);
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
  const form = { ...clusterForm(resolved), kubernetesVersion: "1.34", diskSize: "40" };
  const request = buildClusterUpdate("tenant-a", resolved, form, new Set(["diskSize"]));
  assert.deepEqual(request.updateMask?.paths, ["spec.default_node_pool.disk_size_gb"]);
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
  assert.doesNotThrow(() => buildClusterUpdate("tenant-a", resolved, { ...form, instanceTypes: "", diskSize: "broken" }, new Set(["displayName"])));
  assert.throws(() => buildClusterUpdate("tenant-a", resolved, { ...form, diskSize: "5" }, new Set(["diskSize"])), /Disk size/);
  assert.throws(() => buildClusterUpdate("tenant-a", resolved, { ...form, diskSize: "-1" }, new Set(["diskSize"])), /whole number/);
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
  const form = { ...clusterForm(resolved), diskSize: "40" };
  const request = buildClusterUpdate("tenant-a", resolved, form, new Set(["diskSize"]));
  const plan = create(PlanSchema, { planId: "plan-1", applicable: true, changes: [{ action: 2, name: "nodegroup" }] });
  const review = bindClusterPlan(request, plan);
  const apply = applyReviewedClusterPlan(review, request);
  assert.equal(apply.planId, "plan-1");
  assert.equal(apply.validateOnly, false);
  assert.deepEqual(apply.cluster, request.cluster);
  assert.deepEqual(apply.updateMask, request.updateMask);
  assert.equal(request.validateOnly, true);
  assert.equal(request.planId, "");
  const changed = buildClusterUpdate("tenant-a", resolved, { ...form, diskSize: "50" }, new Set(["diskSize"]));
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

test("guided create covers tags, node metadata, initial size, maintenance, access and AWS settings", () => {
  const form = {
    ...emptyClusterForm, name: "complete", networkId: network.id, publicCIDRs: "203.0.113.0/24",
    labels: '{"cost-center":"42"}', nodeLabels: '{"team.example.com/pool":"payments"}',
    nodeTaints: '[{"key":"team.example.com/dedicated","value":"payments","effect":1}]', desiredSize: "2",
    maintenanceDays: "Monday, Saturday", maintenanceStart: "23:30", maintenanceDuration: "90m",
    zoneIds: "apne1-az1, apne1-az2", serviceCidr: "172.20.0.0/16", encryptionEnabled: "true",
    encryptionKeyArn: "arn:aws:kms:ap-northeast-1:123456789012:key/key-123",
    awsAmiType: "1", awsAdditionalSecurityGroups: "sg-12345678", awsAuthenticationMode: "2", awsSupportType: "1",
    awsLogGroupEnabled: "true", awsLogRetentionDays: "90", awsLogTypes: "api, audit",
    oidcYaml: 'name: corp\nissuerUrl: https://id.example.com\nclientId: platform\ngroupsClaim: groups\ngroupsPrefix: "oidc:"',
    accessEntriesYaml: '- principalArn: arn:aws:iam::123456789012:role/reader\n  policies:\n    - policyArn: arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy\n      scope: 2\n      namespaces: [payments]',
    addonsYaml: '- name: coredns\n  version: "v1.11.4-eksbuild.2"\n  configurationValues: \'{"replicaCount":3}\'',
  };
  const created = buildCreateCluster("tenant-a", form, network, accounts, "same-request").cluster!;
  assert.deepEqual(created.labels, { "cost-center": "42" });
  assert.deepEqual(created.spec?.defaultNodePool?.labels, { "team.example.com/pool": "payments" });
  assert.equal(created.spec?.defaultNodePool?.taints[0].effect, 1);
  assert.equal(created.spec?.defaultNodePool?.scalingConfig?.desiredSize, 2);
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
    assert.equal(aws.defaultNodePool?.amiType, 1);
    assert.deepEqual(aws.additionalSecurityGroupIds, ["sg-12345678"]);
    assert.equal(aws.accessConfig?.authenticationMode, 2);
    assert.equal(aws.upgradePolicy?.supportType, 1);
    assert.equal(aws.controlPlaneLogGroup?.enabled, true);
    assert.deepEqual(aws.enabledClusterLogTypes, [1, 2]);
    assert.equal(aws.addons[0].configurationValues, '{"replicaCount":3}');
  }
  const hydrated = clusterForm(created);
  assert.equal(JSON.parse(hydrated.nodeTaints)[0].effect, 1);
  const roundtrip = buildCreateCluster("tenant-a", hydrated, network, accounts, "same-request").cluster!;
  assert.deepEqual(toJson(ClusterSchema, roundtrip), toJson(ClusterSchema, created));
});

test("cloud tag updates are a sparse map and preserve all unshown settings", () => {
  const form = { ...clusterForm(resolved), labels: '{"team":"platform"}', oidcYaml: "malformed: [" };
  const request = buildClusterUpdate("tenant-a", resolved, form, new Set(["labels"]));
  assert.deepEqual(request.updateMask?.paths, ["labels"]);
  assert.deepEqual(request.cluster?.labels, { team: "platform" });
  assert.equal(request.cluster?.spec?.provider.case, undefined);
  assert.equal(request.cluster?.spec?.access, undefined);
  assert.deepEqual(resolved.labels, { environment: "production" });
});

test("Kubernetes keys/values, duplicate taints and map keys are validated", () => {
  const form = clusterForm(resolved);
  for (const nodeLabels of ['{"UPPER.example/pool":"ok"}', '{"example.com/":"bad"}', '{"pool":"bad value"}', '{"a":"one","a":"two"}', '{"a":3}']) {
    assert.throws(() => validateClusterFormStep({ ...form, nodeLabels }, 1));
  }
  for (const nodeTaints of ['[{"key":"bad/key/extra","effect":1}]', '[{"key":"x","effect":0}]', '[{"key":"x","effect":1},{"key":"x","effect":1}]']) {
    assert.throws(() => validateClusterFormStep({ ...form, nodeTaints }, 1));
  }
  assert.doesNotThrow(() => validateClusterFormStep({ ...form, nodeLabels: '{"example.com/pool":""}', nodeTaints: '[{"key":"x","effect":1},{"key":"x","effect":3}]' }, 1));
  assert.throws(() => buildClusterUpdate("tenant-a", resolved, { ...form, labels: '{"aws:reserved":"x"}' }, new Set(["labels"])), /AWS tag/);
});

test("immutable create-only fields cannot leak into updates", () => {
  const form = clusterForm(resolved);
  for (const field of ["desiredSize", "zoneIds", "serviceCidr", "awsAdditionalSecurityGroups", "awsAuthenticationMode"]) {
    assert.throws(() => buildClusterUpdate("tenant-a", resolved, form, new Set([field])), /immutable or unsupported/);
  }
  assert.throws(() => buildCreateCluster("tenant-a", { ...emptyClusterForm, name: "new", networkId: network.id, publicCIDRs: "203.0.113.0/24", desiredSize: "5" }, network, accounts, "retry"), /between minimum/);
});

test("atomic grouped masks preserve untouched siblings and clear only on explicit edits", () => {
  const baseline = clone(ClusterSchema, resolved);
  baseline.spec!.maintenanceWindow = { $typeName: "cloud.v1.MaintenanceWindow", days: [1, 3], start: "02:00", duration: "4h" };
  if (baseline.spec!.provider.case === "aws") baseline.spec!.provider.value.controlPlaneLogGroup = { $typeName: "cloud.v1.AwsControlPlaneLogGroup", enabled: true, retentionDays: 90 };
  const form = { ...clusterForm(baseline), maintenanceStart: "03:00", maintenanceDuration: "bad unedited value", awsLogRetentionDays: "30", awsLogGroupEnabled: "false" };
  const update = buildClusterUpdate("tenant-a", baseline, form, new Set(["maintenanceStart", "awsLogRetentionDays"]));
  assert.deepEqual(update.updateMask?.paths, ["spec.aws.control_plane_log_group", "spec.maintenance_window"]);
  assert.deepEqual(update.cluster?.spec?.maintenanceWindow?.days, [1, 3]);
  assert.equal(update.cluster?.spec?.maintenanceWindow?.duration, "4h");
  if (update.cluster?.spec?.provider.case === "aws") {
    assert.equal(update.cluster.spec.provider.value.controlPlaneLogGroup?.enabled, true);
    assert.equal(update.cluster.spec.provider.value.controlPlaneLogGroup?.retentionDays, 30);
  }
  const clear = buildClusterUpdate("tenant-a", baseline, { ...form, maintenanceDays: "", maintenanceStart: "", maintenanceDuration: "" }, new Set(["maintenanceDays", "maintenanceStart", "maintenanceDuration"]));
  assert.deepEqual(clear.updateMask?.paths, ["spec.maintenance_window"]);
  assert.equal(clear.cluster?.spec?.maintenanceWindow, undefined);
  assert.throws(() => buildClusterUpdate("tenant-a", baseline, { ...form, maintenanceDuration: "30m" }, new Set(["maintenanceDuration"])));
  assert.throws(() => buildClusterUpdate("tenant-a", baseline, { ...form, maintenanceStart: "25:00" }, new Set(["maintenanceStart"])));
});

test("encryption transitions keep the key and cannot disable existing encryption", () => {
  const baseline = clone(ClusterSchema, resolved);
  baseline.spec!.controlPlane!.secretsEncryption = { $typeName: "cloud.v1.SecretsEncryption", enabled: true, kmsKeyArn: "arn:aws:kms:ap-northeast-1:123456789012:key/original" };
  const form = clusterForm(baseline);
  assert.throws(() => buildClusterUpdate("tenant-a", baseline, { ...form, encryptionEnabled: "false", encryptionKeyArn: "" }, new Set(["encryptionEnabled", "encryptionKeyArn"])), /cannot be disabled/);
  assert.throws(() => buildClusterUpdate("tenant-a", baseline, { ...form, encryptionKeyArn: "arn:aws:kms:ap-northeast-1:123456789012:key/other" }, new Set(["encryptionKeyArn"])), /another key/);
  const update = buildClusterUpdate("tenant-a", baseline, form, new Set(["encryptionEnabled"]));
  assert.deepEqual(update.updateMask?.paths, ["spec.control_plane.secrets_encryption"]);
  assert.equal(update.cluster?.spec?.controlPlane?.secretsEncryption?.kmsKeyArn, baseline.spec?.controlPlane?.secretsEncryption?.kmsKeyArn);
});

test("typed bounded nested YAML handles launch templates, add-ons and exact plan binding", () => {
  const form = { ...clusterForm(resolved), launchTemplateYaml: 'id: lt-12345678\nversion: "2"', diskSize: "", addonsYaml: "- name: coredns" };
  assert.throws(() => buildClusterUpdate("tenant-a", resolved, form, new Set(["launchTemplateYaml"])), /excludes disk size/);
  const request = buildClusterUpdate("tenant-a", resolved, form, new Set(["launchTemplateYaml", "diskSize", "addonsYaml"]));
  assert.deepEqual(request.updateMask?.paths, ["spec.aws.addons", "spec.aws.default_node_pool.launch_template", "spec.default_node_pool.disk_size_gb"]);
  if (request.cluster?.spec?.provider.case === "aws") {
    assert.equal(request.cluster.spec.provider.value.defaultNodePool?.launchTemplate?.version, "2");
    assert.equal(request.cluster.spec.provider.value.addons[0].version, ""); // no fabricated channel pin
  }
  const review = bindClusterPlan(request, create(PlanSchema, { planId: "advanced", applicable: true }));
  assert.equal(applyReviewedClusterPlan(review, request).planId, "advanced");
  const changed = buildClusterUpdate("tenant-a", resolved, { ...form, launchTemplateYaml: 'id: lt-12345678\nversion: "3"' }, new Set(["launchTemplateYaml", "diskSize", "addonsYaml"]));
  assert.throws(() => applyReviewedClusterPlan(review, changed), /Inputs or etag changed/);
  for (const oidcYaml of ["unexpected: true", "name: corp\nname: other", "a: &a\n  b: *a", `name: ${"a".repeat(65537)}`]) {
    assert.throws(() => buildClusterUpdate("tenant-a", resolved, { ...form, oidcYaml }, new Set(["oidcYaml"])));
  }
});

test("nested message clears and access changes retain sparse masks and sibling defaults", () => {
  const form = { ...clusterForm(resolved), oidcYaml: "", accessEntriesYaml: "", addonsYaml: "", launchTemplateYaml: "" };
  const request = buildClusterUpdate("tenant-a", resolved, form, new Set(["oidcYaml", "accessEntriesYaml", "addonsYaml", "launchTemplateYaml"]));
  assert.deepEqual(request.updateMask?.paths, ["spec.access.access_entries", "spec.access.oidc", "spec.aws.addons", "spec.aws.default_node_pool.launch_template"]);
  assert.equal(request.cluster?.spec?.access?.oidc, undefined);
  assert.deepEqual(request.cluster?.spec?.access?.accessEntries, []);
  if (request.cluster?.spec?.provider.case === "aws") {
    assert.deepEqual(request.cluster.spec.provider.value.addons, []);
    assert.equal(request.cluster.spec.provider.value.defaultNodePool?.launchTemplate, undefined);
    assert.equal(request.cluster.spec.provider.value.irsa, undefined);
  }
  assert.throws(() => buildClusterUpdate("tenant-a", resolved, { ...form, adminPrincipals: "not-an-arn" }, new Set(["adminPrincipals"])), /IAM role/);
  assert.throws(() => buildClusterUpdate("tenant-a", resolved, { ...form, irsa: "typo" }, new Set(["irsa"])), /true, false/);
  assert.throws(() => buildClusterUpdate("tenant-a", resolved, { ...form, awsLogRetentionDays: "4" }, new Set(["awsLogRetentionDays"])));
  assert.throws(() => buildClusterUpdate("tenant-a", resolved, { ...form, addonsYaml: "- name: coredns\n- name: coredns" }, new Set(["addonsYaml"])));
  const baseline = clone(ClusterSchema, resolved);
  baseline.spec!.access = { $typeName: "cloud.v1.AccessSpec", adminPrincipals: ["arn:aws:iam::123456789012:role/admin"], accessEntries: [] };
  assert.throws(() => buildClusterUpdate("tenant-a", baseline, { ...clusterForm(baseline), accessEntriesYaml: "- principalArn: arn:aws:iam::123456789012:role/admin" }, new Set(["accessEntriesYaml"])), /cannot be both/);
});

test("advanced network and addon validation belongs to the rendered Access step", () => {
  const base = { ...emptyClusterForm, name: "wizard", networkId: network.id, publicCIDRs: "203.0.113.0/24" };
  for (const invalid of [{ zoneIds: "invalid-zone" }, { serviceCidr: "172.20.0.0/25" }, { addonsYaml: "- unexpected: true" }]) {
    const form = { ...base, ...invalid };
    assert.doesNotThrow(() => validateClusterFormStep(form, 0));
    assert.doesNotThrow(() => validateClusterFormStep(form, 1));
    assert.throws(() => validateClusterFormStep(form, 2));
    const touched = new Set(Object.keys(invalid));
    assert.doesNotThrow(() => validateClusterFormStep(form, 0, touched));
    assert.doesNotThrow(() => validateClusterFormStep(form, 1, touched));
    assert.throws(() => validateClusterFormStep(form, 2, touched));
  }
  const invalidCompute = { ...base, nodeTaints: '[{"key":"dedicated","effect":0}]' };
  assert.doesNotThrow(() => validateClusterFormStep(invalidCompute, 0));
  assert.throws(() => validateClusterFormStep(invalidCompute, 1));
  assert.doesNotThrow(() => validateClusterFormStep(invalidCompute, 2));
});

test("advanced AWS leaf edits never replace unrelated provider settings", () => {
  for (const [field, value, path] of [
    ["awsAmiType", "2", "spec.aws.default_node_pool.ami_type"],
    ["awsSupportType", "2", "spec.aws.upgrade_policy.support_type"],
    ["awsLogTypes", "1,5", "spec.aws.enabled_cluster_log_types"],
  ] as const) {
    const request = buildClusterUpdate("tenant-a", resolved, { ...clusterForm(resolved), [field]: value }, new Set([field]));
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

test("the default node group's labels, taints and scaling are platform-managed and refused in updates", () => {
  const form = { ...clusterForm(resolved), minSize: "2", maxSize: "9", nodeLabels: '{"pool":"jobs"}', nodeTaints: '[{"key":"dedicated","effect":1}]' };
  for (const field of ["minSize", "maxSize", "nodeLabels", "nodeTaints"]) {
    assert.throws(() => buildClusterUpdate("tenant-a", resolved, form, new Set([field])), /managed by the platform/);
    assert.throws(() => buildClusterUpdate("tenant-a", resolved, form, new Set(["displayName", field])), /managed by the platform/);
  }
  const editable = buildClusterUpdate("tenant-a", resolved, { ...form, capacityType: "2", instanceTypes: "m6i.xlarge" }, new Set(["capacityType", "instanceTypes"]));
  assert.deepEqual(editable.updateMask?.paths, ["spec.default_node_pool.capacity_type", "spec.default_node_pool.instance_types"]);
});

test("create still sets the default node group's metadata and scaling", () => {
  const form = { ...emptyClusterForm, name: "fresh", networkId: network.id, publicCIDRs: "203.0.113.0/24", minSize: "2", maxSize: "4", nodeLabels: '{"pool":"jobs"}' };
  const pool = buildCreateCluster("tenant-a", form, network, accounts, "r1").cluster?.spec?.defaultNodePool;
  assert.equal(pool?.scalingConfig?.minSize, 2);
  assert.deepEqual(pool?.labels, { pool: "jobs" });
});
