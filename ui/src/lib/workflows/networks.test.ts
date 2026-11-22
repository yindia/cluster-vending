import assert from "node:assert/strict";
import test from "node:test";
import { clone, create, equals } from "@bufbuild/protobuf";
import {
  FlowLogTrafficType, NatGatewayMode, NetworkSchema, NetworkState,
} from "../../gen/cloud/v1/network_pb";
import { UpdateNetworkRequestSchema } from "../../gen/cloud/v1/network_service_pb";
import { CloudAccountSchema } from "../../gen/cloud/v1/tenant_service_pb";
import { PlanSchema, PlannedAction } from "../../gen/cloud/v1/plan_pb";
import { Tier } from "../../gen/cloud/v1/tier_pb";
import { Disposition, ImportPhase } from "../../gen/cloud/v1/import_pb";
import { ClusterSchema } from "../../gen/cloud/v1/cluster_pb";
import { ListClustersResponseSchema } from "../../gen/cloud/v1/cluster_service_pb";
import {
  buildCreateNetwork, buildImportNetwork, buildNetworkUpdate, buildTakeOverNetwork, canApplyNetworkPlan, createAttempt, enumLabel,
  networkDraft, networkIsReady, networkProvider, newNetworkDraft,
  networkTakeoverBlockReason, networkTakeoverKey, newNetworkImportDraft,
  reviewedNetworkApply, reviewNetworkUpdate,
  validateNetworkStep, validateNetworkImportStep, collectNetworkClusters, filterNetworkInventory, networkClusterRows,
} from "./networks";

const account = create(CloudAccountSchema, { id: "account-1", tenantId: "acme", provider: { case: "aws", value: { accountId: "123456789012" } } });
const draft = () => ({ ...newNetworkDraft(), accountId: account.id, name: "prod", region: "us-east-1" });
const stored = () => create(NetworkSchema, {
  id: "network-1", name: "prod", tenantId: "acme", etag: "etag-1", generation: BigInt(3),
  displayName: "Production", labels: { team: "platform", env: "prod" },
  spec: { bootstrapId: account.id, region: "us-east-1", tier: Tier.TIER_UNSPECIFIED,
    layout: { cidrBlock: "10.0.0.0/16", natGatewayMode: NatGatewayMode.PER_ZONE, availabilityZoneCount: 0,
      subnets: [{ role: 2, cidrBlock: "10.0.0.0/20", zoneId: "use1-az1" }] } },
  status: { state: NetworkState.READY, observedGeneration: BigInt(3) },
});
const applicablePlan = () => create(PlanSchema, { planId: "reviewed-hash", applicable: true, changes: [
  { action: PlannedAction.DELETE, kind: "FlowLog", name: "prod-flow", retainsCloudResource: true, requiresReplacement: true },
] });

test("create uses selected account oneof, generated messages, and explicit defaults", () => {
  const request = buildCreateNetwork("acme", draft(), account, "request-1");
  assert.equal(request.requestId, "request-1");
  assert.equal(request.network?.spec?.bootstrapId, account.id);
  assert.equal(request.network?.spec?.layout?.availabilityZoneCount, 3);
  assert.equal(request.network?.spec?.layout?.natGatewayMode, NatGatewayMode.SINGLE);
  assert.equal(request.network?.spec?.tier, Tier.TIER_0);
  assert.equal(request.network?.spec?.flowLogs, undefined);
});

test("unknown, missing, mismatched and cross-tenant accounts cannot default to AWS", () => {
  const unknown = create(CloudAccountSchema, { id: account.id });
  assert.equal(networkProvider(unknown), undefined);
  assert.equal(networkProvider(undefined), undefined);
  assert.equal(networkProvider(account), "aws");
  for (const value of [unknown, undefined, create(CloudAccountSchema, { ...account, id: "different" }), create(CloudAccountSchema, { ...account, tenantId: "other" })]) {
    assert.throws(() => buildCreateNetwork("acme", draft(), value, "request-1"), /supported provider/);
  }
});

test("create retries retain an ID until inputs or tenant change", () => {
  let calls = 0;
  const uuid = () => `uuid-${++calls}`;
  const first = createAttempt("acme", draft(), undefined, uuid);
  assert.strictEqual(createAttempt("acme", draft(), first, uuid), first);
  assert.equal(calls, 1);
  const changed = createAttempt("acme", { ...draft(), displayName: "Changed" }, first, uuid);
  assert.notEqual(changed.requestId, first.requestId);
  assert.notEqual(createAttempt("other", draft(), first, uuid).requestId, first.requestId);
});

test("explicit subnet JSON uses generated enum codec and suppresses AZ count", () => {
  const value = { ...draft(), zoneCount: "not used", subnetsJson: JSON.stringify([
    { role: "SUBNET_ROLE_PUBLIC", cidrBlock: "10.0.0.0/24", zoneId: "use1-az1" },
    { role: "SUBNET_ROLE_PRIVATE", cidrBlock: "10.0.16.0/20", zoneId: "use1-az1" },
    { role: "SUBNET_ROLE_PRIVATE", cidrBlock: "10.0.32.0/20", zoneId: "use1-az2" },
  ]) };
  const layout = buildCreateNetwork("acme", value, account, "request-1").network!.spec!.layout!;
  assert.equal(layout.availabilityZoneCount, 0);
  assert.equal(layout.subnets[0].role, 1);
  assert.equal(layout.subnets.length, 3);
  assert.throws(() => buildCreateNetwork("acme", { ...value, natMode: NatGatewayMode.PER_ZONE }, account, "request-1"), /PUBLIC subnet in every/);
  assert.throws(() => buildCreateNetwork("acme", { ...value, subnetsJson: value.subnetsJson.replace("SUBNET_ROLE_PUBLIC", "MYSTERY") }, account, "request-1"));
  assert.throws(() => buildCreateNetwork("acme", { ...value, subnetsJson: value.subnetsJson.replace("10.0.32.0/20", "10.0.16.0/20") }, account, "request-1"), /overlap/);
  assert.throws(() => buildCreateNetwork("acme", { ...value, subnetsJson: value.subnetsJson.replace("10.0.32.0/20", "10.1.32.0/20") }, account, "request-1"), /inside/);
});

test("invalid names, CIDRs, layouts, NAT and flow log values fail before a request", () => {
  for (const patch of [
    { name: "Invalid_Name" }, { name: "a".repeat(41) }, { region: "" },
    { cidrBlock: "10.0.0.1/16" }, { cidrBlock: "999.0.0.0/16" }, { cidrBlock: "10.0.0.0/24" },
    { zoneCount: "1" }, { zoneCount: "7" }, { natMode: NatGatewayMode.REGIONAL },
    { subnetsJson: "[]" }, { subnetsJson: "{}" },
    { flowEnabled: true, retentionDays: "31" }, { flowEnabled: true, aggregationSeconds: "300" },
  ]) assert.throws(() => buildCreateNetwork("acme", { ...draft(), ...patch }, account, "request-1"), JSON.stringify(patch));
});

test("label codec rejects non-string values, arrays and invalid or reserved keys", () => {
  for (const labelsJson of ['{"count":2}', "[]", '"label"', "null", '{"":"value"}', '{"aws:owner":"x"}', '{"emoji":"💻"}']) {
    assert.throws(() => buildCreateNetwork("acme", { ...draft(), labelsJson }, account, "request-1"), labelsJson);
  }
});

test("metadata update masks contain only changes and preserve every immutable field", () => {
  const before = stored();
  const untouched = clone(NetworkSchema, before);
  const value = { ...networkDraft(before), displayName: "New name", name: "must-not-change", region: "different", accountId: "different", cidrBlock: "172.16.0.0/16", natMode: NatGatewayMode.SINGLE };
  const request = buildNetworkUpdate("acme", before, value);
  assert.deepEqual(request.updateMask?.paths, ["display_name"]);
  assert.equal(request.validateOnly, true);
  assert.equal(request.planId, "");
  assert.equal(request.network?.etag, before.etag);
  assert.deepEqual(request.network?.spec, before.spec);
  assert.equal(request.network?.name, before.name);
  assert(equals(NetworkSchema, before, untouched), "building a request must not mutate cached input");
});

test("label order and formatting produce no updates; unchanged defaults stay absent", () => {
  const before = stored();
  assert.throws(() => buildNetworkUpdate("acme", before, { ...networkDraft(before), labelsJson: '{"env":"prod","team":"platform"}' }), /No mutable/);
  const request = buildNetworkUpdate("acme", before, { ...networkDraft(before), tier: Tier.TIER_2 });
  assert.deepEqual(request.updateMask?.paths, ["spec.tier"]);
  assert.equal(request.network?.spec?.flowLogs, undefined);
});

test("changed labels and flow logs use precise parent masks", () => {
  const before = stored();
  const request = buildNetworkUpdate("acme", before, { ...networkDraft(before), labelsJson: "{}", flowEnabled: true });
  assert.deepEqual(request.updateMask?.paths, ["labels", "spec.flow_logs"]);
  assert.deepEqual(request.network?.labels, {});
  assert.equal(request.network?.spec?.flowLogs?.enabled, true);
  assert.equal(request.network?.spec?.flowLogs?.trafficType, FlowLogTrafficType.ALL);
});

test("existing flow logs allow retention edits but require off/on for immutable settings", () => {
  const before = stored();
  before.spec!.flowLogs = buildCreateNetwork("acme", { ...draft(), flowEnabled: true }, account, "request-1").network!.spec!.flowLogs;
  const value = networkDraft(before);
  assert.deepEqual(buildNetworkUpdate("acme", before, { ...value, retentionDays: "90" }).updateMask?.paths, ["spec.flow_logs"]);
  assert.throws(() => buildNetworkUpdate("acme", before, { ...value, trafficType: FlowLogTrafficType.REJECT }), /Turn flow logs off/);
  assert.throws(() => buildNetworkUpdate("acme", before, { ...value, aggregationSeconds: "60" }), /Turn flow logs off/);
  assert.equal(buildNetworkUpdate("acme", before, { ...value, flowEnabled: false }).network?.spec?.flowLogs?.enabled, false);
});

test("editing refuses missing etags, imported states and imported flow log changes", () => {
  const before = stored();
  assert.throws(() => buildNetworkUpdate("acme", { ...before, etag: "" }, { ...networkDraft(before), displayName: "Changed" }), /etag/);
  before.status!.state = NetworkState.IMPORTED;
  assert.throws(() => buildNetworkUpdate("acme", before, { ...networkDraft(before), displayName: "Changed" }), /current state/);
  before.status!.state = NetworkState.READY;
  before.status!.import = { $typeName: "cloud.v1.ImportStatus", phase: 0, resources: [] };
  assert.throws(() => buildNetworkUpdate("acme", before, { ...networkDraft(before), flowEnabled: true }), /carried over/);
});

test("apply sends an isolated copy of exactly the reviewed request with only plan controls changed", () => {
  const before = stored();
  const request = buildNetworkUpdate("acme", before, { ...networkDraft(before), displayName: "Reviewed" });
  const original = clone(UpdateNetworkRequestSchema, request);
  const plan = applicablePlan();
  const review = reviewNetworkUpdate(request, plan);
  request.network!.displayName = "Changed after preview";
  plan.planId = "Changed after preview";
  const applied = reviewedNetworkApply(review, before.etag);
  const expected = clone(UpdateNetworkRequestSchema, original);
  expected.validateOnly = false; expected.planId = "reviewed-hash";
  assert(equals(UpdateNetworkRequestSchema, applied, expected));
  assert.equal(review.request.validateOnly, true);
  assert.equal(review.plan.planId, "reviewed-hash");
  applied.network!.displayName = "Mutated result";
  assert.equal(review.request.network!.displayName, "Reviewed");
});

test("missing, inapplicable, unknown, unspecified and rejected plans cannot apply", () => {
  assert.throws(() => reviewNetworkUpdate(create(UpdateNetworkRequestSchema), undefined), /no plan/);
  const request = buildNetworkUpdate("acme", stored(), { ...networkDraft(stored()), displayName: "Changed" });
  const plans = [create(PlanSchema, { applicable: true }), create(PlanSchema, { planId: "hash", applicable: false }), ...[PlannedAction.UNSPECIFIED, PlannedAction.REJECTED_IMMUTABLE, 99].map((action) => create(PlanSchema, { planId: "hash", applicable: true, changes: [{ action }] }))];
  for (const plan of plans) {
    assert.equal(canApplyNetworkPlan(plan), false);
    assert.throws(() => reviewedNetworkApply(reviewNetworkUpdate(request, plan), "etag-1"), /cannot be applied/);
  }
  assert.equal(canApplyNetworkPlan(create(PlanSchema, { applicable: true, planId: "metadata-only" })), true);
});

test("stale etags reject locally without rebuilding or silently replanning", () => {
  const request = buildNetworkUpdate("acme", stored(), { ...networkDraft(stored()), displayName: "Changed" });
  const review = reviewNetworkUpdate(request, applicablePlan());
  for (const etag of ["", "etag-2"]) assert.throws(() => reviewedNetworkApply(review, etag), /changed after review/);
  assert.equal(review.request.network?.etag, "etag-1");
});

test("API acknowledgement is separate from generation-aware readiness; unknown enums remain visible", () => {
  const network = stored();
  assert.equal(networkIsReady(network), true);
  network.status!.observedGeneration = BigInt(2);
  assert.equal(networkIsReady(network), false);
  network.status!.observedGeneration = BigInt(3);
  network.status!.state = NetworkState.UPDATING;
  assert.equal(networkIsReady(network), false);
  assert.equal(enumLabel(NetworkState, NetworkState.PROVISIONING), "Provisioning");
  assert.equal(enumLabel(NetworkState, 99), "Unknown (99)");
});

const importDraft = () => ({ ...newNetworkImportDraft(), accountId: account.id, name: "existing", displayName: "Existing VPC", region: "us-east-1", vpcId: "vpc-0123456789abcdef0", subnetIds: "subnet-01234567, subnet-89abcdef\nsubnet-0123456789abcdef0" });
const plannedImport = () => {
  const network = stored();
  network.status!.state = NetworkState.IMPORTED;
  network.status!.import = { $typeName: "cloud.v1.ImportStatus", phase: ImportPhase.PLANNED, resources: [
    { $typeName: "cloud.v1.ImportedResource", apiVersion: "ec2.aws.m.upbound.io/v1beta1", kind: "VPC", name: "prod-vpc", externalName: "vpc-01234567", specPath: "", disposition: Disposition.MATCHES, detail: "", requiresReplacement: false },
  ] };
  return network;
};
const ownershipConsent = (network = plannedImport()) => ({ reviewKey: networkTakeoverKey(network), ownershipConfirmation: `take over ${network.name}`, acknowledgeNotImported: false });

test("import generates the correct observe-only provider oneof without creation spec or update masks", () => {
  const request = buildImportNetwork("acme", importDraft(), account, "import-request");
  assert.equal(request.$typeName, "cloud.v1.ImportNetworkRequest");
  assert.equal(request.tenantId, "acme");
  assert.equal(request.bootstrapId, account.id);
  assert.equal(request.requestId, "import-request");
  assert.equal(request.name, "existing");
  assert.equal(request.displayName, "Existing VPC");
  assert.equal(request.region, "us-east-1");
  assert.equal(request.provider.case, "aws");
  assert.deepEqual(request.provider.value?.subnetIds, ["subnet-01234567", "subnet-89abcdef", "subnet-0123456789abcdef0"]);
  assert.equal(request.provider.value?.vpcId, "vpc-0123456789abcdef0");
  for (const field of ["network", "spec", "updateMask", "acknowledgeNotImported"]) assert.equal(field in request, false);
});

test("import validation follows AWS IDL limits and rejects unsupported accounts", () => {
  for (const patch of [
    { name: "Bad_Name" }, { name: "n".repeat(41) }, { displayName: "d".repeat(129) },
    { region: "not-a-region" }, { region: "US-EAST-1" }, { vpcId: "vpc-ABCDEF12" },
    { vpcId: "vpc-1234567" }, { vpcId: `vpc-${"a".repeat(18)}` },
    { subnetIds: "" }, { subnetIds: "subnet-1234567" }, { subnetIds: "subnet-01234567,subnet-01234567" },
    { subnetIds: Array.from({ length: 33 }, (_, i) => `subnet-${i.toString(16).padStart(8, "0")}`).join(" ") },
  ]) assert.throws(() => buildImportNetwork("acme", { ...importDraft(), ...patch }, account, "request"), JSON.stringify(patch));
  for (const value of [undefined, create(CloudAccountSchema, { id: account.id }), create(CloudAccountSchema, { ...account, tenantId: "other" }), create(CloudAccountSchema, { ...account, id: "other" })]) assert.throws(() => buildImportNetwork("acme", importDraft(), value, "request"), /supported provider/);
  assert.equal(buildImportNetwork("acme", { ...importDraft(), region: "us-gov-west-1" }, account, "request").region, "us-gov-west-1");
});

test("import retries reuse idempotency IDs until inputs change", () => {
  let sequence = 0;
  const uuid = () => `import-${++sequence}`;
  const value = importDraft();
  const attempt = createAttempt("acme", value, undefined, uuid);
  assert.strictEqual(createAttempt("acme", value, attempt, uuid), attempt);
  assert.notEqual(createAttempt("acme", { ...value, subnetIds: "subnet-01234567" }, attempt, uuid).requestId, attempt.requestId);
  assert.notEqual(createAttempt("other", value, attempt, uuid).requestId, attempt.requestId);
});

test("takeover generates a current-etag request only after typed ownership confirmation", () => {
  const network = plannedImport();
  const original = clone(NetworkSchema, network);
  const request = buildTakeOverNetwork("acme", network, account, ownershipConsent(network));
  assert.equal(request.$typeName, "cloud.v1.TakeOverNetworkRequest");
  assert.equal(request.tenantId, "acme");
  assert.equal(request.networkId, network.id);
  assert.equal(request.etag, network.etag);
  assert.equal(request.acknowledgeNotImported, false);
  assert.equal("updateMask" in request, false);
  assert(equals(NetworkSchema, network, original));
  for (const text of ["", network.name, "take over another", `TAKE OVER ${network.name}`]) assert.throws(() => buildTakeOverNetwork("acme", network, account, { ...ownershipConsent(network), ownershipConfirmation: text }), /Type take over/);
});

test("takeover requires explicit NOT_IMPORTED acknowledgment and preserves its exact wire field", () => {
  const network = plannedImport();
  network.status!.import!.resources.push({ ...network.status!.import!.resources[0], name: "external-owner", disposition: Disposition.NOT_IMPORTED });
  const confirmation = ownershipConsent(network);
  assert.throws(() => buildTakeOverNetwork("acme", network, account, confirmation), /Acknowledge/);
  assert.equal(buildTakeOverNetwork("acme", network, account, { ...confirmation, acknowledgeNotImported: true }).acknowledgeNotImported, true);
});

test("takeover refuses discovery, blocked, converging, converged, unknown and absent import phases", () => {
  for (const phase of [ImportPhase.UNSPECIFIED, ImportPhase.DISCOVERING, ImportPhase.BLOCKED, ImportPhase.CONVERGING, ImportPhase.CONVERGED, 99]) {
    const network = plannedImport(); network.status!.import!.phase = phase;
    assert.throws(() => buildTakeOverNetwork("acme", network, account, ownershipConsent(network)), /PLANNED import/);
  }
  const network = plannedImport(); network.status!.import = undefined;
  assert.throws(() => buildTakeOverNetwork("acme", network, account, ownershipConsent(network)), /PLANNED import/);
  for (const state of [NetworkState.READY, NetworkState.FAILED, NetworkState.DELETING, NetworkState.PROVISIONING, NetworkState.UPDATING]) {
    const value = plannedImport(); value.status!.state = state;
    assert.throws(() => buildTakeOverNetwork("acme", value, account, ownershipConsent(value)), /IMPORTED network/);
  }
});

test("takeover refuses blocking or unknown resource dispositions even in PLANNED phase", () => {
  for (const disposition of [Disposition.BLOCKING, Disposition.UNSPECIFIED, 99]) {
    const network = plannedImport(); network.status!.import!.resources[0].disposition = disposition;
    assert(networkTakeoverBlockReason("acme", network, account));
    assert.throws(() => buildTakeOverNetwork("acme", network, account, { ...ownershipConsent(network), acknowledgeNotImported: true }));
  }
});

test("takeover confirmation expires on etag, generation, account, identity or plan edits", () => {
  const before = plannedImport();
  const confirmation = ownershipConsent(before);
  const changes = [
    (value: typeof before) => { value.etag = "etag-2"; },
    (value: typeof before) => { value.generation += BigInt(1); },
    (value: typeof before) => { value.id = "another-network"; },
    (value: typeof before) => { value.name = "renamed"; },
    (value: typeof before) => { value.status!.import!.resources[0].detail = "New replacement details"; },
    (value: typeof before) => { value.status!.import!.resources[0].requiresReplacement = true; },
  ];
  for (const change of changes) {
    const network = clone(NetworkSchema, before); change(network);
    assert.throws(() => buildTakeOverNetwork("acme", network, account, confirmation), /plan changed/);
  }
  const observed = clone(NetworkSchema, before);
  observed.status!.message = "New observation";
  observed.status!.observedGeneration = BigInt(0);
  assert.equal(networkTakeoverKey(observed), confirmation.reviewKey);
  assert.equal(buildTakeOverNetwork("acme", observed, account, confirmation).etag, before.etag);
});

test("takeover refuses missing etags and mismatched tenant or provider account", () => {
  const network = plannedImport();
  assert.throws(() => buildTakeOverNetwork("acme", { ...network, etag: "" }, account, ownershipConsent(network)), /etag/);
  assert.throws(() => buildTakeOverNetwork("other", network, account, ownershipConsent(network)), /tenant/);
  for (const value of [undefined, create(CloudAccountSchema, { id: account.id }), create(CloudAccountSchema, { ...account, id: "different" }), create(CloudAccountSchema, { ...account, tenantId: "different" })]) assert.throws(() => buildTakeOverNetwork("acme", network, value, ownershipConsent(network)), /supported takeover provider/);
});

test("identity step validates visible fields without depending on unfinished layout or flow fields", () => {
  const value = { ...draft(), cidrBlock: "", zoneCount: "", retentionDays: "", aggregationSeconds: "", flowEnabled: true };
  assert.doesNotThrow(() => validateNetworkStep("acme", value, "identity", account));
  for (const patch of [{ accountId: "" }, { name: "" }, { region: "" }, { labelsJson: "invalid" }]) assert.throws(() => validateNetworkStep("acme", { ...value, ...patch }, "identity", account));
  assert.throws(() => buildCreateNetwork("acme", value, account, "request"));
});

test("layout and flow steps validate only their controls and disabled flow logs ignore hidden fields", () => {
  const value = newNetworkDraft();
  assert.doesNotThrow(() => validateNetworkStep("acme", value, "layout"));
  assert.throws(() => validateNetworkStep("acme", { ...value, cidrBlock: "10.0.0.1/16" }, "layout"), /boundary/);
  assert.doesNotThrow(() => validateNetworkStep("acme", { ...value, retentionDays: "invalid" }, "flowLogs"));
  assert.throws(() => validateNetworkStep("acme", { ...value, flowEnabled: true, retentionDays: "invalid" }, "flowLogs"), /retention/);
  assert.doesNotThrow(() => validateNetworkStep("acme", { ...value, accountId: "", name: "", cidrBlock: "", flowEnabled: true }, "flowLogs"));
});

test("edit metadata step ignores immutable layout and unchanged flow defaults", () => {
  const value = { ...networkDraft(stored()), cidrBlock: "", accountId: "", region: "", name: "", retentionDays: "" };
  assert.doesNotThrow(() => validateNetworkStep("acme", value, "metadata"));
  assert.throws(() => validateNetworkStep("acme", { ...value, labelsJson: "[]" }, "metadata"), /JSON object/);
});

test("import wizard identity and resource steps validate independently; final request validates both", () => {
  const value = { ...importDraft(), vpcId: "", subnetIds: "" };
  assert.doesNotThrow(() => validateNetworkImportStep("acme", value, "identity", account));
  assert.throws(() => validateNetworkImportStep("acme", { ...value, name: "" }, "identity", account), /Name/);
  assert.throws(() => validateNetworkImportStep("acme", value, "resources"), /VPC ID/);
  assert.doesNotThrow(() => validateNetworkImportStep("acme", { ...importDraft(), accountId: "", name: "", region: "" }, "resources"));
  assert.throws(() => buildImportNetwork("acme", value, account, "request"), /VPC ID/);
});

test("flow logs can be disabled even after invalid text was entered in hidden retention fields", () => {
  const before = stored();
  before.spec!.flowLogs = buildCreateNetwork("acme", { ...draft(), flowEnabled: true }, account, "request").network!.spec!.flowLogs;
  const request = buildNetworkUpdate("acme", before, { ...networkDraft(before), flowEnabled: false, retentionDays: "invalid" });
  assert.equal(request.network?.spec?.flowLogs?.enabled, false);
  assert.deepEqual(request.updateMask?.paths, ["spec.flow_logs"]);
});

test("related clusters follow all pages and filter network membership while retaining names", async () => {
  const tokens: string[] = [];
  const payments = create(ClusterSchema, { id: "payments-id", name: "payments", spec: { network: { networkId: "network-1" } } });
  const clusters = await collectNetworkClusters("network-1", async (token) => {
    tokens.push(token);
    return token ? create(ListClustersResponseSchema, { clusters: [payments] }) : create(ListClustersResponseSchema, { nextPageToken: "next", clusters: [create(ClusterSchema, { id: "other", spec: { network: { networkId: "network-2" } } })] });
  }, new AbortController().signal);
  assert.deepEqual(tokens, ["", "next"]);
  assert.deepEqual(clusters.map(({ name }) => name), ["payments"]);
});

test("related cluster scans refuse repeated tokens, propagate unavailable errors and abort stale loads", async () => {
  await assert.rejects(collectNetworkClusters("network-1", async () => create(ListClustersResponseSchema, { nextPageToken: "same" }), new AbortController().signal), /repeated a page token/);
  await assert.rejects(collectNetworkClusters("network-1", async () => { throw new Error("Cluster API unavailable"); }, new AbortController().signal), /unavailable/);
  const controller = new AbortController(); controller.abort();
  let called = false;
  await assert.rejects(collectNetworkClusters("network-1", async () => { called = true; return create(ListClustersResponseSchema); }, controller.signal));
  assert.equal(called, false);
});

test("inventory search covers all rows by name, kind and cloud ID with case-insensitive combined terms", () => {
  const resources = [
    { name: "prod-vpc", kind: "VPC", externalName: "vpc-01234567" },
    { name: "prod-private", kind: "Subnet", externalName: "subnet-89abcdef", paused: true },
    { name: "prod-nat", kind: "NATGateway", externalName: "nat-01234567" },
  ];
  assert.equal(filterNetworkInventory(resources, "").length, 3);
  assert.equal(filterNetworkInventory(resources, "   ").length, 3);
  assert.deepEqual(filterNetworkInventory(resources, "SUBNET 89ABCDEF"), [resources[1]]);
  assert.deepEqual(filterNetworkInventory(resources, "prod-nat"), [resources[2]]);
  assert.deepEqual(filterNetworkInventory(resources, "vpc-01234567"), [resources[0]]);
  assert.deepEqual(filterNetworkInventory(resources, "no-match"), []);
  assert.equal(resources.length, 3);
});

const relatedCluster = (id: string, name = "prod-eks", networkId = "network-1") => create(ClusterSchema, {
  id, name, spec: { network: { networkId } },
});
const firstClusterId = "01a1dfca-f5dd-7144-97b5-baeefe0e4a40";
const secondClusterId = "01a1dfca-f5dd-7144-97b5-baeefe0e4a41";

test("authoritative clusters do not duplicate status names or UUID aliases", () => {
  const cluster = relatedCluster(firstClusterId);
  const rows = networkClusterRows("network-1", ["prod-eks", firstClusterId, "prod-eks"], [cluster, cluster]);
  assert.equal(rows.length, 1);
  assert.equal(rows[0].cluster?.id, firstClusterId);
  assert.equal(rows[0].key, `cluster:${firstClusterId}`);
});

test("an authoritative empty list suppresses stale network status references", () => {
  assert.deepEqual(networkClusterRows("network-1", ["prod-eks", firstClusterId], []), []);
});

test("same-named clusters on other networks are excluded instead of resolved by name", () => {
  const own = relatedCluster(firstClusterId);
  const unrelated = relatedCluster(secondClusterId, own.name, "network-2");
  assert.deepEqual(networkClusterRows("network-1", ["prod-eks"], [own, unrelated]).map((row) => row.cluster?.id), [firstClusterId]);
  assert.deepEqual(networkClusterRows("network-1", ["prod-eks"], [unrelated]), []);
  const fallback = networkClusterRows("network-1", ["prod-eks"], [unrelated], false);
  assert.equal(fallback.length, 1);
  assert.equal(fallback[0].cluster, undefined, "an unrelated name cannot resolve to an actual cluster");
});

test("legitimate clusters with identical names and different UUIDs remain distinct", () => {
  const clusters = [relatedCluster(firstClusterId), relatedCluster(secondClusterId)];
  for (const authoritative of [true, false]) {
    const rows = networkClusterRows("network-1", ["prod-eks", firstClusterId, secondClusterId], clusters, authoritative);
    assert.deepEqual(rows.map((row) => row.cluster?.id), [firstClusterId, secondClusterId]);
    assert.equal(new Set(rows.map((row) => row.key)).size, 2);
  }
});

test("loading and unavailable fallbacks retain raw references without inventing cluster IDs", () => {
  const rows = networkClusterRows("network-1", ["prod-eks", " prod-eks ", "", firstClusterId], undefined);
  assert.deepEqual(rows.map((row) => row.reference), ["prod-eks", firstClusterId]);
  assert(rows.every((row) => row.cluster === undefined));
  assert(rows.every((row) => row.key.startsWith("reference:")));
});

test("known fallback data resolves legacy names and UUIDs to one cluster row", () => {
  const rows = networkClusterRows("network-1", ["prod-eks", firstClusterId, "unknown-name"], [relatedCluster(firstClusterId)], false);
  assert.equal(rows.length, 2);
  assert.equal(rows[0].cluster?.id, firstClusterId);
  assert.equal(rows[1].reference, "unknown-name");
  assert.equal(rows[1].cluster, undefined);
});
