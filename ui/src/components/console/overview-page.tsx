"use client";

import { FilterRow } from "./filter-row";
import { useCallback, useState } from "react";
import { VStack, HStack } from "@astryxdesign/core/Layout";
import { Grid } from "@astryxdesign/core/Grid";
import { Card } from "@astryxdesign/core/Card";
import { Heading, Text } from "@astryxdesign/core/Text";
import { ConsoleTable as Table } from "./console-table";
import { proportional, type TableColumn } from "@astryxdesign/core/Table";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Icon } from "@astryxdesign/core/Icon";
import { Link } from "@astryxdesign/core/Link";
import { StackItem } from "@astryxdesign/core/Stack";
import { Selector } from "@astryxdesign/core/Selector";
import { TextInput } from "@astryxdesign/core/TextInput";
import { usePlatform } from "@/components/platform-provider";
import { RefreshButton } from "./refresh-button";
import { useAsyncQuery } from "@/lib/api/use-query";
import { collectPages } from "@/lib/api/pagination";
import { ReleaseChannel } from "@/gen/cloud/v1/cluster_pb";
import { buildInventoryTree, filterInventoryTree, type InventoryNode, type InventoryRow } from "@/lib/workflows/inventory-tree";
import { CopyValue } from "./copy-value";
import { StatusValue } from "./detail-layout";

function countLabel(count: number, kind: string) { return `${count} ${kind}${count === 1 ? "" : "s"}`; }

export function OverviewPage() {
  const { api, tenantId } = usePlatform();
  const [search, setSearch] = useState("");
  const [kindFilter, setKindFilter] = useState("all");
  const [stateFilter, setStateFilter] = useState("all");
  const [providerFilter, setProviderFilter] = useState("all");
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set());
  const load = useCallback(async (signal: AbortSignal) => {
    const [accounts, networks, clusters] = await Promise.all([
      api.tenant.listCloudAccounts({ tenantId }, { signal }).then((response) => response.accounts),
      collectPages(async (pageToken) => { const page = await api.network.listNetworks({ tenantId, pageToken, pageSize: 200 }, { signal }); return { items: page.networks, nextPageToken: page.nextPageToken }; }, signal),
      collectPages(async (pageToken) => { const page = await api.cluster.listClusters({ tenantId, pageToken, pageSize: 200 }, { signal }); return { items: page.clusters, nextPageToken: page.nextPageToken }; }, signal),
    ]);
    const tree = buildInventoryTree(accounts, networks, clusters);
    const channels = [ReleaseChannel.ALPHA, ReleaseChannel.BETA, ReleaseChannel.STABLE].map((channel) => ({ channel: ReleaseChannel[channel], clusters: clusters.filter((cluster) => cluster.spec?.releaseChannel === channel).length }));
    return { accounts, networks, clusters, tree, channels };
  }, [api, tenantId]);
  const query = useAsyncQuery(load, { pollMs: 30_000 });
  const tree = query.data?.tree ?? [];
  const rows: InventoryNode[] = [];
  function flatten(nodes: readonly InventoryNode[]) { nodes.forEach(node => { if (node.kind !== "Group") rows.push(node); flatten(node.children); }); }
  flatten(tree);
  const filterOptions = (key: "kind" | "state" | "provider") => [...new Set(rows.map((row) => row[key]))].sort((a, b) => a.localeCompare(b)).map((value) => ({ value, label: value }));
  const filtered = filterInventoryTree(tree, { search, kind: kindFilter, state: stateFilter, provider: providerFilter }, collapsed);
  const toggle = (id: string) => setCollapsed(previous => { const next = new Set(previous); if (next.has(id)) next.delete(id); else next.add(id); return next; });
  const columns: TableColumn<InventoryRow>[] = [
    { key: "name", header: "Resource", width: proportional(2, { minWidth: 280 }), renderCell: row => <HStack gap={2} vAlign="center" style={{ paddingInlineStart: `calc(var(--spacing-4) * ${row.depth})` }}>
      <HStack width="2rem" style={{ flexShrink: 0 }} vAlign="center" hAlign="center">{row.children.length > 0 && <Button label={`${collapsed.has(row.id) && !filtered.filtering ? "Expand" : "Collapse"} ${row.name}`} variant="ghost" size="sm" isIconOnly icon={<Icon icon={collapsed.has(row.id) && !filtered.filtering ? "chevronRight" : "chevronDown"} />} aria-expanded={filtered.filtering || !collapsed.has(row.id)} isDisabled={filtered.filtering} onClick={() => toggle(row.id)} />}</HStack>
      {row.href ? <Link href={row.href} maxLines={1}>{row.name}</Link> : <Text weight="semibold">{row.name}</Text>}
      {row.context && row.kind !== "Group" && <Text type="supporting" color="secondary">Context</Text>}
    </HStack> },
    { key: "kind", header: "Kind", renderCell: row => <Text>{row.kind}</Text> },
    { key: "resourceId", header: "ID", renderCell: row => row.resourceId ? <CopyValue value={row.resourceId} label={`${row.kind} ID`} /> : <Text>—</Text> },
    { key: "children", header: "Resources", width: proportional(1, { minWidth: 180 }), renderCell: row => <Text maxLines={1} hasTruncateTooltip>{row.kind === "Account" || row.kind === "Group" ? `${countLabel(row.networks, "network")} · ${countLabel(row.clusters, "cluster")}` : row.kind === "Network" ? countLabel(row.clusters, "cluster") : "—"}</Text> },
    { key: "provider", header: "Provider", renderCell: row => <Text>{row.provider || "—"}</Text> },
    { key: "state", header: "Status", renderCell: row => <StatusValue label={row.state} tone={["Ready", "Running"].includes(row.state) ? "success" : row.state === "Failed" ? "error" : "neutral"} /> },
  ];
  function clearFilters() { setSearch(""); setKindFilter("all"); setStateFilter("all"); setProviderFilter("all"); }
  return <VStack gap={6}>
    <HStack hAlign="end" vAlign="center" gap={3} wrap="wrap" width="100%"><StackItem size="fill"><Heading level={2}>Fleet health</Heading></StackItem><RefreshButton label="Refresh overview" onClick={query.refresh} isLoading={query.refreshing} /></HStack>
    {query.error && <Banner status="error" role="alert" title="Could not refresh the fleet" description={query.error} />}
    {query.loading && <Text role="status">Loading your tenant’s resources…</Text>}
    {query.data && <>
      <Grid columns={{ minWidth: 200, repeat: "fit" }} gap={4}>
        {[["Cloud accounts", query.data.accounts.length], ["Networks", query.data.networks.length], ["Clusters", query.data.clusters.length], ["Active upgrades", query.data.clusters.filter((cluster) => !!cluster.status?.upgrade).length]].map(([name, count]) => <Card key={name}><VStack gap={2}><Text color="secondary">{name}</Text><Text size="2xl" weight="semibold" hasTabularNumbers>{count}</Text></VStack></Card>)}
      </Grid>
      <VStack gap={3}>
        <HStack gap={3} hAlign="end" vAlign="center" wrap="wrap" width="100%"><StackItem size="fill"><Heading level={2}>Resource inventory</Heading></StackItem><Button label="Expand all" variant="ghost" size="sm" isDisabled={filtered.filtering || collapsed.size === 0} onClick={() => setCollapsed(new Set())} /><Button label="Collapse all" variant="ghost" size="sm" isDisabled={filtered.filtering || !tree.length} onClick={() => setCollapsed(new Set(rows.filter(row => row.children.length).map(row => row.id).concat(tree.filter(row => row.kind === "Group").map(row => row.id))))} /></HStack>
        <FilterRow label="Resource filters">
          <TextInput label="Search resources" type="search" placeholder="Name, platform ID, kind, status or provider" value={search} onChange={setSearch} autoComplete="off" />
          <Selector label="Resource kind" value={kindFilter} onChange={setKindFilter} options={[{ value: "all", label: "All kinds" }, ...filterOptions("kind")]} />
          <Selector label="Resource status" value={stateFilter} onChange={setStateFilter} options={[{ value: "all", label: "All statuses" }, ...filterOptions("state")]} />
          <Selector label="Provider" value={providerFilter} onChange={setProviderFilter} options={[{ value: "all", label: "All providers" }, ...filterOptions("provider")]} />
          {(search || kindFilter !== "all" || stateFilter !== "all" || providerFilter !== "all") && <Button label="Clear resource filters" onClick={clearFilters} />}
        </FilterRow>
        <Text role="status" color="secondary">{filtered.matches} of {rows.length} resources{filtered.filtering ? " · Parent rows provide context" : " · Account → Network → Cluster"}</Text>
        {filtered.rows.length ? <Table data={filtered.rows} columns={columns} idKey="id" aria-label="Tenant resource inventory" /> : <Text color="secondary">{rows.length ? "No resources match the current filters." : "No resources were reported for this tenant."}</Text>}
        <Text type="supporting">Live API inventory for tenant {tenantId}. State is the controller’s last observation.</Text>
      </VStack>
      <VStack gap={3}><Heading level={2}>Cluster release channels</Heading><Table data={query.data.channels} columns={[{ key: "channel", header: "Channel" }, { key: "clusters", header: "Clusters" }]} idKey="channel" aria-label="Cluster distribution by explicit release channel" /><Text type="supporting">Channels belong to clusters. Rollout tiers are inherited from their networks.</Text></VStack>
    </>}
  </VStack>;
}
