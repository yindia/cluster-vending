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
import { PLACEHOLDER_ID } from "./manifest-templates";
import { ManifestError } from "./manifest-yaml";

const id = "00000000-0000-4000-8000-000000000001";
const envelope = (kind: ManifestKind, spec: object, metadata = {}) => ({ apiVersion: "cluster-vending.io/v1", kind, metadata: { name: "production", tenant: "acme", ...metadata }, spec });
const account = envelope("CloudAccount", { aws: { accountId: "111122223333", role: "cluster-vending-provisioner" } });
const network = envelope("Network", { bootstrapId: id, region: "us-west-1", tier: "TIER_2", flowLogs: { enabled: true, retentionDays: 90 }, layout: { cidrBlock: "10.2.0.0/16", availabilityZoneCount: 2 } }, { labels: { team: "platform" } });

test("starter examples validate once their reference placeholders are replaced", () => {
  for (const kind of ["CloudAccount", "Network", "Cluster"] as const) {
    if (kind !== "CloudAccount") assert.throws(() => parseManifest(manifestTemplate(kind, "acme"), kind, "acme", id), /Line \d+: Replace the placeholder ID/, kind);
    const text = manifestTemplate(kind, "acme").replaceAll(PLACEHOLDER_ID, id);
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
  reject({ ...account, kind: "Cluster" }, /Use kind: CloudAccount/);
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

test("errors name the manifest line they point at", () => {
  const text = "apiVersion: cluster-vending.io/v1\nkind: Network\nmetadata:\n  name: prod\n  tenant: acme\nspec:\n  bootstrapId: " + id + "\n  region: us-west-1\n  layout: {cidrBlock: 10.0.0.0/16}\n  tier: TIER_9\n";
  const failure = (input: string, kind: ManifestKind = "Network") => { try { parseManifest(input, kind, "acme", id); } catch (cause) { return cause as ManifestError; } assert.fail("expected a refusal"); };
  assert.equal(failure(text).line, 10);
  assert.match(failure(text).message, /^Line 10: "TIER_9" is not a cloud.v1.Tier value/);
  assert.equal(failure(text.replace("tenant: acme", "tenant: other")).line, 5);
  assert.equal(failure(text.replace("  tier: TIER_9\n", "  typo: true\n")).line, 10);
  assert.equal(failure(text.replace("prod", "Bad_Name").replace("  tier: TIER_9\n", "")).line, 4);
  const cluster = "apiVersion: cluster-vending.io/v1\nkind: Cluster\nmetadata: {name: prod}\nspec:\n  region: us-west-1\n  naas: {}\n  aws: {}\n";
  assert.match(failure(cluster, "Cluster").message, /^Line 6: naas was removed with sub-tenants/);
  assert.equal(failure(cluster.replace("naas: {}", "tier: TIER_1"), "Cluster").line, 6);
});

test("a cluster without administrators of its own is created with a warning", () => {
  const value = envelope("Cluster", { region: "us-west-1", network: { networkId: id }, aws: {}, controlPlane: { endpointAccess: { endpointPublicAccess: false, endpointPrivateAccess: true } } });
  assert.match(parseManifest(stringify(value), "Cluster", "acme", id).notes[0], /spec.access.adminPrincipals/);
  const admins = envelope("Cluster", { ...value.spec, access: { adminPrincipals: ["arn:aws:iam::111122223333:role/admin"] } });
  assert.deepEqual(parseManifest(stringify(admins), "Cluster", "acme", id).notes, []);
});

test("cluster review refuses an automatic cluster on an extended-support minor, on the mode's line", async () => {
  const spec = { region: "us-west-1", network: { networkId: id }, controlPlane: { kubernetesVersion: "1.31", endpointAccess: { endpointPublicAccess: false, endpointPrivateAccess: true } }, upgradeMode: "UPGRADE_MODE_AUTOMATIC", releaseChannel: "RELEASE_CHANNEL_STABLE", aws: {} };
  const api = {
    network: { getNetwork: async () => ({ network: { id, tenantId: "acme", spec: { region: "us-west-1", bootstrapId: id } } }) },
    tenant: {
      getCloudAccount: async () => ({ account: { id, tenantId: "acme", provider: { case: "aws", value: { accountId: "111122223333" } } } }),
      listReleaseChannels: async () => ({ channels: [{ channel: 3, release: "stable" }] }),
      getRelease: async () => ({ release: { kubernetes: [{ minor: "1.33", channel: 3 }, { minor: "1.31", channel: 0 }] } }),
    },
  } as unknown as BrowserClients;
  const text = stringify(envelope("Cluster", spec));
  await assert.rejects(reviewManifest(api, text, "Cluster", "acme", id, new AbortController().signal), (cause: ManifestError) => {
    assert.match(cause.message, /extended support: it runs with UPGRADE_MODE_MANUAL only/);
    assert.equal(cause.line, text.split("\n").findIndex((line) => line.includes("upgradeMode")) + 1);
    return true;
  });
  const manual = await reviewManifest(api, stringify(envelope("Cluster", { ...spec, upgradeMode: "UPGRADE_MODE_MANUAL" })), "Cluster", "acme", id, new AbortController().signal);
  assert.equal(manual.kind, "Cluster");
});
