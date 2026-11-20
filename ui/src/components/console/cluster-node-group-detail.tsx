"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Badge } from "@astryxdesign/core/Badge";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { Link } from "@astryxdesign/core/Link";
import { SegmentedControl, SegmentedControlItem } from "@astryxdesign/core/SegmentedControl";
import { proportional, type TableColumn } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { usePlatform } from "@/components/platform-provider";
import { useAsyncQuery } from "@/lib/api/use-query";
import {
  NODE_PAGE_SIZE, collectionState, groupDetail, isTokenMismatch, nodeFilterOptions, nodeRow, nodeStatusFilter, pageRange, pagerBack, pagerForward, pagerStart,
  type NodeRow, type NodeStatusChoice,
} from "@/lib/workflows/node-group-nodes";
import { ConsoleTable } from "./console-table";
import { DetailHeader, DetailSummary, StatusValue } from "./detail-layout";
import { NodeGroupYamlButton } from "./node-group-yaml-button";
import { RefreshButton } from "./refresh-button";

const SEARCH_DELAY_MS = 300;

/** Optional columns follow the table's own width, so a narrow window shows name and status and never overflows. */
function useElementWidth() {
  const ref = useRef<HTMLElement>(null);
  const [width, setWidth] = useState(0);
  useEffect(() => {
    const element = ref.current;
    if (!element) return;
    const measure = () => setWidth(Math.floor(element.getBoundingClientRect().width));
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(element);
    return () => observer.disconnect();
  }, []);
  return [ref, width] as const;
}

/** One node group of a cluster: its settings, and its nodes one server page at a time (the API reads one shard of the results bucket). */
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
  const loadNodes = useCallback(async (signal: AbortSignal) => {
    try {
      const response = await api.cluster.listNodeGroupNodes({ tenantId, clusterId, nodeGroup: group, pageSize: NODE_PAGE_SIZE, pageToken: token, filter: nodeStatusFilter(choice), nameContains: search }, { signal });
      return { response, at: new Date() };
    } catch (error) {
      // The list changed under the token (a new collection): show the first page again.
      if (token && isTokenMismatch(error) && !signal.aborted) { setPager(pagerStart()); setRestarted(true); }
      throw error;
    }
  }, [api, tenantId, clusterId, group, token, choice, search]);
  const nodes = useAsyncQuery(loadNodes);
  const loaded = nodes.data;
  const rows = useMemo(() => loaded ? loaded.response.nodes.map(node => nodeRow(node, loaded.at)) : [], [loaded]);
  const state = loaded ? collectionState(loaded.response) : undefined;
  const [container, width] = useElementWidth();

  const detail = cluster.data ? groupDetail(cluster.data, group) : undefined;
  const manifest = manifests.data?.find(item => item.name === group);
  const back = <Link href={`/clusters/${encodeURIComponent(clusterId)}?tab=node-groups`}>Cluster node groups</Link>;
  const refresh = () => { cluster.refresh(); manifests.refresh(); nodes.refresh(); };
  const refreshing = cluster.refreshing || manifests.refreshing || nodes.refreshing;

  if (cluster.error) return <VStack gap={3}>{back}<Banner status="error" title="Node group unavailable" description={cluster.error} /><RefreshButton label="Refresh node group" onClick={refresh} isLoading={refreshing} /></VStack>;
  if (!cluster.data) return <Text role="status">Loading node group…</Text>;
  if (!detail) return <VStack gap={3}>{back}<Banner status="error" title="Node group not found" description={`Cluster ${cluster.data.name || clusterId} has no node group named ${group}.`} /></VStack>;

  const total = loaded?.response.total ?? 0;
  const filtered = choice !== "all" || !!search;
  const columns: TableColumn<NodeRow>[] = [
    { key: "name", header: "Node", width: proportional(3, { minWidth: 140 }), renderCell: row => <VStack gap={1}>
      <Text type="label" maxLines={1} hasTruncateTooltip>{row.name}</Text>
      {row.providerId && width >= 640 && <Text type="supporting" maxLines={1} hasTruncateTooltip>{row.providerId}</Text>}
    </VStack> },
    { key: "status", header: "Status", width: proportional(2, { minWidth: 110 }), renderCell: row => <VStack gap={1}>
      <StatusValue label={row.statusLabel} tone={row.tone} />
      {row.detail && <Text type="supporting">{row.detail}</Text>}
    </VStack> },
    ...(width >= 520 ? [{ key: "instanceType", header: "Instance type", width: proportional(1.5, { minWidth: 100 }), renderCell: (row: NodeRow) => <Text>{row.instanceType}</Text> }] : []),
    ...(width >= 680 ? [{ key: "zone", header: "Zone", width: proportional(1.2, { minWidth: 90 }), renderCell: (row: NodeRow) => <Text>{row.zone}</Text> }] : []),
    ...(width >= 800 ? [{ key: "capacity", header: "Capacity", width: proportional(1, { minWidth: 90 }), renderCell: (row: NodeRow) => <Text>{row.capacity}</Text> }] : []),
    ...(width >= 920 ? [{ key: "kubeletVersion", header: "Kubelet", width: proportional(1.3, { minWidth: 100 }), renderCell: (row: NodeRow) => <Text>{row.kubeletVersion}</Text> }] : []),
    ...(width >= 1000 ? [{ key: "age", header: "Age", align: "end" as const, width: proportional(0.6, { minWidth: 60 }), renderCell: (row: NodeRow) => <Text>{row.age}</Text> }] : []),
    ...(width >= 1100 ? [{ key: "nodeClaim", header: "NodeClaim", width: proportional(2, { minWidth: 130 }), renderCell: (row: NodeRow) => <VStack gap={1}>
      <Text maxLines={1} hasTruncateTooltip>{row.nodeClaim}</Text>
      {row.nodeClaimDetail && <StatusValue label={row.nodeClaimDetail} tone={row.nodeClaimTone} />}
    </VStack> }] : []),
  ];
  const emptyState = !state ? "Loading nodes…"
    : !state.collected ? "Nodes have not been collected yet. The controller collects them while it reconciles the cluster's node groups, at most once a minute."
      : filtered ? "No nodes match this filter. Change the status or clear the search to see more."
        : "This node group has no nodes right now.";
  const nodeCount = detail.nodes ?? (!filtered && state?.collected ? total : undefined);
  return <VStack as="section" aria-label="Node group details" gap={6}>
    <DetailHeader title={detail.name} description={`Node group in ${cluster.data.displayName || cluster.data.name || clusterId}`}
      status={<HStack gap={3} vAlign="center" wrap="wrap"><StatusValue label={detail.phaseLabel} tone={detail.tone} />{detail.platformManaged && <Badge label="Managed by platform" variant="blue" />}</HStack>}
      actions={<>{manifest && <NodeGroupYamlButton manifest={manifest} />}<RefreshButton label="Refresh node group" onClick={refresh} isLoading={refreshing} /></>}
      metadata={back} />
    {detail.message && <Banner status={detail.tone === "error" ? "error" : "warning"} title="Node group status" description={detail.message} />}
    <DetailSummary items={[
      { label: "Phase", value: <StatusValue label={detail.phaseLabel} tone={detail.tone} /> },
      { label: "Nodes", value: nodeCount === undefined ? "—" : String(nodeCount), note: detail.nodes === undefined ? "Not reported yet" : undefined },
      ...detail.summary,
    ]} />
    <VStack as="section" aria-label="Nodes" gap={3} width="100%">
      <HStack gap={3} hAlign="between" vAlign="end" wrap="wrap" width="100%">
        <VStack gap={1}><Text type="large" weight="semibold">Nodes</Text>
          <Text type="supporting" role="status">{state?.collected ? `Collected ${state.collectedLabel}` : state ? "Not collected yet" : "Loading…"}</Text></VStack>
        <HStack gap={3} vAlign="end" wrap="wrap">
          <SegmentedControl label="Node status" value={choice} size="md" onChange={value => { setChoice(value as NodeStatusChoice); setPager(pagerStart()); }}>
            {nodeFilterOptions.map(option => <SegmentedControlItem key={option.value} value={option.value} label={option.label} />)}
          </SegmentedControl>
          <TextInput label="Search nodes" type="search" value={draft} onChange={setDraft} placeholder="Node name" hasClear autoComplete="off" width="min(100%, 18rem)" />
        </HStack>
      </HStack>
      {restarted && <Banner status="info" title="The node list changed" description="The controller collected the nodes again, so the list starts from the first page." onDismiss={() => setRestarted(false)} />}
      {nodes.error && !(token && restarted) && <Banner status="error" role="alert" title="Nodes unavailable" description={nodes.error} />}
      {state?.notices.map(notice => <Banner key={notice} status="warning" title={state.collected ? "Node list notice" : "Nodes unavailable"} description={notice} />)}
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
