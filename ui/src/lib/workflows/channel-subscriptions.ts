import { ReleaseChannel, UpgradeMode, type Cluster } from "../../gen/cloud/v1/cluster_pb";
import type { Network } from "../../gen/cloud/v1/network_pb";
import type { BrowserClients } from "../api/client";
import { collectPages } from "../api/pagination";

export type ChannelSubscriber = {
  cluster: Cluster;
  network?: Network;
  channel: ReleaseChannel;
  defaulted: boolean;
};

/** Resolve subscription intent, independently of any current rollout's members. */
export function clusterSubscription(cluster: Cluster): { channel: ReleaseChannel; defaulted: boolean } | undefined {
  const configured = cluster.spec?.releaseChannel ?? ReleaseChannel.UNSPECIFIED;
  if ([ReleaseChannel.ALPHA, ReleaseChannel.BETA, ReleaseChannel.STABLE].includes(configured)) return { channel: configured, defaulted: false };
  // ResolveReleaseFields preserves an unset channel. It means Stable only for
  // automatic upgrades; an unspecified/manual mode never opts into a channel.
  if (configured === ReleaseChannel.UNSPECIFIED && cluster.spec?.upgradeMode === UpgradeMode.AUTOMATIC) return { channel: ReleaseChannel.STABLE, defaulted: true };
  return undefined;
}

export async function loadChannelSubscriptions(api: BrowserClients, tenantId: string, channel: ReleaseChannel, signal: AbortSignal): Promise<{ subscribers: ChannelSubscriber[]; networkError: string }> {
  if (!tenantId) throw new Error("Select a tenant to see channel subscriptions.");
  if (![ReleaseChannel.ALPHA, ReleaseChannel.BETA, ReleaseChannel.STABLE].includes(channel)) throw new Error("Choose Alpha, Beta or Stable.");
  const [clusters, networks] = await Promise.allSettled([
    collectPages(async pageToken => {
      const page = await api.cluster.listClusters({ tenantId, pageSize: 200, pageToken }, { signal });
      return { items: page.clusters, nextPageToken: page.nextPageToken };
    }, signal),
    collectPages(async pageToken => {
      const page = await api.network.listNetworks({ tenantId, pageSize: 200, pageToken }, { signal });
      return { items: page.networks, nextPageToken: page.nextPageToken };
    }, signal),
  ]);
  signal.throwIfAborted();
  if (clusters.status === "rejected") throw clusters.reason;
  if (clusters.value.some(cluster => cluster.tenantId !== tenantId || !cluster.id)) throw new Error("The cluster inventory did not identify every cluster in the selected tenant. Refresh subscriptions.");
  let networkError = "";
  const byNetwork = new Map<string, Network>();
  if (networks.status === "rejected") networkError = networks.reason instanceof Error ? networks.reason.message : "Network information could not be loaded.";
  else if (networks.value.some(network => network.tenantId !== tenantId || !network.id)) networkError = "Network inventory does not match the selected tenant. Network names are unavailable.";
  else for (const network of networks.value) byNetwork.set(network.id, network);
  const subscribers: ChannelSubscriber[] = [];
  for (const cluster of new Map(clusters.value.map(cluster => [cluster.id, cluster])).values()) {
    const subscription = clusterSubscription(cluster);
    if (subscription?.channel !== channel) continue;
    subscribers.push({ cluster, network: byNetwork.get(cluster.spec?.network?.networkId ?? ""), ...subscription });
  }
  subscribers.sort((left, right) => (left.cluster.displayName || left.cluster.name).localeCompare(right.cluster.displayName || right.cluster.name) || left.cluster.id.localeCompare(right.cluster.id));
  return { subscribers, networkError };
}
