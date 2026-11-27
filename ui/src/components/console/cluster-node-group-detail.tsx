"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Badge } from "@astryxdesign/core/Badge";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { Link } from "@astryxdesign/core/Link";
import { SegmentedControl, SegmentedControlItem } from "@astryxdesign/core/SegmentedControl";
import { InfoTip } from "@astryxdesign/core/InfoTip";
import { StackItem } from "@astryxdesign/core/Stack";
import { proportional, type TableColumn } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { usePlatform } from "@/components/platform-provider";
import { useAsyncQuery } from "@/lib/api/use-query";
import { PLATFORM_MANAGED_LABEL, hasManifestYaml } from "@/lib/workflows/node-groups";
import {
  NODE_PAGE_SIZE, collectionState, groupDetail, groupResources, isInfoNotice, isTokenMismatch, liveReadFailure, nodeCount, nodeFilterOptions, nodeRow, nodeStatusFilter, pageRange, pagerBack, pagerForward, pagerStart,
  type NodeRow, type NodeStatusChoice,
} from "@/lib/workflows/node-group-nodes";
import type { ClusterStatus } from "@/gen/cloud/v1/cluster_pb";
import { ConsoleTable } from "./console-table";
import { DetailHeader, StatusValue } from "./detail-layout";
import { NodeGroupResources, NodeTotalsNote, totalsResources } from "./node-group-resources";
import { nodeTotalsFailure, nodeTotalsOf, type NodeTotalsState } from "@/lib/workflows/node-resource-totals";
import { NodeGroupYamlButton } from "./node-group-yaml-button";
import { Heading } from "@astryxdesign/core/Heading";
import { RefreshButton } from "./refresh-button";

const SEARCH_DELAY_MS = 300;

/**
 * Optional columns follow the table's own width, so a narrow window shows name and status and never overflows. A
 * callback ref: the table mounts only after the cluster loads, so an effect run on the first render would find nothing.
 */
function useElementWidth() {
  const [element, setElement] = useState<HTMLElement | null>(null);
  const [width, setWidth] = useState(0);
  useEffect(() => {
    if (!element) return;
    const measure = () => setWidth(Math.floor(element.getBoundingClientRect().width));
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(element);
    return () => observer.disconnect();
  }, [element]);
  return [setElement, width] as const;
}

/** One node group of a cluster, read-only: its status and usage, and its nodes one server page at a time (the API reads one shard of the results bucket). */
export function NodeGroupDetail({ clusterId, group }: { clusterId: string; group: string }) {
  const { api, tenantId } = usePlatform();
  const loadCluster = useCallback(async (signal: AbortSignal) => {
    const { cluster } = await api.cluster.getCluster({ tenantId, clusterId }, { signal });
    if (!cluster || cluster.id !== clusterId || cluster.tenantId !== tenantId) throw new Error("The cluster is unavailable in the selected tenant.");
    return cluster;
  }, [api, tenantId, clusterId]);
  const cluster = useAsyncQuery(loadCluster);
  const etag = cluster.data?.etag;
  const loadManifests = useCallback(async (signal: AbortSignal) => {
    void etag;
    return (await api.cluster.listClusterNodeGroupManifests({ tenantId, clusterId }, { signal })).nodeGroups;
  }, [api, tenantId, clusterId, etag]);
  const manifests = useAsyncQuery(loadManifests);
  const loadTotals = useCallback(async (signal: AbortSignal): Promise<NodeTotalsState> => {
    try { return nodeTotalsOf(await api.cluster.getNodeResources({ tenantId, clusterId }, { signal }), group); }
    catch (error) {
      const message = nodeTotalsFailure(error);
      if (message && !signal.aborted) return { kind: "unavailable", message };
      throw error;
    }
  }, [api, tenantId, clusterId, group]);
  const totals = useAsyncQuery(loadTotals);

  const [choice, setChoice] = useState<NodeStatusChoice>("all");
  const [draft, setDraft] = useState("");
  const [search, setSearch] = useState("");
  const [pager, setPager] = useState(pagerStart);
  const [restarted, setRestarted] = useState(false);
  // The search is sent to the server once typing pauses; any new filter starts again at the first page.
  useEffect(() => {
    const timer = setTimeout(() => { setSearch(previous => { if (previous === draft.trim()) return previous; setPager(pagerStart()); return draft.trim(); }); }, SEARCH_DELAY_MS);
    return () => clearTimeout(timer);
  }, [draft]);

  const token = pager.token;
  // The latest summary, for a failed live read's notice, without reloading
  // the nodes each time the cluster is read again.
  const statusRef = useRef<ClusterStatus | undefined>(undefined);
  const clusterStatus = cluster.data?.status;
  useEffect(() => { statusRef.current = clusterStatus; }, [clusterStatus]);
  const loadNodes = useCallback(async (signal: AbortSignal) => {
    try {
      const response = await api.cluster.listNodeGroupNodes({ tenantId, clusterId, nodeGroup: group, pageSize: NODE_PAGE_SIZE, pageToken: token, filter: nodeStatusFilter(choice), nameContains: search }, { signal });
      return { response, at: new Date() };
    } catch (error) {
      // The list changed under the token (a new collection): show the first page again.
      if (token && isTokenMismatch(error) && !signal.aborted) { setPager(pagerStart()); setRestarted(true); }
      // A live read that failed shows why, with the summary's counts, and no rows.
      const failure = liveReadFailure(error, statusRef.current);
      if (failure) throw new Error(failure);
      throw error;
    }
  }, [api, tenantId, clusterId, group, token, choice, search]);
  const nodes = useAsyncQuery(loadNodes);
  const loaded = nodes.data;
  const rows = useMemo(() => loaded ? loaded.response.nodes.map(node => nodeRow(node, loaded.at)) : [], [loaded]);
  const state = loaded ? collectionState(loaded.response) : undefined;
  const [container, width] = useElementWidth();

  const detail = cluster.data ? groupDetail(cluster.data, group) : undefined;
  const found = manifests.data?.find(item => item.name === group);
  const manifest = hasManifestYaml(found) ? found : undefined;
  const back = <Link href={`/clusters/${encodeURIComponent(clusterId)}?tab=node-groups`}>Cluster node groups</Link>;
  const refresh = () => { cluster.refresh(); manifests.refresh(); nodes.refresh(); totals.refresh(); };
  const refreshing = cluster.refreshing || manifests.refreshing || nodes.refreshing || totals.refreshing;

  if (cluster.error) return <VStack gap={3}>{back}<Banner status="error" title="Node group unavailable" description={cluster.error} /><RefreshButton label="Refresh node group" onClick={refresh} isLoading={refreshing} /></VStack>;
  if (!cluster.data) return <Text role="status">Loading node group…</Text>;
  if (!detail) return <VStack gap={3}>{back}<Banner status="error" title="Node group not found" description={`Cluster ${cluster.data.name || clusterId} has no node group named ${group}.`} /></VStack>;

  const total = loaded?.response.total ?? 0;
  const filtered = choice !== "all" || !!search;
  const columns = nodeColumns(width, rows.some(row => row.nodeClaim !== "—"));
  const emptyState = !state ? "Loading nodes…"
    : !state.collected ? "Nodes have not been collected yet. The controller collects them while it reconciles the cluster's node groups, at most once a minute."
      : filtered ? "No nodes match this filter. Change the status or clear the search to see more."
        : "This node group has no nodes right now.";
  const count = nodeCount(loaded ? total : undefined, !!state?.collected, filtered, detail.observed ? detail.nodes : undefined);
  // The whole group is on screen when the list is unfiltered, complete and one page: its nodes are summed live.
  const wholeGroup = loaded && !filtered && pager.page === 1 && !loaded.response.nextPageToken && loaded.response.complete ? loaded.response.nodes : undefined;
  const live = totals.data?.kind === "ready" ? totals.data : undefined;
  const resources = live ? totalsResources(live.totals) : groupResources(detail.observed, wholeGroup);
  const clusterName = cluster.data.displayName || cluster.data.name || clusterId;
  const statusLine = state?.live ? `Live as of ${state.collectedLabel}` : state?.collected ? `Collected ${state.collectedLabel}` : state ? "Not collected yet" : "Loading…";
  return <VStack as="section" aria-label="Node group details" gap={6}>
    <DetailHeader title={detail.name} description={detail.kind === "system" ? `Default node group of ${clusterName}` : detail.kind === "pool" ? `Karpenter platform pool of ${clusterName}` : `Node group in ${clusterName}`}
      status={<HStack gap={3} vAlign="center" wrap="wrap"><StatusValue label={detail.phaseLabel} tone={detail.tone} />
        {detail.platformManaged && <HStack gap={1} vAlign="center"><Badge label={PLATFORM_MANAGED_LABEL} variant="blue" />{detail.about && <InfoTip label={detail.kind === "pool" ? "About the platform pool" : "About the system node group"} content={detail.about} />}</HStack>}</HStack>}
      actions={manifest && <NodeGroupYamlButton manifest={manifest} />}
      metadata={back} />
    {detail.message && <Banner status={detail.tone === "error" ? "error" : "warning"} title="Node group status" description={detail.message} />}
    <VStack as="section" aria-label="Node group totals" gap={3}>
      <NodeGroupResources nodes={live ? live.totals.nodes : count} resources={resources} />
      <NodeTotalsNote state={totals.data} loading={totals.loading} error={totals.error} />
      {!live && resources && <Text type="supporting">{resources.source === "live" ? `Requests summed from the ${count === 1 ? "node" : `${count} nodes`} below.` : "Requests as of the controller's last pass."}</Text>}
    </VStack>
    <VStack as="section" aria-label="Nodes" gap={3} width="100%">
      <HStack gap={3} vAlign="center" wrap="wrap"><Heading level={2}>Nodes</Heading><Text type="supporting" role="status">{statusLine}</Text></HStack>
      <HStack gap={3} vAlign="center" wrap="wrap" width="100%">
        <StackItem size="fill"><TextInput label="Search nodes" isLabelHidden type="search" value={draft} onChange={setDraft} placeholder="Search nodes by name" hasClear autoComplete="off" /></StackItem>
        <SegmentedControl label="Node status" value={choice} size="md" onChange={value => { setChoice(value as NodeStatusChoice); setPager(pagerStart()); }}>
          {nodeFilterOptions.map(option => <SegmentedControlItem key={option.value} value={option.value} label={option.label} />)}
        </SegmentedControl>
        <RefreshButton label="Refresh node group" onClick={refresh} isLoading={refreshing} />
      </HStack>
      {restarted && <Banner status="info" title="The node list changed" description="The list changed; restarting from the first page." onDismiss={() => setRestarted(false)} />}
      {nodes.error && !(token && restarted) && <Banner status="error" role="alert" title="Nodes unavailable" description={nodes.error} />}
      {state?.notices.map(notice => isInfoNotice(notice)
        ? <Banner key={notice} status="info" title="Live usage unavailable" description={`${notice}. Requests and allocatable resources still show.`} />
        : <Banner key={notice} status="warning" title={state.collected ? "Node list notice" : "Nodes unavailable"} description={notice} />)}
      <VStack ref={container} width="100%">
        <ConsoleTable aria-label={`Nodes of node group ${detail.name}`} hasFullDetails idKey="id" data={rows} columns={columns} emptyState={emptyState} />
      </VStack>
      <HStack gap={3} hAlign="between" vAlign="center" wrap="wrap" width="100%">
        <Text type="supporting" role="status" aria-label="Node range">{loaded ? pageRange(pager.page, NODE_PAGE_SIZE, rows.length, total) : ""}</Text>
        <HStack gap={2}>
          <Button label="Previous page" aria-label="Previous nodes page" variant="secondary" isDisabled={!pager.hasPrevious || nodes.loading} onClick={() => setPager(pagerBack(pager))} />
          <Button label="Next page" aria-label="Next nodes page" variant="secondary" isDisabled={!loaded?.response.nextPageToken || nodes.loading} onClick={() => setPager(pagerForward(pager, loaded?.response.nextPageToken ?? ""))} />
        </HStack>
      </HStack>
    </VStack>
  </VStack>;
}

/** A value with an optional second line in the supporting style. */
function Stacked({ value, detail, tooltip = false }: { value: string; detail?: string; tooltip?: boolean }) {
  return <VStack gap={1}>
    <Text maxLines={1} hasTruncateTooltip={tooltip}>{value}</Text>
    {detail && <Text type="supporting" maxLines={1} hasTruncateTooltip>{detail}</Text>}
  </VStack>;
}

/**
 * A flexible column whose weight follows its floor. The table's minimum width is the largest floor-to-weight ratio
 * times the summed weights, so equal ratios make that minimum exactly the sum of the floors the column budget counts.
 */
const fit = (minWidth: number) => proportional(minWidth / 100, { minWidth });

/** Below this table width the name column gives up room so name and status still fit side by side. */
const NARROW_TABLE = 400;

/**
 * The node table's columns. Name and status always show; the others join in priority order while the sum of their
 * minimum widths (each wide enough for its content) fits the table, so nothing is clipped and the table never scrolls.
 */
function nodeColumns(width: number, hasClaims: boolean): TableColumn<NodeRow>[] {
  const nameMin = width < NARROW_TABLE ? 120 : 200;
  const fixed: TableColumn<NodeRow>[] = [
    { key: "name", header: "Node", width: fit(nameMin), renderCell: row => <VStack gap={1}>
      <Text type="label" maxLines={1} hasTruncateTooltip>{row.name}</Text>
      {row.providerId && width >= 640 && <Text type="supporting" maxLines={1} hasTruncateTooltip>{row.providerId}</Text>}
    </VStack> },
    { key: "status", header: "Status", width: fit(110), renderCell: row => <VStack gap={1}>
      <StatusValue label={row.statusLabel} tone={row.tone} />
      {row.detail && <Text type="supporting" maxLines={1} hasTruncateTooltip>{row.detail}</Text>}
    </VStack> },
  ];
  const optional: [number, TableColumn<NodeRow>][] = [
    [150, { key: "instanceType", header: "Instance", width: fit(150), renderCell: row =>
      <Stacked value={row.instanceType} detail={[row.capacity !== "—" ? row.capacity : "", row.architecture].filter(Boolean).join(" · ")} /> }],
    [130, { key: "cpu", header: "CPU", align: "end", width: fit(130), renderCell: row => <Stacked value={row.cpu} detail={row.cpuDetail} /> }],
    [130, { key: "memory", header: "Memory", align: "end", width: fit(130), renderCell: row => <Stacked value={row.memory} detail={row.memoryDetail} /> }],
    [150, { key: "pods", header: "Pods", align: "end", width: fit(150), renderCell: row => <Stacked value={row.pods} detail={row.podsDetail} /> }],
    [110, { key: "zone", header: "Zone", width: fit(110), renderCell: row => <Text>{row.zone}</Text> }],
    [80, { key: "age", header: "Age", align: "end", width: fit(80), renderCell: row => <Text>{row.age}</Text> }],
    [180, { key: "kubeletVersion", header: "Kubelet", width: fit(180), renderCell: row => <Text>{row.kubeletVersion}</Text> }],
    ...(hasClaims ? [[200, { key: "nodeClaim", header: "NodeClaim", width: fit(200), renderCell: (row: NodeRow) => <VStack gap={1}>
      <Text maxLines={1} hasTruncateTooltip>{row.nodeClaim}</Text>
      {row.nodeClaimDetail && <StatusValue label={row.nodeClaimDetail} tone={row.nodeClaimTone} />}
    </VStack> }] as [number, TableColumn<NodeRow>]] : []),
  ];
  // Greedy and in order: a column that does not fit is skipped, a later narrower one may still join.
  const { columns } = optional.reduce((acc, [min, column]) => acc.used + min <= width
    ? { used: acc.used + min, columns: [...acc.columns, column] } : acc, { used: nameMin + 110, columns: fixed });
  return columns;
}
