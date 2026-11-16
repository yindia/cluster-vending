"use client";

import { useCallback, useState } from "react";
import { VStack, HStack } from "@astryxdesign/core/Layout";
import { Grid } from "@astryxdesign/core/Grid";
import { Card } from "@astryxdesign/core/Card";
import { Heading, Text } from "@astryxdesign/core/Text";
import { ConsoleTable as Table } from "./console-table";
import { type TableColumn } from "@astryxdesign/core/Table";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Selector } from "@astryxdesign/core/Selector";
import { TextInput } from "@astryxdesign/core/TextInput";
import { usePlatform } from "@/components/platform-provider";
import { RefreshButton } from "./refresh-button";
import { useAsyncQuery } from "@/lib/api/use-query";
import { collectPages } from "@/lib/api/pagination";
import { CloudAccountState } from "@/gen/cloud/v1/tenant_service_pb";
import { NetworkState } from "@/gen/cloud/v1/network_pb";
import { ClusterState, ReleaseChannel } from "@/gen/cloud/v1/cluster_pb";

type Row = { id: string; name: string; kind: string; provider: string; state: string; href: string };
const columns: TableColumn<Row>[] = [
  { key: "name", header: "Resource", renderCell: (row) => <Button label={row.name} href={row.href} variant="ghost" /> },
  { key: "kind", header: "Kind", renderCell: (row) => <Text>{row.kind}</Text> },
  { key: "provider", header: "Provider", renderCell: (row) => <Text>{row.provider}</Text> },
  { key: "state", header: "Observed state" },
];

export function OverviewPage() {
  const { api, tenantId } = usePlatform();
  const [search, setSearch] = useState("");
  const [kindFilter, setKindFilter] = useState("all");
  const [stateFilter, setStateFilter] = useState("all");
  const [providerFilter, setProviderFilter] = useState("all");
  const load = useCallback(async (signal: AbortSignal) => {
    const [accounts, networks, clusters] = await Promise.all([
      api.tenant.listCloudAccounts({ tenantId }, { signal }).then((response) => response.accounts),
      collectPages(async (pageToken) => { const page = await api.network.listNetworks({ tenantId, pageToken, pageSize: 200 }, { signal }); return { items: page.networks, nextPageToken: page.nextPageToken }; }, signal),
      collectPages(async (pageToken) => { const page = await api.cluster.listClusters({ tenantId, pageToken, pageSize: 200 }, { signal }); return { items: page.clusters, nextPageToken: page.nextPageToken }; }, signal),
    ]);
    const accountProvider = new Map(accounts.map((account) => [account.id, account.provider.case === "aws" ? "AWS" : "Unknown"]));
    const networkProvider = new Map(networks.map((network) => [network.id, accountProvider.get(network.spec?.bootstrapId ?? "") ?? "Unknown"]));
    const rows: Row[] = [
      ...accounts.map((account) => ({ id: account.id, name: account.accountName || account.id, kind: "Account", provider: accountProvider.get(account.id) ?? "Unknown", state: CloudAccountState[account.status?.state ?? 0] ?? "Unknown", href: `/accounts/${encodeURIComponent(account.id)}` })),
      ...networks.map((network) => ({ id: network.id, name: network.displayName || network.name, kind: "Network", provider: networkProvider.get(network.id) ?? "Unknown", state: NetworkState[network.status?.state ?? 0] ?? "Unknown", href: `/networks/${encodeURIComponent(network.id)}` })),
      ...clusters.map((cluster) => ({ id: cluster.id, name: cluster.displayName || cluster.name, kind: "Cluster", provider: networkProvider.get(cluster.spec?.network?.networkId ?? "") ?? "Unknown", state: ClusterState[cluster.status?.state ?? 0] ?? "Unknown", href: `/clusters/${encodeURIComponent(cluster.id)}` })),
    ];
    const channels = [ReleaseChannel.ALPHA, ReleaseChannel.BETA, ReleaseChannel.STABLE].map((channel) => ({ channel: ReleaseChannel[channel], clusters: clusters.filter((cluster) => cluster.spec?.releaseChannel === channel).length }));
    return { accounts, networks, clusters, rows, channels };
  }, [api, tenantId]);
  const query = useAsyncQuery(load, { pollMs: 30_000 });
  const rows = query.data?.rows ?? [];
  const filterOptions = (key: "kind" | "state" | "provider") => [...new Set(rows.map((row) => row[key]))].sort((a, b) => a.localeCompare(b)).map((value) => ({ value, label: value }));
  const terms = search.trim().toLowerCase().split(/\s+/).filter(Boolean);
  const filtered = rows.filter((row) => {
    const searchable = `${row.name} ${row.id} ${row.kind} ${row.state} ${row.provider}`.toLowerCase();
    return terms.every((term) => searchable.includes(term)) &&
      (kindFilter === "all" || row.kind === kindFilter) &&
      (stateFilter === "all" || row.state === stateFilter) &&
      (providerFilter === "all" || row.provider === providerFilter);
  });
  function clearFilters() { setSearch(""); setKindFilter("all"); setStateFilter("all"); setProviderFilter("all"); }
  return <VStack gap={6}>
    <HStack hAlign="between" vAlign="center" gap={3} wrap="wrap"><Heading level={2}>Fleet health</Heading><RefreshButton label="Refresh overview" onClick={query.refresh} isLoading={query.refreshing} /></HStack>
    {query.error && <Banner status="error" role="alert" title="Could not refresh the fleet" description={query.error} />}
    {query.loading && <Text role="status">Loading your tenant’s resources…</Text>}
    {query.data && <>
      <Grid columns={{ minWidth: 200, repeat: "fit" }} gap={4}>
        {[["Cloud accounts", query.data.accounts.length], ["Networks", query.data.networks.length], ["Clusters", query.data.clusters.length], ["Active upgrades", query.data.clusters.filter((cluster) => !!cluster.status?.upgrade).length]].map(([name, count]) => <Card key={name}><VStack gap={2}><Text color="secondary">{name}</Text><Text size="2xl" weight="semibold" hasTabularNumbers>{count}</Text></VStack></Card>)}
      </Grid>
      <VStack gap={3}>
        <Heading level={2}>Resource inventory</Heading>
        <HStack gap={3} wrap="wrap" vAlign="end">
          <TextInput label="Search resources" type="search" placeholder="Name, platform ID, kind, status or provider" value={search} onChange={setSearch} autoComplete="off" />
          <Selector label="Resource kind" value={kindFilter} onChange={setKindFilter} options={[{ value: "all", label: "All kinds" }, ...filterOptions("kind")]} />
          <Selector label="Resource status" value={stateFilter} onChange={setStateFilter} options={[{ value: "all", label: "All statuses" }, ...filterOptions("state")]} />
          <Selector label="Provider" value={providerFilter} onChange={setProviderFilter} options={[{ value: "all", label: "All providers" }, ...filterOptions("provider")]} />
          {(search || kindFilter !== "all" || stateFilter !== "all" || providerFilter !== "all") && <Button label="Clear resource filters" onClick={clearFilters} />}
        </HStack>
        <Text role="status" color="secondary">{filtered.length} of {rows.length} resources</Text>
        {filtered.length ? <Table data={filtered} columns={columns} idKey={(row) => `${row.kind}:${row.id}`} aria-label="Tenant resource inventory" /> : <Text color="secondary">{rows.length ? "No resources match the current filters." : "No resources were reported for this tenant."}</Text>}
        <Text type="supporting">Live API inventory for tenant {tenantId}. State is the controller’s last observation.</Text>
      </VStack>
      <VStack gap={3}><Heading level={2}>Cluster release channels</Heading><Table data={query.data.channels} columns={[{ key: "channel", header: "Channel" }, { key: "clusters", header: "Clusters" }]} idKey="channel" aria-label="Cluster distribution by explicit release channel" /><Text type="supporting">Channels belong to clusters. Rollout tiers are inherited from their networks.</Text></VStack>
    </>}
  </VStack>;
}
