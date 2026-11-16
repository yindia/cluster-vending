import { create, fromJson, fromJsonString, toJsonString, type JsonObject, type DescMessage, type MessageShape } from "@bufbuild/protobuf";
import { createValidator } from "@bufbuild/protovalidate";
import { stringify } from "yaml";
import { CloudAccountSchema, BootstrapTenantRequestSchema, type CloudAccount } from "../../gen/cloud/v1/tenant_service_pb";
import { NetworkSchema, type Network } from "../../gen/cloud/v1/network_pb";
import { ClusterSchema, ReleaseChannel, type Cluster } from "../../gen/cloud/v1/cluster_pb";
import { Tier } from "../../gen/cloud/v1/tier_pb";
import { CreateNetworkRequestSchema } from "../../gen/cloud/v1/network_service_pb";
import { CreateClusterRequestSchema } from "../../gen/cloud/v1/cluster_service_pb";
import type { BrowserClients } from "../api/client";
import { supportsCapability } from "../providers/registry";
import { parseComponentValues } from "./components";

import { parseImportManifest, reviewImportManifest, applyImportManifest, importManifestTemplate } from "./import-manifests";

export type ManifestKind = "CloudAccount" | "Network" | "Cluster" | "NetworkImport" | "ClusterImport";
export type ManifestReview = Readonly<{
  kind: ManifestKind; tenantId: string; name: string; requestJson: string;
  summary: readonly { label: string; value: string }[];
}>;
const validator = createValidator();
const displayEnum = (values: Record<number, string>, value: number) => (values[value] ?? `Unknown (${value})`).toLowerCase().replaceAll("_", " ").replace(/^./, letter => letter.toUpperCase());
function checked<S extends DescMessage>(schema: S, value: MessageShape<S>) {
  const result = validator.validate(schema, value);
  if (result.kind !== "valid") throw new Error(result.error.message);
  return value;
}
function object(value: unknown, label: string): JsonObject {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error(`${label} must be an object.`);
  return value as JsonObject;
}
function keys(value: JsonObject, allowed: readonly string[], label: string) {
  const extra = Object.keys(value).find(key => !allowed.includes(key));
  if (extra) throw new Error(`Unknown ${label} field: ${extra}.`);
}

/** Same manifest envelope and strict generated spec codecs as the CLI. No writes. */
export function parseManifest(text: string, kind: ManifestKind, tenantId: string, requestId: string): ManifestReview {
  if (kind === "NetworkImport" || kind === "ClusterImport") return parseImportManifest(text, kind, tenantId, requestId);
  if (!tenantId) throw new Error("Select a tenant before creating a resource.");
  // Reuse the bounded, safe YAML/JSON reader: 64 KiB, string keys, no custom
  // tags, duplicate keys, recursive aliases or unbounded expansion.
  let root: JsonObject;
  try { root = object(parseComponentValues(text), "Manifest"); }
  catch (error) { throw new Error(`Invalid manifest: ${error instanceof Error ? error.message : "YAML parsing failed."}`); }
  keys(root, ["apiVersion", "kind", "metadata", "spec", "status"], "manifest");
  if (root.apiVersion !== "cluster-vending.io/v1") throw new Error("Use apiVersion: cluster-vending.io/v1.");
  if (root.kind !== kind) throw new Error(`Expected kind: ${kind}.`);
  const meta = object(root.metadata, "metadata");
  keys(meta, ["name", "id", "tenant", "displayName", "labels"], "metadata");
  if (typeof meta.name !== "string" || !meta.name.trim()) throw new Error("metadata.name is required.");
  if (meta.id !== undefined && meta.id !== "") throw new Error("Omit metadata.id when creating a resource. Use the detail page to update it.");
  if (meta.tenant !== undefined && (typeof meta.tenant !== "string" || meta.tenant !== tenantId)) throw new Error(`metadata.tenant must match the active tenant (${tenantId}).`);
  if (meta.displayName !== undefined && typeof meta.displayName !== "string") throw new Error("metadata.displayName must be a string.");
  const spec = object(root.spec, "spec");
  const summary = [{ label: "Tenant", value: tenantId }, { label: "Name", value: meta.name }];
  let requestJson: string;
  if (kind === "CloudAccount") {
    if (meta.labels !== undefined && Object.keys(object(meta.labels, "metadata.labels")).length) throw new Error("CloudAccount does not support metadata.labels.");
    const account = fromJson(CloudAccountSchema, { ...spec, accountName: meta.name });
    if (account.id || account.tenantId || account.status || account.createTime || account.updateTime) throw new Error("CloudAccount spec must omit output-only IDs, tenant, status and timestamps.");
    if (!supportsCapability(account.provider.case ?? "", "account.onboard")) throw new Error("This account provider does not support onboarding.");
    const request = checked(BootstrapTenantRequestSchema, create(BootstrapTenantRequestSchema, { tenantId, account }));
    requestJson = toJsonString(BootstrapTenantRequestSchema, request);
    summary.push({ label: "Provider", value: account.provider.case ?? "Unknown" });
    if (account.provider.case === "aws") summary.push({ label: "Cloud account ID", value: account.provider.value.accountId }, { label: "IAM role", value: account.provider.value.role || "Platform default" });
  } else {
    const resource = { name: meta.name, ...(meta.displayName !== undefined ? { displayName: meta.displayName } : {}), ...(meta.labels !== undefined ? { labels: meta.labels } : {}), spec };
    if (kind === "Network") {
      const network = fromJson(NetworkSchema, resource);
      const request = checked(CreateNetworkRequestSchema, create(CreateNetworkRequestSchema, { tenantId, network, requestId }));
      requestJson = toJsonString(CreateNetworkRequestSchema, request);
      summary.push({ label: "Account ID", value: network.spec?.bootstrapId ?? "" }, { label: "Region", value: network.spec?.region ?? "" }, { label: "Tier", value: network.spec?.tier ? displayEnum(Tier, network.spec.tier) : "Tier 0 (default)" });
    } else {
      if (Object.hasOwn(spec, "tier")) throw new Error("Cluster tier is inherited from its network. Omit spec.tier.");
      const cluster = fromJson(ClusterSchema, resource);
      const request = checked(CreateClusterRequestSchema, create(CreateClusterRequestSchema, { tenantId, cluster, requestId }));
      requestJson = toJsonString(CreateClusterRequestSchema, request);
      summary.push({ label: "Network ID", value: cluster.spec?.network?.networkId ?? "" }, { label: "Region", value: cluster.spec?.region ?? "" }, { label: "Release channel", value: cluster.spec?.releaseChannel ? displayEnum(ReleaseChannel, cluster.spec.releaseChannel) : "Server default" }, { label: "Kubernetes", value: cluster.spec?.controlPlane?.kubernetesVersion || "Release channel default" });
    }
  }
  return Object.freeze({ kind, tenantId, name: meta.name, requestJson, summary: Object.freeze(summary.map(item => Object.freeze(item))) });
}

export async function reviewManifest(api: BrowserClients, text: string, kind: ManifestKind, tenantId: string, requestId: string, signal: AbortSignal): Promise<ManifestReview> {
  signal.throwIfAborted();
  if (kind === "NetworkImport" || kind === "ClusterImport") return reviewImportManifest(api, text, kind, tenantId, requestId, signal);
  const review = parseManifest(text, kind, tenantId, requestId);
  let tier: string | undefined;
  if (kind !== "CloudAccount") {
    let accountId: string;
    if (kind === "Network") {
      accountId = fromJsonString(CreateNetworkRequestSchema, review.requestJson).network!.spec!.bootstrapId;
    } else {
      const cluster = fromJsonString(CreateClusterRequestSchema, review.requestJson).cluster!;
      const networkId = cluster.spec!.network!.networkId;
      const { network } = await api.network.getNetwork({ tenantId, networkId }, { signal });
      signal.throwIfAborted();
      if (!network || network.id !== networkId || network.tenantId !== tenantId) throw new Error("The referenced network does not belong to this tenant.");
      if (network.spec?.region !== cluster.spec!.region) throw new Error("Cluster region must match the referenced network.");
      accountId = network.spec.bootstrapId;
      tier = network.spec.tier ? displayEnum(Tier, network.spec.tier) : "Tier 0 (default)";
    }
    const { account } = await api.tenant.getCloudAccount({ tenantId, id: accountId }, { signal });
    signal.throwIfAborted();
    if (!account || account.id !== accountId || account.tenantId !== tenantId) throw new Error("The referenced account does not belong to this tenant.");
    if (!supportsCapability(account.provider.case ?? "", kind === "Network" ? "network.create" : "cluster.create")) throw new Error("The referenced account has no supported provider for this resource.");
  }
  return tier ? Object.freeze({ ...review, summary: Object.freeze([...review.summary, Object.freeze({ label: "Inherited tier", value: tier })]) }) : review;
}

/** Submit the immutable reviewed protobuf snapshot, preserving optional fields. */
export async function applyManifest(api: BrowserClients, review: ManifestReview, signal: AbortSignal): Promise<CloudAccount | Network | Cluster> {
  signal.throwIfAborted();
  if (review.kind === "NetworkImport" || review.kind === "ClusterImport") return applyImportManifest(api, review, signal);
  let resource: CloudAccount | Network | Cluster | undefined;
  if (review.kind === "CloudAccount") {
    const request = fromJsonString(BootstrapTenantRequestSchema, review.requestJson);
    if (request.tenantId !== review.tenantId) throw new Error("The reviewed tenant changed. Review again.");
    resource = (await api.tenant.bootstrapTenant(request, { signal })).account;
  } else if (review.kind === "Network") {
    const request = fromJsonString(CreateNetworkRequestSchema, review.requestJson);
    if (request.tenantId !== review.tenantId) throw new Error("The reviewed tenant changed. Review again.");
    resource = (await api.network.createNetwork(request, { signal })).network;
  } else {
    const request = fromJsonString(CreateClusterRequestSchema, review.requestJson);
    if (request.tenantId !== review.tenantId) throw new Error("The reviewed tenant changed. Review again.");
    resource = (await api.cluster.createCluster(request, { signal })).cluster;
  }
  signal.throwIfAborted();
  if (!resource?.id || resource.tenantId !== review.tenantId) throw new Error("The response did not identify an accepted resource in this tenant. Refresh inventory before retrying.");
  return resource;
}

export function manifestTemplate(kind: ManifestKind, tenantId: string): string {
  if (kind === "NetworkImport" || kind === "ClusterImport") return importManifestTemplate(kind, tenantId);
  const spec = kind === "CloudAccount" ? { aws: { accountId: "111122223333", role: "cluster-vending-provisioner" } }
    : kind === "Network" ? { bootstrapId: "REPLACE_WITH_ACCOUNT_UUID", region: "us-west-1", tier: "TIER_2", layout: { cidrBlock: "10.0.0.0/16", availabilityZoneCount: 3, natGatewayMode: "NAT_GATEWAY_MODE_SINGLE" } }
    : { region: "us-west-1", network: { networkId: "REPLACE_WITH_NETWORK_UUID" }, aws: {}, defaultNodePool: { instanceTypes: ["m6i.large"], scalingConfig: { minSize: 1, maxSize: 3 } }, controlPlane: { endpointAccess: { endpointPublicAccess: false, endpointPrivateAccess: true } }, releaseChannel: "RELEASE_CHANNEL_STABLE", upgradeMode: "UPGRADE_MODE_MANUAL" };
  return stringify({ apiVersion: "cluster-vending.io/v1", kind, metadata: { name: kind === "CloudAccount" ? "production" : "prod", tenant: tenantId }, spec });
}
