import { fromJsonString, toJsonString } from "@bufbuild/protobuf";
import { ImportNetworkRequestSchema } from "../../gen/cloud/v1/network_service_pb";
import { ImportClusterRequestSchema } from "../../gen/cloud/v1/cluster_service_pb";
import type { BrowserClients } from "../api/client";
import { supportsCapability } from "../providers/registry";
import type { ManifestReview } from "./manifests";
import { reviewClusterPlacement } from "./cluster-placement";
import { decodeChecked, ManifestError, readManifest, refuseLabels, requestPath } from "./manifest-yaml";

export type ImportManifestKind = "NetworkImport" | "ClusterImport";

/** The CLI import envelope: metadata names the import, spec holds the request's other fields. */
export function parseImportManifest(text: string, kind: ImportManifestKind, tenantId: string, requestId: string): ManifestReview {
  const manifest = readManifest(text, kind, tenantId);
  const { metadata, spec, at } = manifest;
  if (metadata.id !== undefined && metadata.id !== "") throw new ManifestError("Omit metadata.id when importing a resource; the API assigns it.", at.lineOf(["metadata", "id"]));
  refuseLabels(manifest, "metadata.labels cannot be set on import. Update the resource after takeover.");
  for (const field of ["tenantId", "tenant_id", "name", "displayName", "display_name", "requestId", "request_id"]) {
    if (Object.hasOwn(spec, field)) throw new ManifestError(`Omit spec.${field}; identity comes from metadata and the reviewed request.`, at.lineOf(["spec", field]));
  }
  const name = metadata.name as string;
  const input = { ...spec, name, displayName: metadata.displayName ?? "", tenantId, requestId };
  const summary = [{ label: "Tenant", value: tenantId }, { label: "Name", value: name }];
  let requestJson: string;
  if (kind === "NetworkImport") {
    const request = decodeChecked(ImportNetworkRequestSchema, input, at, requestPath);
    if (request.provider.case !== "aws") throw new ManifestError("This import provider is not supported. Use spec.aws.", at.lineOf(["spec"]));
    summary.push({ label: "Provider", value: request.provider.case }, { label: "Ownership", value: "Observe only until explicit takeover" },
      { label: "Account ID", value: request.bootstrapId }, { label: "Region", value: request.region }, { label: "VPC ID", value: request.provider.value.vpcId }, { label: "Subnet IDs", value: request.provider.value.subnetIds.join(", ") });
    requestJson = toJsonString(ImportNetworkRequestSchema, request);
  } else {
    const request = decodeChecked(ImportClusterRequestSchema, input, at, requestPath);
    if (request.provider.case !== "aws") throw new ManifestError("This import provider is not supported. Use spec.aws.", at.lineOf(["spec"]));
    summary.push({ label: "Provider", value: request.provider.case }, { label: "Ownership", value: "Observe only until explicit takeover" },
      { label: "Network ID", value: request.networkId }, { label: "Existing node group", value: request.provider.value.defaultNodeGroupName });
    requestJson = toJsonString(ImportClusterRequestSchema, request);
  }
  return Object.freeze({ kind, tenantId, name, requestJson, summary: Object.freeze(summary.map(item => Object.freeze(item))), notes: Object.freeze([]) });
}

export async function reviewImportManifest(api: BrowserClients, text: string, kind: ImportManifestKind, tenantId: string, requestId: string, signal: AbortSignal): Promise<ManifestReview> {
  signal.throwIfAborted();
  const review = parseImportManifest(text, kind, tenantId, requestId);
  const { at } = readManifest(text, kind, tenantId);
  const pointed = (path: string[]) => (cause: unknown): never => {
    if (cause instanceof Error && cause.name === "AbortError") throw cause;
    throw new ManifestError(cause instanceof Error ? cause.message : String(cause), at.lineOf(path));
  };
  if (kind === "ClusterImport") {
    const request = fromJsonString(ImportClusterRequestSchema, review.requestJson);
    return reviewClusterPlacement(api, review, request.networkId, signal).catch(pointed(["spec", "networkId"]));
  }
  const accountId = fromJsonString(ImportNetworkRequestSchema, review.requestJson).bootstrapId;
  const { account } = await api.tenant.getCloudAccount({ tenantId, id: accountId }, { signal }).catch(pointed(["spec", "bootstrapId"]));
  signal.throwIfAborted();
  if (!account || account.id !== accountId || account.tenantId !== tenantId) throw new ManifestError("The referenced account does not belong to this tenant.", at.lineOf(["spec", "bootstrapId"]));
  if (!supportsCapability(account.provider.case ?? "", "network.import")) throw new ManifestError("The referenced account provider does not support this import.", at.lineOf(["spec", "bootstrapId"]));
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
