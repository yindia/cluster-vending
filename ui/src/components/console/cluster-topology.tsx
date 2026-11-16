"use client";

import { useCallback, useEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";
import { Background, MarkerType, ReactFlow, type Edge, type ReactFlowInstance } from "@xyflow/react";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { CheckboxInput } from "@astryxdesign/core/CheckboxInput";
import { Card } from "@astryxdesign/core/Card";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { Link } from "@astryxdesign/core/Link";
import { Text } from "@astryxdesign/core/Text";
import { MetadataList, MetadataListItem } from "@astryxdesign/core/MetadataList";
import { usePlatform } from "@/components/platform-provider";
import { useAsyncQuery } from "@/lib/api/use-query";
import type { Cluster } from "@/gen/cloud/v1/cluster_pb";
import type { Network } from "@/gen/cloud/v1/network_pb";
import { buildClusterArchitecture, clusterRelationship, requireClusterTopologyScope } from "@/lib/workflows/cluster-architecture";
import type { ArchitectureDimensions } from "@/lib/workflows/network-topology";
import { clusterComputeDetails } from "@/lib/workflows/cluster-topology-details";
import { routeArchitectureEdges } from "@/lib/workflows/network-edge-routing";
import { architectureNodeTypes, architectureEdgeTypes, architectureGraphTheme, traceArchitecture, type FlowNode } from "./network-topology";
import { ConsoleTable } from "./console-table";
import { CopyValue } from "./copy-value";
import { StatusValue } from "./detail-layout";
import { RefreshButton } from "./refresh-button";
import { KubernetesIcon } from "./provider-icons";

const unavailable = () => false;
const subscribeFullscreen = (notify: () => void) => { document.addEventListener("fullscreenchange", notify); return () => document.removeEventListener("fullscreenchange", notify); };
const subscribeCompact = (notify: () => void) => { const media = window.matchMedia("(max-width: 48rem)"); media.addEventListener("change", notify); return () => media.removeEventListener("change", notify); };
const compactSnapshot = () => window.matchMedia("(max-width: 48rem)").matches;

export function ClusterTopology({ cluster, network }: { cluster: Cluster; network?: Network }) {
  const { api, tenantId } = usePlatform();
  const load = useCallback(async (signal: AbortSignal) => {
    const result = await api.cluster.getCluster({ tenantId, clusterId: cluster.id, live: true }, { signal });
    return requireClusterTopologyScope(result.cluster, tenantId, cluster.id);
  }, [api, tenantId, cluster.id]);
  const live = useAsyncQuery(load, { pollMs: 5000 });
  const safeCluster = live.data ?? (cluster.tenantId === tenantId ? cluster : undefined);
  const linkedNetwork = network && network.tenantId === tenantId && network.id === safeCluster?.spec?.network?.networkId ? network : undefined;
  const [showNetwork, setShowNetwork] = useState(false);
  const networkId = safeCluster?.spec?.network?.networkId;
  const loadNetwork = useCallback(async (signal: AbortSignal) => {
    if (!showNetwork || !networkId) return undefined;
    const result = await api.network.getNetwork({ tenantId, networkId, live: true }, { signal });
    if (!result.network || result.network.id !== networkId || result.network.tenantId !== tenantId) throw new Error("The network architecture response did not match this cluster and tenant.");
    return result.network;
  }, [api, tenantId, networkId, showNetwork]);
  const liveNetwork = useAsyncQuery(loadNetwork, { pollMs: 5000 });
  const overlayNetwork = liveNetwork.data ?? linkedNetwork;
  const [dimensions, setDimensions] = useState<ArchitectureDimensions>(new Map());
  const reportSize = useCallback((id: string, size: { width: number; height: number; headerHeight?: number; headerWidth?: number }) => {
    setDimensions(previous => {
      const old = previous.get(id);
      if (old && old.width >= size.width && old.height >= size.height && old.headerWidth === size.headerWidth && (old.headerHeight ?? 0) >= (size.headerHeight ?? 0)) return previous;
      const next = new Map(previous);
      next.set(id, { width: Math.max(old?.width ?? 0, size.width), height: Math.max(old?.height ?? 0, size.height), headerHeight: Math.max(old?.headerHeight ?? 0, size.headerHeight ?? 0), headerWidth: size.headerWidth });
      return next;
    });
  }, []);
  const graph = useMemo(() => buildClusterArchitecture(safeCluster, dimensions, { showNetwork, network: overlayNetwork }), [safeCluster, dimensions, showNetwork, overlayNetwork]);
  const [selectedId, select] = useState("");
  const excludedKinds = new Set(["Addon", "Release", "Component", "Pod", "Deployment", "DaemonSet", "Service", "ConfigMap", "Secret", "Controller"]);
  const inspectorResources = graph.inventoryNodes.filter(node => !excludedKinds.has(node.data.kind));
  const inspectorIds = new Set(inspectorResources.map(node => node.id));
  const selected = inspectorResources.find(node => node.id === selectedId) ?? graph.nodes.find(node => node.id === selectedId);
  const related = graph.allEdges.filter(edge => inspectorIds.has(edge.source) && inspectorIds.has(edge.target) && (edge.source === selectedId || edge.target === selectedId));
  const inspection = useMemo(() => {
    const ids = new Set(graph.nodes.map(node => node.id));
    const edges = graph.allEdges.filter(edge => ids.has(edge.source) && ids.has(edge.target));
    const owner = graph.nodes.find(node => node.data.attachments?.some(attachment => attachment.graphId === selectedId));
    const traced = traceArchitecture({ nodes: graph.nodes, edges }, owner?.id ?? selectedId);
    return { traced, edges: edges.filter(edge => edge.data.relation !== "nat_internet_egress" || traced.has(edge.id)) };
  }, [graph, selectedId]);
  const relatedIds = new Set([...related.map(edge => edge.id), ...inspection.traced]);
  const highlighted = new Set(inspection.edges.filter(edge => relatedIds.has(edge.id)).flatMap(edge => [edge.source, edge.target]));
  const inspectedTables = inspectorResources.filter(node => node.data.kind === "RouteTable" && (
    node.id === selectedId || graph.allEdges.some(edge =>
      selected?.data.kind === "Subnet" ? edge.source === selectedId && edge.target === node.id && edge.data.relation === "routes_via"
        : ["NATGateway", "InternetGateway"].includes(selected?.data.kind ?? "") && edge.source === node.id && edge.target === selectedId && edge.data.relation === "forwards_to"
    )
  ));
  const nodes: FlowNode[] = graph.nodes.map(node => ({ ...node, type: node.type === "group" ? "boundary" : node.type,
    width: node.width, height: node.height, measured: { width: node.width, height: node.height }, style: { width: node.width, height: node.height },
    selected: node.id === selectedId || highlighted.has(node.id), ariaLabel: `${node.data.kind}: ${node.data.title}`,
    data: { ...node.data, summary: safeCluster && ["NodeGroup", "NodePool", "Node", "Instance", "EC2Instance", "WorkerNode", "KarpenterNode", "Cluster"].includes(node.data.kind) ? clusterComputeDetails(safeCluster, node.data).filter(row => node.data.kind === "Cluster" ? row.label === "Kubernetes version" : row.label !== "Initial desired nodes") : undefined,
      summaryNote: node.data.kind === "Cluster" ? safeCluster?.spec?.provider.case === "aws" ? "AWS-managed control plane" : "Managed control plane" : safeCluster && ["NodeGroup", "NodePool"].includes(node.data.kind) && clusterComputeDetails(safeCluster, node.data).some(row => row.source === "Configured default pool") ? "Configured default pool" : undefined, nodeId: node.id, inspect: () => select(node.id), inspectAttachment: select, reportSize } }));
  const routing = useMemo(() => {
    try {
      const context = typeof document === "undefined" ? null : document.createElement("canvas").getContext("2d");
      if (context) context.font = `12px ${getComputedStyle(document.body).fontFamily}`;
      return { edges: routeArchitectureEdges(graph.nodes, inspection.edges, { measureLabel: text => ({ width: Math.ceil(context?.measureText(text).width ?? text.length * 8) + 16, height: 24 }) }), error: "" };
    } catch { return { edges: [], error: "Some connections could not be placed safely. Select a resource to inspect its recorded relationships." }; }
  }, [graph, inspection.edges]);
  const edges: Edge[] = routing.edges.map(edge => {
    const color = selected && relatedIds.has(edge.id) ? "var(--color-text-blue)" : "var(--color-text-secondary)";
    const association = edge.data.relationship === "associated-with" || edge.data.relationship === "attached-to";
    return { ...edge, type: "architecture", sourceHandle: `out-${edge.data.route.sourceSide}`, targetHandle: `in-${edge.data.route.targetSide}`,
      markerEnd: association ? undefined : { type: MarkerType.ArrowClosed, color }, zIndex: 1, interactionWidth: 0,
      ariaLabel: `${clusterRelationship(edge.data.relation)}, ${edge.data.evidence}${edge.data.derived ? " logical path" : ""}`,
      style: { stroke: color, strokeWidth: selected && relatedIds.has(edge.id) ? 2.5 : 1.5, strokeDasharray: association || edge.data.evidence === "configured" ? "5 4" : edge.data.derived ? "2 4" : undefined, opacity: selected && !relatedIds.has(edge.id) ? 0.15 : 1, pointerEvents: "none" } };
  });
  const container = useRef<HTMLDivElement>(null), flow = useRef<ReactFlowInstance<FlowNode> | null>(null);
  const fullscreen = useSyncExternalStore(subscribeFullscreen, () => !!container.current && document.fullscreenElement === container.current, unavailable);
  const compact = useSyncExternalStore(subscribeCompact, compactSnapshot, unavailable);
  useEffect(() => {
    const escape = (event: KeyboardEvent) => { if (event.key === "Escape" && document.fullscreenElement === container.current) void document.exitFullscreen().catch(() => undefined); };
    document.addEventListener("keydown", escape);
    return () => document.removeEventListener("keydown", escape);
  }, []);
  const [fullscreenError, setFullscreenError] = useState("");
  const fit = async () => {
    await new Promise<void>(resolve => requestAnimationFrame(() => requestAnimationFrame(() => resolve())));
    await flow.current?.fitView({ padding: "8%", duration: 0, maxZoom: 1 });
  };
  const toggleFullscreen = async () => {
    setFullscreenError("");
    try { if (document.fullscreenElement === container.current) await document.exitFullscreen(); else await container.current?.requestFullscreen(); await fit(); }
    catch { setFullscreenError("Fullscreen is unavailable. Pan, zoom and fit remain available."); }
  };
  const layoutKey = JSON.stringify(graph.nodes.map(node => [node.id, node.parentId, node.position.x, node.position.y, node.width, node.height]));
  useEffect(() => {
    let second = 0;
    const first = requestAnimationFrame(() => { second = requestAnimationFrame(() => { void flow.current?.fitView({ padding: "8%", duration: 0, maxZoom: 0.9 }); }); });
    return () => { cancelAnimationFrame(first); cancelAnimationFrame(second); };
  }, [layoutKey]);
  const resourceById = new Map(inspectorResources.map(node => [node.id, node]));
  return <VStack gap={3}>
    {showNetwork && liveNetwork.error && <Banner status="warning" title="Live network architecture unavailable" description={liveNetwork.error} />}
    {live.error && <Banner status="warning" title="Live architecture unavailable" description={live.error} />}
    {routing.error && <Banner status="warning" title="Routing unavailable" description={routing.error} />}
    {fullscreenError && <Banner status="warning" title="Fullscreen unavailable" description={fullscreenError} />}
    <Card ref={container} role="region" aria-label="Cluster topology diagram" padding={0} width="100%" height={fullscreen ? "100dvh" : "min(80dvh, 56rem)"} minHeight="28rem" style={{ display: "flex", flexDirection: "column", ...(fullscreen ? { borderRadius: 0 } : {}) }}>
      <HStack padding={3} gap={3} wrap="wrap" hAlign="between" vAlign="center" style={{ flex: "0 0 auto", borderBlockEnd: "thin solid var(--color-border)" }}>
        <VStack gap={2} style={{ minWidth: 0 }}>
          <HStack gap={2} vAlign="center"><KubernetesIcon size="sm" /><Heading level={2}>Cluster architecture</Heading></HStack>
          <HStack gap={2} wrap="wrap" vAlign="center"><Text type="supporting">{safeCluster?.spec?.region || "Region not reported"} · {graph.resources.length} resources{live.loading || showNetwork && liveNetwork.loading ? " · Loading…" : ""}</Text>{linkedNetwork && <Link href={`/networks/${encodeURIComponent(linkedNetwork.id)}`}>{linkedNetwork.displayName || linkedNetwork.name || linkedNetwork.id}</Link>}</HStack>
        </VStack>
        <RefreshButton label="Refresh cluster topology" onClick={() => { live.refresh(); if (showNetwork) liveNetwork.refresh(); }} isLoading={live.refreshing || showNetwork && liveNetwork.refreshing} />
      </HStack>
      <HStack padding={2} gap={3} wrap="wrap" hAlign="between" vAlign="center" aria-label="Diagram controls" role="group" style={{ flex: "0 0 auto", borderBlockEnd: "thin solid var(--color-border)" }}>
        <HStack gap={3} wrap="wrap" vAlign="center"><Text type="supporting">Solid: route / ownership · Dashed: association / configured</Text><Text type="supporting">{showNetwork ? "Cluster + Network" : "Cluster Architecture"}</Text></HStack>
        <HStack gap={2} wrap="wrap" vAlign="center"><CheckboxInput label="Show Network" size="sm" value={showNetwork} onChange={value => { setShowNetwork(value); select(""); }} /><Text type="supporting">{showNetwork ? "On" : "Off"}</Text><Button label="Zoom out" variant="ghost" size="sm" onClick={() => { void flow.current?.zoomOut({ duration: 0 }); }}>−</Button><Button label="Zoom in" variant="ghost" size="sm" onClick={() => { void flow.current?.zoomIn({ duration: 0 }); }}>+</Button><Button label="Fit view" variant="ghost" size="sm" onClick={fit} /><Button label={fullscreen ? "Exit fullscreen" : "Enter fullscreen"} variant="ghost" size="sm" clickAction={toggleFullscreen} /></HStack>
      </HStack>
      {!graph.resources.length && <VStack gap={2} padding={4}><Heading level={3}>{live.loading ? "Loading cluster architecture" : "No cluster architecture reported"}</Heading><Text type="supporting">{live.loading ? "Reading current resource references." : "Refresh after the server and controller report cluster relationships. Resource inventory remains available in the Resources tab."}</Text></VStack>}
      <HStack gap={0} width="100%" vAlign="stretch" wrap="wrap" style={{ flex: "1 1 0%", minHeight: 0, overflow: "auto" }}>
        <VStack style={{ display: compact && selected ? "none" : "flex", flex: "1 1 24rem", minWidth: 0, minHeight: "20rem", height: "100%" }}>
          <ReactFlow<FlowNode> aria-label="Cluster topology canvas" nodes={nodes} edges={edges} nodeTypes={architectureNodeTypes} edgeTypes={architectureEdgeTypes}
            onInit={instance => { flow.current = instance; }} onNodeClick={(_, node) => { if (node.data.kind !== "Visual category") select(node.id); }} onPaneClick={() => select("")}
            nodesDraggable={false} nodesConnectable={false} edgesReconnectable={false} deleteKeyCode={null} nodesFocusable edgesFocusable
            fitView fitViewOptions={{ padding: "8%", maxZoom: 0.9 }} minZoom={0.04} maxZoom={2} zoomOnScroll={false} zoomOnDoubleClick={false} preventScrolling={false} style={architectureGraphTheme}>
            <Background color="var(--color-border)" />
          </ReactFlow>
        </VStack>
        {selected && <VStack as="aside" role="region" aria-label="Selected cluster topology resource" gap={3} padding={3} style={{ flex: compact ? "1 1 100%" : "0 1 24rem", minWidth: 0, maxHeight: "100%", overflow: "auto", borderInlineStart: "thin solid var(--color-border)", background: "var(--color-background-card)" }}>
          <HStack gap={2} vAlign="start" hAlign="between"><VStack gap={1} style={{ minWidth: 0 }}><Text type="supporting">{selected.data.kind}</Text><Heading level={3} style={{ overflowWrap: "anywhere" }}>{selected.data.title}</Heading></VStack><Button label="Close inspector" size="sm" variant="ghost" onClick={() => select("")} /></HStack>
          <StatusValue label={`${selected.data.state}${selected.data.paused ? " · Paused" : ""}`} tone={selected.data.tone} />
          <MetadataList columns={1}>
            {selected.data.resourceId && <MetadataListItem label="Managed resource"><CopyValue value={selected.data.resourceId} label="managed resource name" /></MetadataListItem>}
            {selected.data.cloudId && <MetadataListItem label="Cloud resource ID"><CopyValue value={selected.data.cloudId} label="cloud resource ID" /></MetadataListItem>}
            {selected.data.zoneId && <MetadataListItem label="Availability Zone">{selected.data.zoneId}</MetadataListItem>}
            {selected.data.cidr && <MetadataListItem label="CIDR">{selected.data.cidr}</MetadataListItem>}
          </MetadataList>
          <Text type="supporting">{selected.data.message}</Text>
          {safeCluster && <MetadataList columns={1}>{clusterComputeDetails(safeCluster, selected.data).map(row => <MetadataListItem key={row.label} label={row.label}><VStack gap={1}><>{["Cluster ARN", "VPC"].includes(row.label) && row.value !== "Not reported" ? <CopyValue value={row.value} label={row.label} /> : <Text>{row.value}</Text>}</>{row.source && <Text type="supporting">{row.source}</Text>}</VStack></MetadataListItem>)}</MetadataList>}
          {["NodeGroup", "NodePool"].includes(selected.data.kind) && <Text type="supporting">Live desired and current node counts are not supplied by this snapshot. Initial desired size is a create-time setting.</Text>}
          {inspectorResources.filter(node => node.data.attachments?.some(attachment => attachment.graphId === selectedId)).map(owner => <Button key={owner.id} label={`Inspect ${owner.data.title}`} variant="ghost" size="sm" onClick={() => select(owner.id)}>Attached to {owner.data.title}</Button>)}
          {inspectedTables.map(table => <VStack key={table.id} gap={2}><Heading level={4}>Route entries</Heading>{table.id !== selectedId && <Button label={`Inspect ${table.data.title}`} variant="ghost" size="sm" onClick={() => select(table.id)}>{table.data.title}</Button>}<ConsoleTable aria-label={`Routes for ${table.data.title}`} data={(table.data.routes ?? []).map((route, index) => ({ ...route, key: `${route.id}:${index}` }))} idKey="key" columns={[
            { key: "destination", header: "Destination" },
            { key: "targetName", header: "Target", renderCell: route => resourceById.has(route.targetId) ? <Button label={`Inspect route target ${route.targetName}`} tooltip={route.targetName} variant="ghost" size="sm" onClick={() => select(route.targetId)}>{route.targetName}</Button> : <Text maxLines={2} hasTruncateTooltip>{route.targetName || "Target not reported"}</Text> },
            { key: "evidence", header: "Reference" },
          ]} /></VStack>)}
          <Heading level={4}>Relationships</Heading>
          <ConsoleTable aria-label="Selected cluster resource relationships" data={related.map(edge => ({ id: edge.id, relation: clusterRelationship(edge.data.relation), evidence: edge.data.evidence, direction: edge.source === selectedId ? "Outgoing" : "Incoming", resource: resourceById.get(edge.source === selectedId ? edge.target : edge.source) }))} idKey="id" columns={[
            { key: "relation", header: "Relationship", renderCell: row => <VStack gap={1}><Text type="supporting">{row.relation}</Text><Text type="supporting">{row.direction} · {row.evidence}</Text></VStack> },
            { key: "resource", header: "Resource", renderCell: row => row.resource ? <VStack gap={1}><Button label={`Inspect ${row.resource.data.title}`} tooltip={row.resource.data.title} size="sm" variant="ghost" onClick={() => select(row.resource!.id)}>{row.resource.data.title}</Button>{row.resource.data.cloudId && <CopyValue value={row.resource.data.cloudId} label="related resource ID" isSecondary />}</VStack> : <Text>Not reported</Text> },
          ]} />
          {!related.length && <Text type="supporting">No explicit relationships reported.</Text>}
        </VStack>}
      </HStack>
    </Card>
    {!!graph.diagnostics.length && !(showNetwork && liveNetwork.loading) && <Banner status="warning" title="Incomplete architecture" description={graph.diagnostics.join(" ")} />}
  </VStack>;
}
