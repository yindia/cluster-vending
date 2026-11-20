import type { CloudAccount } from "../../gen/cloud/v1/tenant_service_pb";
import type { BrowserClients } from "../api/client";
import { supportsCapability } from "../providers/registry";
import type { ManifestReview } from "./manifests";

export function accountDescription(account?: CloudAccount) {
  if (!account) return "Account unavailable";
  const id = account.provider.case === "aws" ? account.provider.value.accountId : account.id;
  return [account.accountName, id].filter(Boolean).join(" · ");
}

/** Enrich the reviewed request with placement verified in the active tenant. */
export async function reviewClusterPlacement(api: BrowserClients, review: ManifestReview, networkId: string, signal: AbortSignal, requestedRegion?: string): Promise<ManifestReview> {
  const { tenantId } = review;
  const { network } = await api.network.getNetwork({ tenantId, networkId }, { signal });
  signal.throwIfAborted();
  if (!network || network.id !== networkId || network.tenantId !== tenantId) throw new Error("The referenced network does not belong to this tenant.");
  if (!network.spec?.region) throw new Error("The referenced network has no configured region.");
  if (requestedRegion !== undefined && requestedRegion !== network.spec.region) throw new Error("Cluster region must match the referenced network.");
  const id = network.spec.bootstrapId;
  const { account } = await api.tenant.getCloudAccount({ tenantId, id }, { signal });
  signal.throwIfAborted();
  if (!account || account.id !== id || account.tenantId !== tenantId) throw new Error("The referenced account does not belong to this tenant.");
  if (!supportsCapability(account.provider.case ?? "", review.kind === "ClusterImport" ? "cluster.import" : "cluster.create")) throw new Error("The referenced account has no supported provider for this operation.");

  const placement = [
    { label: "Cloud account", value: accountDescription(account) },
    { label: "Network", value: network.displayName || network.name || networkId },
    ...(review.kind === "ClusterImport" ? [{ label: "Region", value: network.spec.region }] : []),
    { label: "Inherited tier", value: network.spec.tier ? `Tier ${network.spec.tier - 1}` : "Network default" },
  ];
  return Object.freeze({ ...review, summary: Object.freeze([...review.summary, ...placement.map(item => Object.freeze(item))]) });
}
