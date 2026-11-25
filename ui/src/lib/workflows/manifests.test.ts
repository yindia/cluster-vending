import assert from "node:assert/strict";
import test from "node:test";
import { readFileSync } from "node:fs";
import { stringify } from "yaml";
import { fromJsonString } from "@bufbuild/protobuf";
import { CreateClusterRequestSchema } from "../../gen/cloud/v1/cluster_service_pb";
import { CreateNetworkRequestSchema } from "../../gen/cloud/v1/network_service_pb";
import { BootstrapTenantRequestSchema } from "../../gen/cloud/v1/tenant_service_pb";
import type { BrowserClients } from "../api/client";
import { applyManifest, manifestTemplate, parseManifest, reviewManifest, type ManifestKind } from "./manifests";

const id = "00000000-0000-4000-8000-000000000001";
const envelope = (kind: ManifestKind, spec: object, metadata = {}) => ({ apiVersion: "cluster-vending.io/v1", kind, metadata: { name: "production", tenant: "acme", ...metadata }, spec });
const account = envelope("CloudAccount", { aws: { accountId: "111122223333", role: "cluster-vending-provisioner" } });
const network = envelope("Network", { bootstrapId: id, region: "us-west-1", tier: "TIER_2", flowLogs: { enabled: true, retentionDays: 90 }, layout: { cidrBlock: "10.2.0.0/16", availabilityZoneCount: 2 } }, { labels: { team: "platform" } });

test("starter examples validate once their reference placeholders are replaced", () => {
  for (const kind of ["CloudAccount", "Network", "Cluster"] as const) {
    const text = manifestTemplate(kind, "acme").replace("REPLACE_WITH_ACCOUNT_UUID", id).replace("REPLACE_WITH_NETWORK_UUID", id);
    assert.equal(parseManifest(text, kind, "acme", id).kind, kind);
  }
});

test("CLI example manifests use the same full generated spec codecs", () => {
  for (const [file, kind] of [["cloudaccount", "CloudAccount"], ["network", "Network"], ["cluster", "Cluster"]] as const) {
    const review = parseManifest(readFileSync(new URL(`../../../../examples/${file}.yaml`, import.meta.url), "utf8"), kind, "acme", id);
    assert.equal(review.kind, kind);
    assert.equal(review.tenantId, "acme");
  }
});
test("network advanced fields and cluster optional false survive the exact review snapshot", () => {
  const review = parseManifest(stringify(network), "Network", "acme", id);
  const request = fromJsonString(CreateNetworkRequestSchema, review.requestJson);
  assert.equal(request.network?.spec?.flowLogs?.retentionDays, 90);
  assert.equal(request.network?.spec?.layout?.cidrBlock, "10.2.0.0/16");
  assert.deepEqual(request.network?.labels, { team: "platform" });
  assert.equal(request.requestId, id);
  const value = envelope("Cluster", { region: "us-west-1", network: { networkId: id }, controlPlane: { endpointAccess: { endpointPublicAccess: false, endpointPrivateAccess: true } }, aws: { irsa: false } });
  const cluster = fromJsonString(CreateClusterRequestSchema, parseManifest(stringify(value), "Cluster", "acme", id).requestJson).cluster!;
  assert.equal(cluster.spec?.provider.case, "aws");
  assert.equal(cluster.spec?.provider.value?.irsa, false);
});
test("creation rejects wrong kind, tenant, IDs, unknown fields and schema violations", () => {
  const reject = (value: object, pattern: RegExp) => assert.throws(() => parseManifest(stringify(value), "CloudAccount", "acme", id), pattern);
  reject({ ...account, kind: "Cluster" }, /Expected kind/);
  reject({ ...account, apiVersion: "v1" }, /apiVersion/);
  reject({ ...account, metadata: { name: "prod", tenant: "other" } }, /active tenant/);
  reject({ ...account, metadata: { name: "prod", id } }, /Omit metadata.id/);
  reject({ ...account, other: true }, /Unknown manifest/);
  reject({ ...account, spec: { aws: { accountId: "bad" } } }, /account_id|accountId|12/);
  reject({ ...account, spec: { aws: { accountId: "111122223333", typo: true } } }, /unknown/i);
  assert.throws(() => parseManifest(stringify(envelope("Cluster", { tier: "TIER_2" })), "Cluster", "acme", id), /inherited/i);
});
test("unsafe, ambiguous and oversized YAML fail before review", () => {
  for (const text of ["[]", "kind: CloudAccount\nkind: Cluster", "---\na: b\n---\nc: d", "a: !custom value", "a: &loop [*loop]", "__proto__: {}", "a: .inf", "a: " + "x".repeat(65536)]) {
    assert.throws(() => parseManifest(text, "CloudAccount", "acme", id));
  }
});

test("cluster YAML accepts generated NaaS booleans and preserves their reviewed value", () => {
  for (const naas of [false, true]) {
    const spec = { region: "us-west-1", network: { networkId: id }, controlPlane: { endpointAccess: { endpointPublicAccess: false, endpointPrivateAccess: true } }, aws: {}, naas };
    const review = parseManifest(stringify(envelope("Cluster", spec)), "Cluster", "acme", id);
    const request = fromJsonString(CreateClusterRequestSchema, review.requestJson);
    assert.equal(request.cluster?.spec?.naas, naas);
    assert.equal(request.requestId, id);
  }
  const template = manifestTemplate("Cluster", "acme").replace("REPLACE_WITH_NETWORK_UUID", id);
  assert.equal(fromJsonString(CreateClusterRequestSchema, parseManifest(template, "Cluster", "acme", id).requestJson).cluster?.spec?.naas ?? false, false);
});
test("a Cluster manifest cannot define node groups: the platform creates them", () => {
  const spec = { region: "us-west-1", network: { networkId: id }, controlPlane: { endpointAccess: { endpointPublicAccess: false, endpointPrivateAccess: true } } };
  for (const extra of [{ defaultNodePool: { instanceTypes: ["m6i.large"] } }, { nodeGroups: [{ name: "batch" }] }]) {
    assert.throws(() => parseManifest(stringify(envelope("Cluster", { ...spec, ...extra })), "Cluster", "acme", id), /defaultNodePool|nodeGroups/);
  }
});
test("omitted tenant uses active tenant and output status is ignored like CLI", () => {
  const value = { ...account, metadata: { name: "prod" }, status: { anything: "output only" } };
  const review = parseManifest(stringify(value), "CloudAccount", "acme", id);
  const request = fromJsonString(BootstrapTenantRequestSchema, review.requestJson);
  assert.equal(request.tenantId, "acme");
  assert.equal(request.account?.status, undefined);
});
test("review uses read-only tenant checks; apply uses the reviewed request unchanged", async () => {
  const calls: string[] = [];
  let saved: unknown;
  const api = {
    tenant: { getCloudAccount: async () => { calls.push("get"); return { account: { id, tenantId: "acme", provider: { case: "aws" } } }; } },
    network: { createNetwork: async (request: unknown) => { calls.push("create"); saved = request; return { network: { id, tenantId: "acme" } }; } },
  } as unknown as BrowserClients;
  const signal = new AbortController().signal;
  const review = await reviewManifest(api, stringify(network), "Network", "acme", id, signal);
  assert.deepEqual(calls, ["get"]);
  await applyManifest(api, review, signal);
  assert.deepEqual(saved, fromJsonString(CreateNetworkRequestSchema, review.requestJson));
  assert.deepEqual(calls, ["get", "create"]);
  const other = { tenant: { getCloudAccount: async () => ({ account: { id, tenantId: "other" } }) } } as unknown as BrowserClients;
  await assert.rejects(reviewManifest(other, stringify(network), "Network", "acme", id, signal), /does not belong/);
});

test("cluster review refuses cross-tenant and region-mismatched network references", async () => {
  const value = envelope("Cluster", { region: "us-west-1", network: { networkId: id }, controlPlane: { endpointAccess: { endpointPublicAccess: false, endpointPrivateAccess: true } }, aws: {} });
  for (const referenced of [{ id, tenantId: "other", spec: { region: "us-west-1" } }, { id, tenantId: "acme", spec: { region: "eu-west-1" } }]) {
    const api = { network: { getNetwork: async () => ({ network: referenced }) } } as unknown as BrowserClients;
    await assert.rejects(reviewManifest(api, stringify(value), "Cluster", "acme", id, new AbortController().signal), /tenant|region/i);
  }
});
