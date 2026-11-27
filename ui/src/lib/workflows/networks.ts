import { create, toJsonString } from "@bufbuild/protobuf";
import { NetworkSchema, NetworkState, type Network } from "../../gen/cloud/v1/network_pb";
import { TakeOverNetworkRequestSchema, type TakeOverNetworkRequest } from "../../gen/cloud/v1/network_service_pb";
import { Disposition, ImportPhase, ImportStatusSchema } from "../../gen/cloud/v1/import_pb";
import type { CloudAccount } from "../../gen/cloud/v1/tenant_service_pb";
import { supportsCapability } from "../providers/registry";
import type { Cluster } from "../../gen/cloud/v1/cluster_pb";
import type { ListClustersResponse } from "../../gen/cloud/v1/cluster_service_pb";

/** Never infer a provider from an account ID, region, or a missing oneof. */
export function networkProvider(account: CloudAccount | undefined): "aws" | undefined {
  const provider = account?.provider.case;
  return provider === "aws" && supportsCapability(provider, "network.create") ? provider : undefined;
}

export function enumLabel(values: Record<number, string>, value: number | undefined): string {
  if (value === undefined) return "Not reported";
  const name = values[value];
  if (!name) return `Unknown (${value})`;
  return name.toLowerCase().replaceAll("_", " ").replace(/^./, (letter) => letter.toUpperCase());
}

export function networkIsReady(network: Network): boolean {
  return network.status?.state === NetworkState.READY && network.status.observedGeneration >= network.generation;
}

export function networkJson(network: Network): string {
  return toJsonString(NetworkSchema, network, { prettySpaces: 2 });
}

/** Bind consent to the reviewed ownership plan, not changing observation messages. */
export function networkTakeoverKey(network: Network): string {
  return JSON.stringify([network.tenantId, network.id, network.name, network.etag, String(network.generation),
    network.spec?.bootstrapId, network.status?.state,
    network.status?.import ? toJsonString(ImportStatusSchema, network.status.import) : null]);
}

export function networkTakeoverBlockReason(tenantId: string, network: Network, account: CloudAccount | undefined): string {
  if (!tenantId || !network.id || !network.etag || (network.tenantId && network.tenantId !== tenantId)) return "Reload the network to obtain its current tenant, ID and etag.";
  const provider = networkProvider(account);
  if (!account || account.id !== network.spec?.bootstrapId || (account.tenantId && account.tenantId !== tenantId) || !provider || !supportsCapability(provider, "network.takeover")) return "The network needs an account with a supported takeover provider.";
  if (network.status?.state !== NetworkState.IMPORTED || network.status.import?.phase !== ImportPhase.PLANNED) return "Takeover requires an IMPORTED network with a PLANNED import. Wait for discovery or resolve the reported failures.";
  const dispositions = [Disposition.MATCHES, Disposition.WILL_UPDATE, Disposition.WILL_CREATE, Disposition.NOT_IMPORTED, Disposition.CARRIED_OVER];
  if (network.status.import.resources.some(({ disposition }) => disposition === Disposition.BLOCKING)) return "Blocking resources prevent takeover. Resolve the import plan before continuing.";
  if (network.status.import.resources.some(({ disposition }) => !dispositions.includes(disposition))) return "The import plan contains an unspecified or unsupported resource disposition; takeover is disabled.";
  return "";
}

export interface NetworkTakeoverConfirmation {
  reviewKey: string;
  ownershipConfirmation: string;
  acknowledgeNotImported: boolean;
}

export function buildTakeOverNetwork(tenantId: string, network: Network, account: CloudAccount | undefined, confirmation: NetworkTakeoverConfirmation): TakeOverNetworkRequest {
  const blocked = networkTakeoverBlockReason(tenantId, network, account);
  if (blocked) throw new Error(blocked);
  if (confirmation.reviewKey !== networkTakeoverKey(network)) throw new Error("The import plan changed. Review it and confirm ownership again.");
  if (confirmation.ownershipConfirmation !== `take over ${network.name}`) throw new Error(`Type take over ${network.name} to confirm the previous tool has stopped managing these resources.`);
  if (network.status!.import!.resources.some(({ disposition }) => disposition === Disposition.NOT_IMPORTED) && !confirmation.acknowledgeNotImported) throw new Error("Acknowledge that every NOT_IMPORTED resource has another owner or was deleted, and the previous tool no longer manages anything here.");
  return create(TakeOverNetworkRequestSchema, { tenantId, networkId: network.id, etag: network.etag, acknowledgeNotImported: confirmation.acknowledgeNotImported });
}

/** Follow all tenant pages; never present a partial scan as complete inventory. */
export async function collectNetworkClusters(networkId: string, loadPage: (pageToken: string) => Promise<ListClustersResponse>, signal: AbortSignal): Promise<Cluster[]> {
  const result = new Map<string, Cluster>();
  const seen = new Set<string>();
  let token = "";
  for (let page = 0; page < 100; page++) {
    signal.throwIfAborted();
    if (seen.has(token)) throw new Error("Cluster listing repeated a page token. Refresh related clusters to retry.");
    seen.add(token);
    const response = await loadPage(token);
    signal.throwIfAborted();
    for (const cluster of response.clusters) if (cluster.spec?.network?.networkId === networkId) result.set(cluster.id, cluster);
    token = response.nextPageToken;
    if (!token) return [...result.values()];
  }
  throw new Error("Cluster listing exceeded 100 pages. Related cluster inventory could not be fully loaded.");
}

/** Shared search for managed, imported and carried-over resource inventories. */
export function filterNetworkInventory<T extends { name: string; kind: string; externalName: string }>(resources: readonly T[], search: string): T[] {
  const terms = search.trim().toLowerCase().split(/\s+/).filter(Boolean);
  return resources.filter((resource) => {
    const text = `${resource.name} ${resource.kind} ${resource.externalName}`.toLowerCase();
    return terms.every((term) => text.includes(term));
  });
}

/** UI rows only: status references may be names despite the cluster_ids field name. */
export type NetworkClusterRow = {
  key: string;
  reference: string;
  cluster?: Cluster;
};

/** A completed list is authoritative; never union legacy status names into UUID rows. */
export function networkClusterRows(
  networkId: string,
  statusReferences: readonly string[],
  clusters: readonly Cluster[] | undefined,
  authoritative = clusters !== undefined,
): NetworkClusterRow[] {
  const matching = new Map<string, Cluster>();
  for (const cluster of clusters ?? []) {
    if (cluster.id && cluster.spec?.network?.networkId === networkId) matching.set(cluster.id, cluster);
  }
  const rows: NetworkClusterRow[] = [...matching.values()].map((cluster) => ({
    key: `cluster:${cluster.id}`, reference: cluster.name || cluster.id, cluster,
  }));
  if (authoritative) return rows;
  // Resolve by ID or resource name only within this network. Distinct UUIDs stay distinct.
  const resolved = new Set([...matching.values()].flatMap((cluster) => [cluster.id, cluster.name]));
  for (const reference of new Set(statusReferences.map((value) => value.trim()).filter(Boolean))) {
    if (!resolved.has(reference)) rows.push({ key: `reference:${reference}`, reference });
  }
  return rows;
}
