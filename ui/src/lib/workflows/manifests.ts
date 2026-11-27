import { create, fromJsonString, toJsonString, type JsonObject } from "@bufbuild/protobuf";
import { CloudAccountSchema, BootstrapTenantRequestSchema, type CloudAccount } from "../../gen/cloud/v1/tenant_service_pb";
import { NetworkSchema, type Network } from "../../gen/cloud/v1/network_pb";
import { ClusterSchema, ReleaseChannel, type Cluster } from "../../gen/cloud/v1/cluster_pb";
import { Tier } from "../../gen/cloud/v1/tier_pb";
import { CreateNetworkRequestSchema } from "../../gen/cloud/v1/network_service_pb";
import { CreateClusterRequestSchema } from "../../gen/cloud/v1/cluster_service_pb";
import type { BrowserClients } from "../api/client";
import { supportsCapability } from "../providers/registry";
import { reviewClusterPlacement } from "./cluster-placement";
import { administratorsNote, checkExtendedSupport, refuseClusterTier } from "./cluster-manifest";
import { parseImportManifest, reviewImportManifest, applyImportManifest } from "./import-manifests";
import { checkedRequest, decodeChecked, ManifestError, readManifest, refuseLabels, type Manifest, type ManifestKind } from "./manifest-yaml";

export type { ManifestKind } from "./manifest-yaml";
export { manifestTemplate } from "./manifest-templates";

export interface ReviewItem { label: string; value: string }
/** A reviewed create or import: the exact request JSON it submits, and what review found. */
export type ManifestReview = Readonly<{
  kind: ManifestKind; tenantId: string; name: string; requestJson: string;
  summary: readonly ReviewItem[];
  /** Warnings that do not block the request, such as a cluster without administrators. */
  notes: readonly string[];
}>;

export const displayEnum = (values: Record<number, string>, value: number) => (values[value] ?? `Unknown (${value})`).toLowerCase().replaceAll("_", " ").replace(/^./, letter => letter.toUpperCase());

export function freezeReview(review: Omit<ManifestReview, "summary" | "notes"> & { summary: ReviewItem[]; notes?: string[] }): ManifestReview {
  return Object.freeze({ ...review, summary: Object.freeze(review.summary.map(item => Object.freeze(item))), notes: Object.freeze([...(review.notes ?? [])]) });
}

/** The decoded resource body of a Network or Cluster manifest (protojson names). */
export function resourceJson({ metadata, spec }: Manifest, id?: string): JsonObject {
  return {
    ...(id ? { id } : {}), name: metadata.name ?? "", spec,
    ...(metadata.displayName !== undefined ? { displayName: metadata.displayName } : {}),
    ...(metadata.labels !== undefined ? { labels: metadata.labels } : {}),
  };
}

/** The CloudAccount a manifest states: metadata.name is the account name. */
export function accountFromManifest(manifest: Manifest): CloudAccount {
  refuseLabels(manifest, "metadata.labels do not apply to a CloudAccount.");
  const account = decodeChecked(CloudAccountSchema, { ...manifest.spec, accountName: manifest.metadata.name ?? "" }, manifest.at, ([head, ...rest]) => head === "accountName" ? ["metadata", "name"] : ["spec", head, ...rest]);
  if (account.id || account.tenantId || account.status || account.createTime || account.updateTime) throw new ManifestError("CloudAccount spec must omit output-only IDs, tenant, status and timestamps.");
  if (!supportsCapability(account.provider.case ?? "", "account.onboard")) throw new ManifestError("This account provider does not support onboarding.", manifest.at.lineOf(["spec"]));
  return account;
}

/** Same manifest envelope and strict generated spec codecs as the CLI. No writes. */
export function parseManifest(text: string, kind: ManifestKind, tenantId: string, requestId: string): ManifestReview {
  if (kind === "NetworkImport" || kind === "ClusterImport") return parseImportManifest(text, kind, tenantId, requestId);
  const manifest = readManifest(text, kind, tenantId);
  const { metadata, at } = manifest;
  if (metadata.id !== undefined && metadata.id !== "") throw new ManifestError("Omit metadata.id when creating a resource. To change an existing one, use Edit on its detail page.", at.lineOf(["metadata", "id"]));
  const name = metadata.name as string;
  const summary: ReviewItem[] = [{ label: "Tenant", value: tenantId }, { label: "Name", value: name }];
  const notes: string[] = [];
  let requestJson: string;
  if (kind === "CloudAccount") {
    const account = accountFromManifest(manifest);
    requestJson = toJsonString(BootstrapTenantRequestSchema, checkedRequest(BootstrapTenantRequestSchema, create(BootstrapTenantRequestSchema, { tenantId, account })));
    summary.push({ label: "Provider", value: account.provider.case ?? "Unknown" });
    if (account.provider.case === "aws") summary.push({ label: "Cloud account ID", value: account.provider.value.accountId }, { label: "IAM role", value: account.provider.value.role || "Platform default" });
  } else if (kind === "Network") {
    const network = decodeChecked(NetworkSchema, resourceJson(manifest), at);
    requestJson = toJsonString(CreateNetworkRequestSchema, checkedRequest(CreateNetworkRequestSchema, create(CreateNetworkRequestSchema, { tenantId, network, requestId })));
    summary.push({ label: "Account ID", value: network.spec?.bootstrapId ?? "" }, { label: "Region", value: network.spec?.region ?? "" }, { label: "Tier", value: network.spec?.tier ? displayEnum(Tier, network.spec.tier) : "Tier 0 (default)" });
  } else {
    refuseClusterTier(manifest);
    const cluster = decodeChecked(ClusterSchema, resourceJson(manifest), at);
    requestJson = toJsonString(CreateClusterRequestSchema, checkedRequest(CreateClusterRequestSchema, create(CreateClusterRequestSchema, { tenantId, cluster, requestId })));
    summary.push({ label: "Network ID", value: cluster.spec?.network?.networkId ?? "" }, { label: "Region", value: cluster.spec?.region ?? "" }, { label: "Release channel", value: cluster.spec?.releaseChannel ? displayEnum(ReleaseChannel, cluster.spec.releaseChannel) : "Server default" }, { label: "Kubernetes", value: cluster.spec?.controlPlane?.kubernetesVersion || "Release channel default" });
    const admins = administratorsNote(cluster.spec);
    if (admins) notes.push(admins);
  }
  return freezeReview({ kind, tenantId, name, requestJson, summary, notes });
}

/** An error from a reference check, pointed at the manifest line that names the reference. */
async function at<T>(manifest: Manifest, path: (string | number)[], check: () => Promise<T>): Promise<T> {
  try { return await check(); }
  catch (cause) {
    if (cause instanceof ManifestError || (cause instanceof Error && cause.name === "AbortError")) throw cause;
    const message = cause instanceof Error ? cause.message : String(cause);
    throw new ManifestError(message, /region/i.test(message) ? manifest.at.lineOf(["spec", "region"]) : manifest.at.lineOf(path));
  }
}

/** Parses, then checks the references in the active tenant (read only) and the server's version rules. */
export async function reviewManifest(api: BrowserClients, text: string, kind: ManifestKind, tenantId: string, requestId: string, signal: AbortSignal): Promise<ManifestReview> {
  signal.throwIfAborted();
  if (kind === "NetworkImport" || kind === "ClusterImport") return reviewImportManifest(api, text, kind, tenantId, requestId, signal);
  const review = parseManifest(text, kind, tenantId, requestId);
  const manifest = readManifest(text, kind, tenantId);
  if (kind === "Cluster") {
    const spec = fromJsonString(CreateClusterRequestSchema, review.requestJson).cluster!.spec!;
    const placed = await at(manifest, ["spec", "network", "networkId"], () => reviewClusterPlacement(api, review, spec.network?.networkId ?? "", signal, spec.region));
    const note = await checkExtendedSupport(api, tenantId, spec, manifest, signal);
    return note ? freezeReview({ ...placed, summary: [...placed.summary], notes: [...placed.notes, note] }) : placed;
  }
  if (kind === "Network") {
    const accountId = fromJsonString(CreateNetworkRequestSchema, review.requestJson).network!.spec!.bootstrapId;
    await at(manifest, ["spec", "bootstrapId"], async () => {
      const { account } = await api.tenant.getCloudAccount({ tenantId, id: accountId }, { signal });
      signal.throwIfAborted();
      if (!account || account.id !== accountId || account.tenantId !== tenantId) throw new Error("The referenced account does not belong to this tenant.");
      if (!supportsCapability(account.provider.case ?? "", "network.create")) throw new Error("The referenced account has no supported provider for this resource.");
    });
  }
  return review;
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
