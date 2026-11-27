import assert from "node:assert/strict";
import test from "node:test";
import { clone, create, equals } from "@bufbuild/protobuf";
import { NatGatewayMode, NetworkSchema, NetworkState } from "../../gen/cloud/v1/network_pb";
import { CloudAccountSchema } from "../../gen/cloud/v1/tenant_service_pb";
import { Tier } from "../../gen/cloud/v1/tier_pb";
import { Disposition, ImportPhase } from "../../gen/cloud/v1/import_pb";
import { ClusterSchema } from "../../gen/cloud/v1/cluster_pb";
import { ListClustersResponseSchema } from "../../gen/cloud/v1/cluster_service_pb";
import { buildTakeOverNetwork, enumLabel, networkIsReady, networkTakeoverBlockReason, networkTakeoverKey, collectNetworkClusters, filterNetworkInventory, networkClusterRows } from "./networks";

const account = create(CloudAccountSchema, { id: "account-1", tenantId: "acme", provider: { case: "aws", value: { accountId: "123456789012" } } });
const stored = () => create(NetworkSchema, {
  id: "network-1", name: "prod", tenantId: "acme", etag: "etag-1", generation: BigInt(3),
  displayName: "Production", labels: { team: "platform", env: "prod" },
  spec: { bootstrapId: account.id, region: "us-east-1", tier: Tier.TIER_UNSPECIFIED,
    layout: { cidrBlock: "10.0.0.0/16", natGatewayMode: NatGatewayMode.PER_ZONE, availabilityZoneCount: 0,
      subnets: [{ role: 2, cidrBlock: "10.0.0.0/20", zoneId: "use1-az1" }] } },
  status: { state: NetworkState.READY, observedGeneration: BigInt(3) },
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

const plannedImport = () => {
  const network = stored();
  network.status!.state = NetworkState.IMPORTED;
  network.status!.import = { $typeName: "cloud.v1.ImportStatus", phase: ImportPhase.PLANNED, resources: [
    { $typeName: "cloud.v1.ImportedResource", apiVersion: "ec2.aws.m.upbound.io/v1beta1", kind: "VPC", name: "prod-vpc", externalName: "vpc-01234567", specPath: "", disposition: Disposition.MATCHES, detail: "", requiresReplacement: false },
  ] };
  return network;
};
const ownershipConsent = (network = plannedImport()) => ({ reviewKey: networkTakeoverKey(network), ownershipConfirmation: `take over ${network.name}`, acknowledgeNotImported: false });

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
