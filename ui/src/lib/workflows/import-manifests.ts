import { createValidator } from "@bufbuild/protovalidate";
import { fromJson, fromJsonString, toJsonString, type JsonObject } from "@bufbuild/protobuf";
import { stringify } from "yaml";
import { ImportNetworkRequestSchema } from "../../gen/cloud/v1/network_service_pb";
import { ImportClusterRequestSchema } from "../../gen/cloud/v1/cluster_service_pb";
import type { BrowserClients } from "../api/client";
import { supportsCapability } from "../providers/registry";
import { parseComponentValues } from "./components";
import type { ManifestReview } from "./manifests";

export type ImportManifestKind = "NetworkImport" | "ClusterImport";
const validator = createValidator();
function object(value: unknown, label: string): JsonObject {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error(`${label} must be an object.`);
  return value as JsonObject;
}
function keys(value: JsonObject, allowed: readonly string[], label: string) {
  const extra = Object.keys(value).find(key => !allowed.includes(key));
  if (extra) throw new Error(`Unknown ${label} field: ${extra}.`);
}

/** The CLI import envelope, parsed with the same bounded YAML reader as create. */
export function parseImportManifest(text: string, kind: ImportManifestKind, tenantId: string, requestId: string): ManifestReview {
  if (!tenantId) throw new Error("Select a tenant before importing a resource.");
  const root = object(parseComponentValues(text), "Manifest");
  keys(root, ["apiVersion", "kind", "metadata", "spec"], "manifest");
  if (root.apiVersion !== "cluster-vending.io/v1") throw new Error("Use apiVersion: cluster-vending.io/v1.");
  if (root.kind !== kind) throw new Error(`Expected kind: ${kind}. Import existing infrastructure with an import manifest.`);
  const metadata = object(root.metadata, "metadata");
  keys(metadata, ["name", "id", "tenant", "displayName", "labels"], "metadata");
  if (typeof metadata.name !== "string" || !metadata.name.trim()) throw new Error("metadata.name is required.");
  if (metadata.id !== undefined && metadata.id !== "") throw new Error("Omit metadata.id when importing a resource; the API assigns it.");
  if (metadata.tenant !== undefined && metadata.tenant !== tenantId) throw new Error(`metadata.tenant must match the active tenant (${tenantId}).`);
  if (metadata.displayName !== undefined && typeof metadata.displayName !== "string") throw new Error("metadata.displayName must be a string.");
  if (metadata.labels !== undefined && Object.keys(object(metadata.labels, "metadata.labels")).length) throw new Error("metadata.labels cannot be set on import. Update the resource after takeover.");
  const spec = object(root.spec, "spec");
  for (const field of ["tenantId", "tenant_id", "name", "displayName", "display_name", "requestId", "request_id"]) {
    if (Object.hasOwn(spec, field)) throw new Error(`Omit spec.${field}; identity comes from metadata and the reviewed request.`);
  }
  const input = { ...spec, name: metadata.name, displayName: metadata.displayName ?? "", tenantId, requestId };
  const request = kind === "NetworkImport" ? fromJson(ImportNetworkRequestSchema, input) : fromJson(ImportClusterRequestSchema, input);
  const checked = validator.validate(kind === "NetworkImport" ? ImportNetworkRequestSchema : ImportClusterRequestSchema, request);
  if (checked.kind !== "valid") throw new Error(checked.error.message);
  if (request.provider.case !== "aws") throw new Error("This import provider is not supported.");
  const summary = [{ label: "Tenant", value: tenantId }, { label: "Name", value: metadata.name }, { label: "Provider", value: request.provider.case }, { label: "Ownership", value: "Observe only until explicit takeover" }];
  let requestJson: string;
  if (kind === "NetworkImport") {
    const network = fromJson(ImportNetworkRequestSchema, input);
    summary.push({ label: "Account ID", value: network.bootstrapId }, { label: "Region", value: network.region }, { label: "VPC ID", value: network.provider.value?.vpcId ?? "" }, { label: "Subnet IDs", value: network.provider.value?.subnetIds.join(", ") ?? "" });
    requestJson = toJsonString(ImportNetworkRequestSchema, network);
  } else {
    const cluster = fromJson(ImportClusterRequestSchema, input);
    summary.push({ label: "Network ID", value: cluster.networkId }, { label: "Existing node group", value: cluster.provider.value?.defaultNodeGroupName ?? "" });
    requestJson = toJsonString(ImportClusterRequestSchema, cluster);
  }
  return Object.freeze({ kind, tenantId, name: metadata.name, requestJson, summary: Object.freeze(summary.map(item => Object.freeze(item))) });
}

export async function reviewImportManifest(api: BrowserClients, text: string, kind: ImportManifestKind, tenantId: string, requestId: string, signal: AbortSignal): Promise<ManifestReview> {
  signal.throwIfAborted();
  const review = parseImportManifest(text, kind, tenantId, requestId);
  let accountId: string;
  if (kind === "NetworkImport") accountId = fromJsonString(ImportNetworkRequestSchema, review.requestJson).bootstrapId;
  else {
    const request = fromJsonString(ImportClusterRequestSchema, review.requestJson);
    const { network } = await api.network.getNetwork({ tenantId, networkId: request.networkId }, { signal });
    signal.throwIfAborted();
    if (!network || network.id !== request.networkId || network.tenantId !== tenantId || !network.spec) throw new Error("The referenced network does not belong to this tenant or has no discovered configuration.");
    accountId = network.spec.bootstrapId;
  }
  const { account } = await api.tenant.getCloudAccount({ tenantId, id: accountId }, { signal });
  signal.throwIfAborted();
  if (!account || account.id !== accountId || account.tenantId !== tenantId) throw new Error("The referenced account does not belong to this tenant.");
  if (!supportsCapability(account.provider.case ?? "", kind === "NetworkImport" ? "network.import" : "cluster.import")) throw new Error("The referenced account provider does not support this import.");
  return review;
}

export async function applyImportManifest(api: BrowserClients, review: ManifestReview, signal: AbortSignal) {
  signal.throwIfAborted();
  let resource;
  if (review.kind === "NetworkImport") {
    const request = fromJsonString(ImportNetworkRequestSchema, review.requestJson);
    if (request.tenantId !== review.tenantId || request.name !== review.name) throw new Error("The reviewed import identity changed. Review again.");
    resource = (await api.network.importNetwork(request, { signal })).network;
  } else if (review.kind === "ClusterImport") {
    const request = fromJsonString(ImportClusterRequestSchema, review.requestJson);
    if (request.tenantId !== review.tenantId || request.name !== review.name) throw new Error("The reviewed import identity changed. Review again.");
    resource = (await api.cluster.importCluster(request, { signal })).cluster;
  } else throw new Error("Expected a reviewed import manifest.");
  signal.throwIfAborted();
  if (!resource?.id || resource.tenantId !== review.tenantId || resource.name !== review.name) throw new Error("The response did not identify the accepted import in this tenant. Refresh inventory before retrying.");
  return resource;
}

export function importManifestTemplate(kind: ImportManifestKind, tenantId: string) {
  const spec = kind === "NetworkImport" ? { bootstrapId: "REPLACE_WITH_ACCOUNT_UUID", region: "us-west-1", aws: { vpcId: "vpc-0123456789abcdef0", subnetIds: ["subnet-0123456789abcdef0", "subnet-0123456789abcdef1"] } }
    : { networkId: "REPLACE_WITH_NETWORK_UUID", aws: { defaultNodeGroupName: "existing-workers" } };
  return stringify({ apiVersion: "cluster-vending.io/v1", kind, metadata: { name: kind === "ClusterImport" ? "existing-eks" : "existing-network", tenant: tenantId }, spec });
}
