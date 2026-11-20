"use client";

import { FilterRow } from "./filter-row";
import { StackItem } from "@astryxdesign/core/Stack";

import { RefreshButton } from "./refresh-button";

import { useCallback, useState } from "react";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { Link } from "@astryxdesign/core/Link";
import { Pagination } from "@astryxdesign/core/Pagination";
import { Selector } from "@astryxdesign/core/Selector";
import { proportional, type TableColumn } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { usePlatform } from "@/components/platform-provider";
import { ClusterState, ClusterTier, ReleaseChannel, UpgradeMode } from "@/gen/cloud/v1/cluster_pb";
import { Tier } from "@/gen/cloud/v1/tier_pb";
import { useAsyncQuery } from "@/lib/api/use-query";
import { loadChannelSubscriptions, type ChannelSubscriber } from "@/lib/workflows/channel-subscriptions";
import { ConsoleTable } from "./console-table";
import { CopyButton, CopyValue } from "./copy-value";
import { StatusValue } from "./detail-layout";

const PAGE_SIZE = 25;
type TierDisplay = { key: string; label: string };
type SubscriberRow = { id: string; subscriber: ChannelSubscriber; tier: TierDisplay };

function label(value: string | undefined, fallback = "Unknown") {
  return value ? value.toLowerCase().replaceAll("_", " ").replace(/^./, (letter) => letter.toUpperCase()) : fallback;
}

function subscriberTier({ network, cluster }: ChannelSubscriber): TierDisplay {
  if (network) {
    switch (network.spec?.tier ?? Tier.TIER_UNSPECIFIED) {
      case Tier.TIER_UNSPECIFIED:
      case Tier.TIER_0: return { key: "0", label: "Tier 0" };
      case Tier.TIER_1: return { key: "1", label: "Tier 1" };
      case Tier.TIER_2: return { key: "2", label: "Tier 2" };
      default: return { key: "unknown", label: "Unknown" };
    }
  }
  switch (cluster.status?.tier) {
    case ClusterTier.CLUSTER_TIER_0: return { key: "0", label: "Tier 0" };
    case ClusterTier.CLUSTER_TIER_1: return { key: "1", label: "Tier 1" };
    case ClusterTier.CLUSTER_TIER_2: return { key: "2", label: "Tier 2" };
    default: return { key: "unknown", label: "Not reported" };
  }
}

function upgradeMode(mode: UpgradeMode | undefined) {
  return mode === undefined || mode === UpgradeMode.UNSPECIFIED || mode === UpgradeMode.MANUAL ? "Manual" : mode === UpgradeMode.AUTOMATIC ? "Automatic" : "Unknown";
}

/** The selected tenant and channel form the entire read-only subscription scope. */
export function ChannelClusters({ channel }: { channel: ReleaseChannel }) {
  const { scopeKey } = usePlatform();
  return <ChannelSubscriberList key={`${scopeKey}:${channel}`} channel={channel} />;
}

function ChannelSubscriberList({ channel }: { channel: ReleaseChannel }) {
  const { api, tenantId } = usePlatform();
  const [search, setSearch] = useState("");
  const [stateFilter, setStateFilter] = useState("all");
  const [tierFilter, setTierFilter] = useState("all");
  const [modeFilter, setModeFilter] = useState("all");
  const [page, setPage] = useState(1);
  const load = useCallback(async (signal: AbortSignal) => {
    if (!tenantId) return undefined;
    return loadChannelSubscriptions(api, tenantId, channel, signal);
  }, [api, tenantId, channel]);
  const query = useAsyncQuery(load);
  const subscribers = query.data?.subscribers ?? [];
  const terms = search.trim().toLowerCase().split(/\s+/).filter(Boolean);
  const rows: SubscriberRow[] = subscribers.map((subscriber) => ({ id: subscriber.cluster.id, subscriber, tier: subscriberTier(subscriber) }));
  const filtered = rows.filter(({ subscriber: { cluster, network }, tier }) => {
    const searchable = [cluster.name, cluster.displayName, cluster.id, cluster.spec?.region,
      cluster.spec?.network?.networkId, network?.name, network?.displayName, network?.id, network?.spec?.region].filter(Boolean).join(" ").toLowerCase();
    return terms.every((term) => searchable.includes(term)) &&
      (stateFilter === "all" || stateFilter === String(cluster.status?.state ?? ClusterState.UNSPECIFIED)) &&
      (tierFilter === "all" || tierFilter === tier.key) &&
      (modeFilter === "all" || modeFilter === upgradeMode(cluster.spec?.upgradeMode));
  });
  const currentPage = Math.min(Math.max(1, page), Math.max(1, Math.ceil(filtered.length / PAGE_SIZE)));
  const visible = filtered.slice((currentPage - 1) * PAGE_SIZE, currentPage * PAGE_SIZE);
  const hasFilters = !!search || stateFilter !== "all" || tierFilter !== "all" || modeFilter !== "all";
  const channelName = label(ReleaseChannel[channel]);
  const columns: TableColumn<SubscriberRow>[] = [
    { key: "name", header: "Name", width: proportional(2, { minWidth: 80 }), renderCell: ({ subscriber: { cluster } }) => {
      const name = cluster.name || cluster.displayName || cluster.id;
      return <HStack gap={1} vAlign="center"><Link href={`/clusters/${encodeURIComponent(cluster.id)}`} maxLines={1}>{name}</Link><CopyButton value={name} label="Cluster name" /></HStack>;
    } },
    { key: "id", header: "Platform ID", width: proportional(2, { minWidth: 80 }), renderCell: ({ id }) => <CopyValue value={id} label="Cluster ID" /> },
    { key: "status", header: "Status", width: proportional(1, { minWidth: 80 }), renderCell: ({ subscriber: { cluster } }) => <StatusValue label={label(ClusterState[cluster.status?.state ?? ClusterState.UNSPECIFIED])} tone={cluster.status?.state === ClusterState.RUNNING ? "success" : cluster.status?.state === ClusterState.FAILED ? "error" : "neutral"} /> },
    { key: "kubernetes", header: "Kubernetes", width: proportional(1, { minWidth: 80 }), renderCell: ({ subscriber: { cluster } }) => <Text maxLines={1}>{cluster.status?.currentKubernetesVersion || "Not reported"}</Text> },
    { key: "network", header: "Network", width: proportional(2, { minWidth: 80 }), renderCell: ({ subscriber: { cluster, network } }) => {
      const id = cluster.spec?.network?.networkId;
      const name = network?.displayName || network?.name || id;
      return id && name ? <HStack gap={1} vAlign="center"><Link href={`/networks/${encodeURIComponent(id)}`} maxLines={1}>{name}</Link><CopyButton value={name} label={network ? "Network name" : "Network ID"} /></HStack> : <Text>Not reported</Text>;
    } },
    { key: "tier", header: "Tier", width: proportional(1, { minWidth: 80 }), renderCell: ({ tier }) => <Text maxLines={1}>{tier.label}</Text> },
    { key: "mode", header: "Upgrade mode", width: proportional(1, { minWidth: 80 }), renderCell: ({ subscriber: { cluster } }) => <Text maxLines={1}>{upgradeMode(cluster.spec?.upgradeMode)}</Text> },
    { key: "subscription", header: "Subscription", width: proportional(1, { minWidth: 80 }), renderCell: ({ subscriber }) => <Text maxLines={1}>{subscriber.defaulted ? "Stable default" : "Explicit"}</Text> },
  ];

  return <VStack as="section" aria-label="Channel cluster subscriptions" gap={4}>
    <HStack hAlign="end" vAlign="center" gap={4} wrap="wrap" width="100%">
      <StackItem size="fill"><VStack gap={1}><Heading level={2}>Subscribed clusters</Heading><Text color="secondary">{channelName} channel · Active tenant: {tenantId || "not selected"}. This list covers this tenant only.</Text><Text type="supporting" color="secondary">Explicit channel subscriptions include manual clusters; upgrade policy is separate. Automatic clusters with no channel selected follow Stable by default.</Text></VStack></StackItem>
      <RefreshButton label="Refresh subscribed clusters" isDisabled={!tenantId} isLoading={query.refreshing} onClick={query.refresh} />
    </HStack>
    {!tenantId && <Banner status="info" title="Select a tenant to view its channel subscriptions" />}
    {tenantId && query.loading && <Text role="status">Loading all subscribed clusters and network metadata…</Text>}
    {query.error && <Banner status="error" title="Channel subscriptions could not be loaded" description={query.error} />}
    {query.data?.networkError && <Banner status="warning" title="Network metadata unavailable" description={`${query.data.networkError} Cluster subscriptions are retained. Network UUIDs are shown where names are unavailable, and tiers fall back to the cluster’s observed status.`} />}
    <FilterRow label="Subscribed cluster filters">
      <TextInput label="Search subscribed clusters" type="search" placeholder="Cluster, network, ID or region" value={search} onChange={(value) => { setSearch(value); setPage(1); }} autoComplete="off" width="min(100%, 24rem)" />
      <Selector label="Subscription cluster status" value={stateFilter} onChange={(value) => { setStateFilter(value); setPage(1); }} width="min(100%, 12rem)" options={[{ value: "all", label: "All statuses" }, ...Object.values(ClusterState).filter((state) => typeof state === "number").map((state) => ({ value: String(state), label: label(ClusterState[state]) }))]} />
      <Selector label="Subscription rollout tier" value={tierFilter} onChange={(value) => { setTierFilter(value); setPage(1); }} width="min(100%, 12rem)" options={[{ value: "all", label: "All tiers" }, { value: "0", label: "Tier 0" }, { value: "1", label: "Tier 1" }, { value: "2", label: "Tier 2" }, { value: "unknown", label: "Unknown / not reported" }]} />
      <Selector label="Subscription upgrade mode" value={modeFilter} onChange={(value) => { setModeFilter(value); setPage(1); }} width="min(100%, 12rem)" options={[{ value: "all", label: "All upgrade modes" }, ...["Manual", "Automatic", "Unknown"].map((value) => ({ value, label: value }))]} />
      {hasFilters && <Button label="Clear subscription filters" variant="ghost" onClick={() => { setSearch(""); setStateFilter("all"); setTierFilter("all"); setModeFilter("all"); setPage(1); }} />}
    </FilterRow>
    {query.data && <Text role="status" color="secondary">{filtered.length} matching / {subscribers.length} subscribed clusters · {channelName} channel · Tenant {tenantId}{query.error ? " · Last successful load; refresh failed" : ""}</Text>}
    {query.data && (visible.length ? <ConsoleTable aria-label="Channel subscribed clusters" data={visible} columns={columns} idKey="id" rowIndexStart={(currentPage - 1) * PAGE_SIZE + 1} rowCount={filtered.length} /> : !query.error && <Text>{subscribers.length ? "No subscribed clusters match the current filters." : `No subscribed clusters were returned for tenant ${tenantId} on the ${channelName} channel.`}</Text>)}
    {filtered.length > PAGE_SIZE && <Pagination label="Subscribed cluster pagination" page={currentPage} onChange={setPage} totalItems={filtered.length} pageSize={PAGE_SIZE} variant="count" />}
  </VStack>;
}
