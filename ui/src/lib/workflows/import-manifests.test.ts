import assert from "node:assert/strict";
import test from "node:test";
import { create, fromJsonString } from "@bufbuild/protobuf";
import { stringify } from "yaml";
import { ImportNetworkRequestSchema } from "../../gen/cloud/v1/network_service_pb";
import { ImportClusterRequestSchema } from "../../gen/cloud/v1/cluster_service_pb";
import { NetworkSchema } from "../../gen/cloud/v1/network_pb";
import { CloudAccountSchema } from "../../gen/cloud/v1/tenant_service_pb";
import type { BrowserClients } from "../api/client";
import { parseImportManifest, reviewImportManifest, applyImportManifest } from "./import-manifests";
import { manifestTemplate, PLACEHOLDER_ID } from "./manifest-templates";

const accountId = "00000000-0000-4000-8000-000000000001";
const networkId = "00000000-0000-4000-8000-000000000002";
const requestId = "00000000-0000-4000-8000-000000000003";
const network = { apiVersion: "cluster-vending.io/v1", kind: "NetworkImport", metadata: { name: "existing-network", tenant: "acme" }, spec: { bootstrapId: accountId, region: "us-west-1", aws: { vpcId: "vpc-0123456789abcdef0", subnetIds: ["subnet-0123456789abcdef0", "subnet-0123456789abcdef1"] } } };
const cluster = { apiVersion: "cluster-vending.io/v1", kind: "ClusterImport", metadata: { name: "existing-eks", tenant: "acme" }, spec: { networkId, aws: { defaultNodeGroupName: "existing-workers" } } };

test("import manifests preserve CLI fields and the reviewed idempotency key", () => {
  const parsed = fromJsonString(ImportNetworkRequestSchema, parseImportManifest(stringify(network), "NetworkImport", "acme", requestId).requestJson);
  assert.equal(parsed.requestId, requestId); assert.equal(parsed.tenantId, "acme"); assert.equal(parsed.bootstrapId, accountId);
  assert.deepEqual(parsed.provider.value?.subnetIds, network.spec.aws.subnetIds);
  const parsedCluster = fromJsonString(ImportClusterRequestSchema, parseImportManifest(stringify(cluster), "ClusterImport", "acme", requestId).requestJson);
  assert.equal(parsedCluster.networkId, networkId); assert.equal(parsedCluster.provider.value?.defaultNodeGroupName, "existing-workers");
  for (const kind of ["NetworkImport", "ClusterImport"] as const) {
    const example = manifestTemplate(kind, "acme").replaceAll(PLACEHOLDER_ID, kind === "NetworkImport" ? accountId : networkId);
    assert.equal(parseImportManifest(example, kind, "acme", requestId).kind, kind);
  }
});

test("import YAML refuses cross-tenant identities, ownership fields and invalid cloud identifiers", () => {
  const reject = (input: object, pattern: RegExp) => assert.throws(() => parseImportManifest(stringify(input), "NetworkImport", "acme", requestId), pattern);
  reject({ ...network, kind: "Network" }, /Use kind: NetworkImport/);
  reject({ ...network, metadata: { ...network.metadata, tenant: "other" } }, /active tenant/);
  reject({ ...network, metadata: { ...network.metadata, id: networkId } }, /Omit metadata.id/);
  reject({ ...network, metadata: { ...network.metadata, labels: { team: "platform" } } }, /labels cannot/);
  reject({ ...network, spec: { ...network.spec, tenant_id: "other" } }, /Omit spec.tenant_id/);
  reject({ ...network, spec: { ...network.spec, requestId: "forged" } }, /Omit spec.requestId/);
  reject({ ...network, spec: { ...network.spec, aws: { ...network.spec.aws, vpcId: "invalid" } } }, /vpc|pattern/);
  reject({ ...network, spec: { ...network.spec, typo: true } }, /unknown/i);
  assert.throws(() => parseImportManifest("a: &loop [*loop]", "NetworkImport", "acme", requestId));
  assert.throws(() => parseImportManifest("a: " + "x".repeat(65536), "NetworkImport", "acme", requestId), /64 KiB/);
});

test("import review only reads owned references, and applying retries the exact reviewed request", async () => {
  const writes: unknown[] = [];
  const api = {
    tenant: { getCloudAccount: async () => ({ account: create(CloudAccountSchema, { id: accountId, tenantId: "acme", provider: { case: "aws", value: { accountId: "111122223333" } } }) }) },
    network: { getNetwork: async () => ({ network: create(NetworkSchema, { id: networkId, tenantId: "acme", spec: { bootstrapId: accountId, region: "us-west-1" } }) }) },
    cluster: { importCluster: async (request: unknown) => { writes.push(request); return { cluster: { id: requestId, tenantId: "acme", name: "existing-eks" } }; } },
  } as unknown as BrowserClients;
  const signal = new AbortController().signal;
  const review = await reviewImportManifest(api, stringify(cluster), "ClusterImport", "acme", requestId, signal);
  assert.deepEqual(writes, []);
  await applyImportManifest(api, review, signal); await applyImportManifest(api, review, signal);
  assert.deepEqual(writes[0], writes[1]);
  assert.equal((writes[0] as { requestId: string }).requestId, requestId);
  api.network.getNetwork = (async () => ({ network: create(NetworkSchema, { id: networkId, tenantId: "foreign" }) })) as unknown as typeof api.network.getNetwork;
  await assert.rejects(reviewImportManifest(api, stringify(cluster), "ClusterImport", "acme", requestId, signal), /does not belong/);
});
