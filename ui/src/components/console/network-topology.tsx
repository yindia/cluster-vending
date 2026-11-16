"use client";

import { useCallback, useEffect, useMemo, useRef, useState, useSyncExternalStore, type CSSProperties } from "react";
import { Background, BaseEdge, Handle, MarkerType, Position, ReactFlow, type Edge, type EdgeProps, type Node, type NodeProps, type ReactFlowInstance } from "@xyflow/react";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { CheckboxInput } from "@astryxdesign/core/CheckboxInput";
import { Heading } from "@astryxdesign/core/Heading";
import { Icon } from "@astryxdesign/core/Icon";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { Link } from "@astryxdesign/core/Link";
import { MetadataList, MetadataListItem } from "@astryxdesign/core/MetadataList";
import { Text } from "@astryxdesign/core/Text";
import { usePlatform } from "@/components/platform-provider";
import type { Network } from "@/gen/cloud/v1/network_pb";
import type { CloudAccount } from "@/gen/cloud/v1/tenant_service_pb";
import { useAsyncQuery } from "@/lib/api/use-query";
import { collectNetworkClusters } from "@/lib/workflows/networks";
import { buildNetworkArchitectureOverview, type ArchitectureDimensions, type ArchitectureEdge, type NetworkArchitectureGraph, type TopologyNodeData } from "@/lib/workflows/network-topology";
import { ArchitectureRoutingError, architectureEdgePath, routeArchitectureEdges, type EdgeRoute } from "@/lib/workflows/network-edge-routing";
import { inspectArchitectureSecurity, architectureSecurityCoverage } from "@/lib/workflows/network-architecture-security";
import { networkProvider } from "@/lib/workflows/networks";
import { ProviderIcon } from "./provider-icons";
import { ConsoleTable } from "./console-table";
import { CopyValue } from "./copy-value";
import { StatusValue } from "./detail-layout";
import { RefreshButton } from "./refresh-button";

type NodeSize = { width: number; height: number; headerHeight?: number; headerWidth?: number };
type FlowData = TopologyNodeData & { summary?: { label: string; value: string }[]; summaryNote?: string; securityNotes?: string[]; nodeId: string; inspect: () => void; inspectAttachment: (id: string) => void; reportSize: (id: string, size: NodeSize) => void };
function useNodeMeasurement({ nodeId, reportSize, presentation }: FlowData) {
  const root = useRef<HTMLDivElement>(null), content = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const element = root.current, body = content.current;
    if (!element || !body) return;
    const observer = new ResizeObserver(() => {
      const style = getComputedStyle(element);
      const horizontal = parseFloat(style.paddingLeft) + parseFloat(style.paddingRight) + parseFloat(style.borderLeftWidth) + parseFloat(style.borderRightWidth);
      const vertical = parseFloat(style.paddingTop) + parseFloat(style.paddingBottom) + parseFloat(style.borderTopWidth) + parseFloat(style.borderBottomWidth);
      reportSize(nodeId, {
        width: Math.ceil(presentation === "group" ? body.scrollWidth + horizontal : Math.max(element.offsetWidth, body.scrollWidth + horizontal)),
        height: Math.ceil(presentation === "group" ? body.scrollHeight + vertical : Math.max(element.offsetHeight, body.scrollHeight + vertical)),
        ...(presentation === "group" ? { headerHeight: Math.ceil(body.offsetHeight + parseFloat(style.borderTopWidth) + parseFloat(getComputedStyle(body.parentElement!).borderTopWidth || "0")), headerWidth: Math.ceil(body.offsetWidth) } : {}),
      });
    });
    observer.observe(element); observer.observe(body);
    return () => observer.disconnect();
  }, [nodeId, reportSize, presentation]);
  return { root, content };
}
export type FlowNode = Node<FlowData, "resource" | "boundary" | "external">;
const sides = [Position.Top, Position.Right, Position.Bottom, Position.Left] as const;
function ResourceHandles() {
  return sides.map(side => <Handle key={side} id={`out-${side}`} type="source" position={side} isConnectable={false} style={{ opacity: 0 }} />).concat(
    sides.map(side => <Handle key={`in-${side}`} id={`in-${side}`} type="target" position={side} isConnectable={false} style={{ opacity: 0 }} />),
  );
}
type ArchitectureColor = "blue" | "purple" | "cyan" | "teal" | "orange" | "gray";
function zoneColor(zoneId: string): ArchitectureColor {
  const hash = Array.from(zoneId).reduce((sum, character) => (sum * 31 + character.charCodeAt(0)) >>> 0, 0);
  return (["blue", "purple", "cyan"] as const)[hash % 3];
}
function architectureColor(data: FlowData): ArchitectureColor {
  if (data.kind === "Visual category") return ({ "Control plane": "blue", Compute: "orange", "Network and dependencies": "cyan", "Add-ons and components": "purple" } as Record<string, ArchitectureColor>)[data.title] ?? "gray";
  if (["NodeGroup", "LaunchTemplate"].includes(data.kind)) return "orange";
  if (["Addon", "Release"].includes(data.kind)) return "purple";
  if (data.kind === "SecurityGroup") return "cyan";
  if (data.boundary === "account") return "orange";
  if (data.boundary === "zone") return zoneColor(data.zoneId || data.title);
  if (data.boundary === "routing") return "purple";
  if (data.boundary === "shared") return "gray";
  if (data.boundary === "public" || data.kind === "NATGateway") return "teal";
  if (data.boundary === "private") return "blue";
  if (data.kind === "RouteTable") return "purple";
  if (data.kind === "InternetGateway") return "orange";
  if (data.kind === "EIP") return "cyan";
  return data.kind === "Route" || data.presentation === "external" ? "gray" : "blue";
}
function architectureTint(color: ArchitectureColor, amount: number) {
  return `color-mix(in oklab, var(--color-background-${color}) ${amount}%, var(--color-background-card))`;
}
function friendlyKind(data: Pick<TopologyNodeData, "kind" | "boundary">): string {
  if (data.kind === "Subnet") return data.boundary === "public" ? "Public subnet" : data.boundary === "private" ? "Private subnet" : "Subnet";
  return ({ NodeGroup: "Node group", LaunchTemplate: "Launch template", SecurityGroup: "Security group", Addon: "Managed add-on", Release: "Helm release", InternetGateway: "Internet gateway", NATGateway: "NAT gateway", EIP: "Elastic IP", RouteTable: "Route table" } as Record<string, string>)[data.kind] || data.kind;
}
function resourcePurpose(data: TopologyNodeData): string {
  return ({ NodeGroup: "Managed worker capacity", LaunchTemplate: "Node launch configuration", SecurityGroup: "Network access rules", Addon: "Managed Kubernetes add-on", Release: "Helm component", NATGateway: "Private subnet outbound access", InternetGateway: "Connects public routes to the internet", Cluster: "Kubernetes control plane", EIP: "Public egress address", RouteTable: "Selects the next hop for a destination" } as Record<string, string>)[data.kind] || data.cloudId || data.resourceId || "Logical destination";
}
function BoundaryNode({ data, selected }: NodeProps<FlowNode>) {
  const { root, content } = useNodeMeasurement(data);
  const zone = data.boundary === "zone";
  const context = ["account", "zone", "region", "routing", "shared"].includes(data.boundary || "");
  const color = architectureColor(data);
  const ink = `var(--color-text-${color})`;
  return <Card ref={root} width="100%" height="100%" padding={0} variant="default" data-architecture-boundary={data.boundary} data-zone-id={data.zoneId} style={{ outline: data.securityNotes?.length ? "thin dashed var(--color-text-orange)" : undefined, borderWidth: "thin", borderColor: selected ? "var(--color-accent)" : ink, borderStyle: zone ? "dashed" : "solid", background: architectureTint(color, zone ? 22 : 8) }}>
    <VStack gap={0} width="100%" data-architecture-header style={{ background: architectureTint(color, zone ? 70 : 45), borderBlockStart: `var(--spacing-1) solid ${ink}`, borderStartStartRadius: "inherit", borderStartEndRadius: "inherit" }}>
      <VStack ref={content} gap={0} padding={2} data-architecture-title style={{ width: "fit-content", maxWidth: "100%" }}>
      <HStack gap={2} hAlign="between" vAlign="center">
        <HStack gap={1} vAlign="center" style={{ color: ink }}><Icon icon={zone ? "viewColumns" : data.boundary === "public" ? "externalLink" : "viewColumns"} size="sm" /><Text type="supporting" weight="semibold" hasCapsize={false} data-architecture-role-label style={{ color: ink }}>{zone ? "AVAILABILITY ZONE" : data.boundary === "public" ? "PUBLIC SUBNET" : data.boundary === "private" ? "PRIVATE SUBNET" : data.boundary === "routing" ? "ROUTING" : data.boundary === "region" ? "REGION" : data.boundary === "shared" ? data.kind === "Visual category" ? "RESOURCE GROUP" : "SHARED RESOURCES" : data.kind.toUpperCase()}</Text></HStack>
        {data.boundary === "account" && data.cloudId && <CopyValue value={data.cloudId} label="AWS account ID" />}
        {data.cidr && <Text type="supporting" hasCapsize={false} style={{ color: ink }}>{data.cidr}</Text>}
      </HStack>
      <HStack hAlign="between" vAlign="center" gap={2}>
        {data.kind === "Visual category" ? <Text weight="semibold" maxLines={1} hasTruncateTooltip hasCapsize={false}>{data.title}</Text> : <Button tooltip={[data.title, ...data.securityNotes ?? []].join(" · ")} label={`Inspect ${data.title}`} size="sm" variant="ghost" onClick={data.inspect}><Text weight="semibold" maxLines={1} hasTruncateTooltip hasCapsize={false}>{data.title}</Text></Button>}
        {!context && <StatusValue label={data.paused ? `${data.state} · Paused` : data.state} tone={data.tone} />}
      </HStack>
      </VStack>
    </VStack>
    <ResourceHandles />
  </Card>;
}
function ResourceNode({ data, selected }: NodeProps<FlowNode>) {
  const { root, content } = useNodeMeasurement(data);
  const color = architectureColor(data);
  const ink = `var(--color-text-${color})`;
  return <Card ref={root} width="100%" height="100%" padding={2} elevation="low" style={{ outline: data.securityNotes?.length ? "thin dashed var(--color-text-orange)" : undefined, borderColor: selected ? "var(--color-accent)" : "var(--color-border-emphasized)", borderInlineStart: `var(--spacing-1) solid ${ink}`, borderStyle: data.presentation === "external" ? "dashed" : "solid", background: architectureTint(color, 10) }}>
    <ResourceHandles />
    <VStack ref={content} gap={1}>
      <HStack gap={2} vAlign="center" hAlign="between"><HStack gap={1} vAlign="center" style={{ color: ink }}>{["InternetGateway", "NATGateway"].includes(data.kind) && <Icon icon="externalLink" size="sm" />}<Text type="supporting" weight="semibold" hasCapsize={false} style={{ color: ink }}>{friendlyKind(data)}</Text></HStack>{data.presentation !== "external" && <StatusValue label={data.state} tone={data.tone} />}</HStack>
      <Button tooltip={[data.title, ...data.securityNotes ?? []].join(" · ")} label={`Inspect ${data.title}${data.kind === "Route" ? ` (${data.resourceId})` : ""}`} size="sm" variant="ghost" width="100%" onClick={data.inspect}><Text weight="semibold" maxLines={1} hasTruncateTooltip hasCapsize={false}>{data.title}</Text></Button>
      {data.presentation !== "external" && !data.summary?.length && <Text type="supporting" maxLines={2} hasTruncateTooltip hasCapsize={false}>{data.kind === "RouteTable" && data.routes ? `${data.routes.length} route ${data.routes.length === 1 ? "entry" : "entries"}` : resourcePurpose(data)}{data.paused ? " · Paused" : ""}</Text>}
      {data.summary?.map(row => <HStack key={row.label} gap={2} hAlign="between" vAlign="start"><Text type="supporting" hasCapsize={false}>{row.label}</Text><Text type="supporting" hasCapsize={false} maxLines={2} hasTruncateTooltip style={{ minWidth: 0, textAlign: "end" }}>{row.value}</Text></HStack>)}
      {data.summaryNote && <Text type="supporting" hasCapsize={false}>{data.summaryNote}</Text>}
      {data.attachments?.map(attachment => <Button key={attachment.graphId} tooltip={`${attachment.title} · ${attachment.cloudId}`} label={`Inspect ${attachment.title}`} size="sm" variant="secondary" width="100%" onClick={event => { event.stopPropagation(); data.inspectAttachment(attachment.graphId!); }}><Text type="supporting" maxLines={1} hasCapsize={false}>{friendlyKind(attachment)} · {attachment.title}</Text></Button>)}
    </VStack>
  </Card>;
}
export const architectureNodeTypes = { resource: ResourceNode, boundary: BoundaryNode, external: ResourceNode };
type RoutedFlowEdge = Edge<{ route: EdgeRoute }>;
function ArchitectureConnection({ id, data, label, style, markerEnd }: EdgeProps<RoutedFlowEdge>) {
  if (!data?.route) return null;
  const route = data.route;
  return <BaseEdge id={id} path={architectureEdgePath(route.points)} style={style} markerEnd={markerEnd} interactionWidth={0}
    label={label || undefined} labelX={route.label.x} labelY={route.label.y}
    labelStyle={{ fill: "var(--color-text-primary)", fontSize: 12, pointerEvents: "none" }}
    labelBgStyle={{ fill: "var(--color-background-card)", pointerEvents: "none" }} labelBgPadding={[6, 4]} labelBgBorderRadius={4} />;
}
export const architectureEdgeTypes = { architecture: ArchitectureConnection };
const fullscreenUnavailable = () => false;
const subscribeStatic = () => () => {};
const subscribeCompact = (notify: () => void) => { const query = window.matchMedia("(max-width: 48rem)"); query.addEventListener("change", notify); return () => query.removeEventListener("change", notify); };
const compactSnapshot = () => window.matchMedia("(max-width: 48rem)").matches;

// The graph engine owns positioning; its paint follows the active Astryx theme.
export const architectureGraphTheme = {
  "--xy-background-color": "var(--color-background-body)",
  "--xy-edge-stroke": "var(--color-border-emphasized)",
  "--xy-handle-background-color": "var(--color-text-secondary)",
  "--xy-handle-border-color": "var(--color-background-card)",
  "--xy-controls-button-background-color": "var(--color-background-card)",
  "--xy-controls-button-background-color-hover": "var(--color-background-muted)",
  "--xy-controls-button-color": "var(--color-text-primary)",
  "--xy-controls-button-border-color": "var(--color-border)",
  "--xy-minimap-background-color": "var(--color-background-card)",
  "--xy-minimap-mask-background-color": "var(--color-overlay-hover)",
} as CSSProperties;

function relationship(edge: ArchitectureEdge) {
  return edge.data.relationship || (edge.data.relation === "routes_via" ? "associated-with" : edge.data.relation === "uses_address" ? "attached-to" : "routes-to");
}
function ResourceInspector({ selected, selectedId, graph, onInspect, onClose, securityNotes, expanded }: {
  selected: TopologyNodeData; selectedId: string; graph: NetworkArchitectureGraph; onInspect: (id: string) => void; onClose: () => void; securityNotes: string[]; expanded: boolean;
}) {
  const nodes = new Map(graph.nodes.map(node => [node.id, node]));
  const associations = graph.edges.filter(edge => relationship(edge) === "associated-with");
  const targets = graph.edges.filter(edge => edge.data.relation === "forwards_to");
  const tableIds = new Set<string>();
  if (selected.kind === "RouteTable") tableIds.add(selectedId);
  if (selected.kind === "Subnet") for (const edge of associations) if (edge.source === selectedId) tableIds.add(edge.target);
  if (["NATGateway", "InternetGateway"].includes(selected.kind)) for (const edge of targets) if (edge.target === selectedId) tableIds.add(edge.source);
  const tables = [...tableIds].map(id => nodes.get(id)).filter(node => !!node);
  const associatedSubnets = associations.filter(edge => tableIds.has(edge.target)).map(edge => ({ id: edge.id, node: nodes.get(edge.source), evidence: edge.data.evidence }));
  const owners = graph.nodes.filter(node => node.data.attachments?.some(attachment => attachment.graphId === selectedId));
  return <VStack as="aside" role="region" aria-label="Selected topology resource" gap={4} padding={3} style={{ flex: expanded ? "1 1 100%" : "0 1 24rem", minWidth: 0, minHeight: "16rem", maxHeight: "100%", overflow: "auto", borderInlineStart: "thin solid var(--color-border)", background: "var(--color-background-card)" }}>
    <HStack gap={2} vAlign="start" hAlign="between"><VStack gap={1} style={{ minWidth: 0 }}><Text type="supporting" hasCapsize={false}>{friendlyKind(selected)}</Text><Heading level={3} style={{ overflowWrap: "anywhere" }}>{selected.title}</Heading></VStack><Button label="Close resource inspection" icon={<Icon icon="close" />} isIconOnly size="sm" variant="ghost" onClick={onClose} /></HStack>
    <MetadataList>
      <MetadataListItem label="Status"><StatusValue label={selected.state} tone={selected.tone} /></MetadataListItem>
      {selected.cidr && <MetadataListItem label="CIDR">{selected.cidr}</MetadataListItem>}
      {selected.zoneId && <MetadataListItem label="Availability zone">{selected.zoneId}</MetadataListItem>}
      {!selected.referenceOnly && selected.resourceId && <MetadataListItem label="Platform identifier"><CopyValue value={selected.resourceId} label="Topology resource identifier" /></MetadataListItem>}
      {selected.cloudId && <MetadataListItem label={selected.referenceOnly ? "Referenced cloud identifier" : "Cloud identifier"}><CopyValue value={selected.cloudId} label="Topology cloud identifier" /></MetadataListItem>}
      {!selected.referenceOnly && selected.resourceId && <MetadataListItem label="Reconciliation">{selected.paused ? "Paused" : "Not paused"}</MetadataListItem>}
    </MetadataList>
    {securityNotes.map(note => <Text key={note} type="supporting">{note}</Text>)}
    {selected.referenceOnly && <Text type="supporting">Referenced target. Its cloud state has not been independently verified.</Text>}
    {selected.message && <Text type="supporting">{selected.message}</Text>}
    {selected.href && <Link href={selected.href}>Open {friendlyKind(selected).toLowerCase()} details</Link>}
    {owners.map(owner => <Button key={owner.id} label={`Inspect ${owner.data.title}`} size="sm" variant="ghost" onClick={() => onInspect(owner.id)}>Attached to {owner.data.title}</Button>)}
    {tables.map(table => <VStack key={table.id} gap={2}>
      <HStack gap={2} vAlign="center"><Heading level={4}>Route entries</Heading>{table.id !== selectedId && <Button label={`Inspect ${table.data.title}`} size="sm" variant="ghost" onClick={() => onInspect(table.id)}>{table.data.title}</Button>}</HStack>
      {table.data.routes?.length ? <ConsoleTable aria-label={`Routes for ${table.data.title}`} data={table.data.routes.map(route => ({ ...route, id: `${route.id}:${route.destination}:${route.targetId}:${route.evidence}` }))} idKey="id" columns={[
        { key: "destination", header: "Destination" },
        { key: "targetName", header: "Target", renderCell: route => <VStack gap={1}><Text type="supporting">{friendlyKind({ kind: route.targetKind })}</Text>{nodes.has(route.targetId) ? <Button tooltip={route.targetName} label={`Inspect route target ${route.targetName}`} size="sm" variant="ghost" onClick={() => onInspect(route.targetId)}>{route.targetName}</Button> : <Text maxLines={2} hasTruncateTooltip>{route.targetName || "Target not reported"}</Text>}{route.message && <Text type="supporting">{route.message}</Text>}</VStack> },
        { key: "evidence", header: "Reference", renderCell: route => <Text>{route.evidence === "observed" ? "Observed" : "Configured"}</Text> },
      ]} /> : <Text type="supporting">No route entries reported for this table.</Text>}
    </VStack>)}
    {associatedSubnets.length > 0 && <VStack gap={2}><Heading level={4}>Associated subnets</Heading><ConsoleTable aria-label="Route table subnet associations" data={associatedSubnets} idKey="id" columns={[
      { key: "node", header: "Subnet", renderCell: row => row.node ? <Button tooltip={row.node.data.title} label={`Inspect ${row.node.data.title}`} size="sm" variant="ghost" onClick={() => onInspect(row.node!.id)}>{row.node.data.title}</Button> : <Text>Subnet not reported</Text> },
      { key: "evidence", header: "Reference", renderCell: row => <Text>{row.evidence === "observed" ? "Observed" : "Configured"}</Text> },
    ]} /></VStack>}
    {selected.kind === "Subnet" && !tables.length && <Text type="supporting">No route table association has been reported for this subnet.</Text>}
  </VStack>;
}

export default function NetworkTopology({ network, account, onRefresh }: { network: Network; account?: CloudAccount; onRefresh: () => void }) {
  const { api, tenantId } = usePlatform();
  const graphElement = useRef<HTMLDivElement>(null);
  const flow = useRef<ReactFlowInstance<FlowNode> | null>(null);
  const [fullscreenError, setFullscreenError] = useState("");
  const subscribeFullscreen = useCallback((notify: () => void) => {
    const escape = (event: KeyboardEvent) => { if (event.key === "Escape" && document.fullscreenElement === graphElement.current) void document.exitFullscreen().catch(() => undefined); };
    document.addEventListener("fullscreenchange", notify);
    document.addEventListener("keydown", escape);
    return () => { document.removeEventListener("fullscreenchange", notify); document.removeEventListener("keydown", escape); };
  }, []);
  const fullscreen = useSyncExternalStore(subscribeFullscreen, () => !!graphElement.current && document.fullscreenElement === graphElement.current, fullscreenUnavailable);
  const fullscreenAvailable = useSyncExternalStore(subscribeStatic, () => document.fullscreenEnabled, fullscreenUnavailable);
  const toggleFullscreen = async () => {
    setFullscreenError("");
    try {
      if (document.fullscreenElement === graphElement.current) await document.exitFullscreen();
      else await graphElement.current?.requestFullscreen();
    } catch { setFullscreenError("Fullscreen could not open. You can still pan, zoom and fit the diagram."); }
  };

  const [selectedId, setSelectedId] = useState("");
  const [securityOverlay, setSecurityOverlay] = useState(false);
  const compact = useSyncExternalStore(subscribeCompact, compactSnapshot, fullscreenUnavailable);
  const [measurements, setMeasurements] = useState<ArchitectureDimensions>(new Map());
  const reportSize = useCallback((id: string, size: NodeSize) => {
    setMeasurements(previous => {
      const old = previous.get(id);
      if (old && old.width >= size.width && old.height >= size.height && old.headerWidth === size.headerWidth && (old.headerHeight ?? 0) >= (size.headerHeight ?? 0)) return previous;
      const next = new Map(previous);
      next.set(id, { width: Math.max(old?.width ?? 0, size.width), height: Math.max(old?.height ?? 0, size.height), headerHeight: Math.max(old?.headerHeight ?? 0, size.headerHeight ?? 0), headerWidth: size.headerWidth });
      return next;
    });
  }, []);
  const loadNetwork = useCallback(async (signal: AbortSignal) => {
    const response = await api.network.getNetwork({ tenantId, networkId: network.id, live: true }, { signal });
    if (!response.network || response.network.id !== network.id || response.network.tenantId !== tenantId) throw new Error("The architecture response did not match this network and tenant.");
    return response.network;
  }, [api, tenantId, network.id]);
  const liveNetwork = useAsyncQuery(loadNetwork, { pollMs: 5000 });
  const architectureNetwork = liveNetwork.data ?? network;
  const scopedAccount = account && account.tenantId === tenantId && account.tenantId === architectureNetwork.tenantId && account.id === architectureNetwork.spec?.bootstrapId ? account : undefined;
  const load = useCallback((signal: AbortSignal) => collectNetworkClusters(network.id, pageToken => api.cluster.listClusters({ tenantId, pageSize: 200, pageToken }, { signal }), signal), [api, tenantId, network.id]);
  const clusters = useAsyncQuery(load);
  const graph = useMemo(() => buildNetworkArchitectureOverview(architectureNetwork, scopedAccount, clusters.data ?? [], measurements), [architectureNetwork, scopedAccount, clusters.data, measurements]);
  const inventory = [...graph.nodes.map(node => ({ ...node.data, graphId: node.id })), ...graph.supportingResources].map(data => ({ ...data, id: data.graphId || `${data.kind}:${data.resourceId || data.cloudId || data.title}` }));
  const securityFindings = useMemo(() => inspectArchitectureSecurity(graph), [graph]);
  const securityCoverage = useMemo(() => architectureSecurityCoverage(graph), [graph]);
  const selected = inventory.find(row => row.id === selectedId);
  const selectedNode = graph.nodes.find(n => n.id === selectedId) ?? graph.nodes.find(n => n.data.attachments?.some(a => a.graphId === selectedId));
  const tracedEdges = traceArchitecture(graph, selectedNode?.id ?? "");
  const tracedNodes = new Set(graph.edges.filter(e => tracedEdges.has(e.id)).flatMap(e => [e.source, e.target]));
  // Controlled graph updates must retain measured dimensions: otherwise React Flow
  // clears handle bounds and may hide nodes/edges until another resize occurs.
  const nodes: FlowNode[] = graph.nodes.map(({ width, height, ...node }) => ({ ...node, type: node.type === "group" ? "boundary" : node.type, selected: node.id === selectedNode?.id || tracedNodes.has(node.id),
    width, height, measured: { width, height }, style: { width, height }, ariaLabel: `${friendlyKind(node.data)}: ${node.data.title}, ${node.data.state}${node.data.paused ? ", paused" : ""}`,
    data: { ...node.data, securityNotes: securityOverlay ? securityFindings.filter(f => f.nodeId === node.id).map(f => f.title) : [], nodeId: node.id, inspect: () => setSelectedId(node.id), inspectAttachment: setSelectedId, reportSize } }));
  const routing = useMemo(() => {
    try {
      const context = typeof document === "undefined" ? null : document.createElement("canvas").getContext("2d");
      if (context) context.font = `12px ${getComputedStyle(document.body).fontFamily}`;
      const edges: Edge[] = routeArchitectureEdges(graph.nodes, graph.edges, { measureLabel: text => ({ width: Math.ceil(context?.measureText(text).width ?? text.length * 8) + 16, height: 24 }) }).map(edge => {
        const association = edge.data.relationship === "associated-with" || edge.data.relationship === "attached-to";
        const logical = edge.data.derived;
        const color = association ? "var(--color-text-disabled)" : edge.data.crossZone ? "var(--color-text-orange)" : "var(--color-text-secondary)";
        return { ...edge, type: "architecture", interactionWidth: 0,
          sourceHandle: `out-${edge.data.route.sourceSide}`, targetHandle: `in-${edge.data.route.targetSide}`,
          markerEnd: association ? undefined : { type: MarkerType.ArrowClosed, color },
          style: { stroke: color, strokeWidth: association ? 1 : 1.6, pointerEvents: "none", strokeDasharray: association ? "5 4" : logical ? "2 4" : undefined },
          zIndex: 1, ariaLabel: `${edge.label || edge.data.relationship}, ${edge.data.evidence}${logical ? " logical path" : " reference"}` };
      });
      return { edges, error: "" };
    } catch (error) {
      return { edges: [] as Edge[], error: error instanceof ArchitectureRoutingError ? `A connection could not be placed safely (${error.reason}). Resource and route details remain available in the inspector.` : "Routing could not be calculated. Resource details remain available." };
    }
  }, [graph]);
  const styledEdges = routing.edges.filter(edge => graph.edges.find(e => e.id === edge.id)?.data.relation !== "nat_internet_egress" || tracedEdges.has(edge.id)).map(edge => ({ ...edge, markerEnd: tracedEdges.has(edge.id) && edge.markerEnd ? { type: MarkerType.ArrowClosed, color: "var(--color-text-blue)" } : edge.markerEnd, style: { ...edge.style, ...(selectedNode ? { opacity: tracedEdges.has(edge.id) ? 1 : 0.15, stroke: tracedEdges.has(edge.id) ? "var(--color-text-blue)" : edge.style?.stroke, strokeWidth: tracedEdges.has(edge.id) ? 2.5 : edge.style?.strokeWidth } : {}) } }));
  const provider = networkProvider(scopedAccount);
  const fit = async () => { await new Promise<void>(resolve => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))); await flow.current?.fitView({ padding: "8%", duration: 0, maxZoom: 1 }); };
  return <VStack gap={3}>
    {liveNetwork.error && <Banner status="warning" title="Live architecture unavailable" description={`Previously reported resources remain visible. ${liveNetwork.error}`} />}
    {clusters.error && <Banner status="warning" title="Cluster topology unavailable" description={`Network resources remain visible. ${clusters.error}`} />}
    {fullscreenError && <Banner status="warning" title="Fullscreen unavailable" description={fullscreenError} />}
    {routing.error && <Banner status="warning" title="Routing unavailable" description={routing.error} />}
    <Card ref={graphElement} role="region" aria-label="Network topology diagram" padding={0} height={fullscreen ? "100dvh" : "min(80dvh, 56rem)"} minHeight="28rem" width="100%" style={{ position: "relative", display: "flex", flexDirection: "column", ...(fullscreen ? { borderRadius: 0 } : {}) }}>
      <HStack padding={3} gap={3} wrap="wrap" hAlign="between" vAlign="center" style={{ flex: "0 0 auto", borderBlockEnd: "thin solid var(--color-border)" }}>
        <VStack gap={2} style={{ minWidth: 0 }}>
          <HStack gap={2} vAlign="center"><ProviderIcon provider={provider || ""} size="sm" /><Heading level={2}>Network architecture</Heading></HStack>
          <HStack gap={2} wrap="wrap" vAlign="center">
            {scopedAccount && <Link tooltip={scopedAccount.accountName || scopedAccount.id} href={`/accounts/${encodeURIComponent(scopedAccount.id)}`}>{scopedAccount.accountName || scopedAccount.id}</Link>}
            <Text type="supporting">{network.spec?.region || "Region not reported"} · {graph.total} resources{liveNetwork.loading ? " · Loading…" : ""}</Text>
            {inventory.some(n => n.kind === "Cluster") && <HStack gap={2} wrap="wrap" vAlign="center" aria-label="Associated clusters"><Text type="supporting">· Clusters</Text>{inventory.filter(n => n.kind === "Cluster").map(n => <Link key={n.id} tooltip={n.title} href={n.href}>{n.title}</Link>)}</HStack>}
          </HStack>
        </VStack>
        <RefreshButton label="Refresh topology clusters" onClick={() => { liveNetwork.refresh(); clusters.refresh(); onRefresh(); }} isLoading={clusters.refreshing || liveNetwork.refreshing} />
      </HStack>
      <HStack padding={2} gap={3} wrap="wrap" hAlign="between" vAlign="center" role="group" aria-label="Diagram controls" style={{ flex: "0 0 auto", borderBlockEnd: "thin solid var(--color-border)" }}>
        <HStack gap={3} wrap="wrap" vAlign="center" aria-label="Architecture reference legend" role="group">
          <HStack gap={1} vAlign="center"><HStack width="1.5rem" style={{ borderBlockStart: "thin dashed var(--color-text-disabled)" }} /><Text type="supporting" hasCapsize={false}>Association</Text></HStack>
          <HStack gap={1} vAlign="center"><HStack width="1.5rem" style={{ borderBlockStart: "thin solid var(--color-text-secondary)" }} /><Text type="supporting" hasCapsize={false}>Route</Text></HStack>
          <HStack gap={1} vAlign="center"><HStack width="1.5rem" style={{ borderBlockStart: "thin dotted var(--color-text-secondary)" }} /><Text type="supporting" hasCapsize={false}>Egress</Text></HStack>
          {graph.edges.some(e => e.data.crossZone) && <Text type="supporting" hasCapsize={false} style={{ color: "var(--color-text-orange)" }}>Orange: cross-AZ</Text>}
        </HStack>
        <HStack gap={2} wrap="wrap" vAlign="center">
          <CheckboxInput label="Security overlay" size="sm" value={securityOverlay} onChange={setSecurityOverlay} />
          <Button label="Zoom out" size="sm" variant="ghost" onClick={() => { void flow.current?.zoomOut({ duration: 0 }); }}><Text hasCapsize={false}>−</Text></Button>
          <Button label="Zoom in" size="sm" variant="ghost" onClick={() => { void flow.current?.zoomIn({ duration: 0 }); }}><Text hasCapsize={false}>+</Text></Button>
          <Button label="Fit view" size="sm" variant="ghost" onClick={fit} />
          {fullscreenAvailable && <Button label={fullscreen ? "Exit fullscreen" : "Enter fullscreen"} size="sm" variant="ghost" clickAction={toggleFullscreen} />}
        </HStack>
      </HStack>
      <HStack gap={0} width="100%" vAlign="stretch" wrap="wrap" style={{ flex: "1 1 0%", minHeight: 0, overflow: "auto" }}>
        <VStack style={{ display: compact && (selected || securityOverlay) ? "none" : "flex", flex: "1 1 24rem", minWidth: 0, minHeight: "20rem", height: "100%" }}>
          <ReactFlow<FlowNode> aria-label="Network topology canvas" nodes={nodes} edges={styledEdges} nodeTypes={architectureNodeTypes} edgeTypes={architectureEdgeTypes}
            onInit={instance => { flow.current = instance; }} onNodeClick={(_, node) => setSelectedId(node.id)} onPaneClick={() => setSelectedId("")}
            nodesDraggable={false} nodesConnectable={false} edgesReconnectable={false} deleteKeyCode={null}
            nodesFocusable edgesFocusable fitView fitViewOptions={{ padding: "8%", minZoom: 0.6, maxZoom: 0.9 }} minZoom={0.04} maxZoom={2}
            zoomOnScroll={false} zoomOnDoubleClick={false} preventScrolling={false} style={architectureGraphTheme}
            ariaLabelConfig={{ "node.a11yDescription.default": "Use the Inspect button to view this resource. The diagram is read-only.", "edge.a11yDescription.default": "Recorded relationship, not a reachability check." }}>
            <Background color="var(--color-border)" />
          </ReactFlow>
        </VStack>
        {securityOverlay && !selected && <VStack as="aside" role="region" aria-label="Network security observations" gap={3} padding={3} style={{ flex: compact ? "1 1 100%" : "0 1 24rem", minWidth: 0, maxHeight: "100%", overflow: "auto", borderInlineStart: "thin solid var(--color-border)" }}>
          <Heading level={3}>Security observations</Heading>
          <Text type="supporting">{securityCoverage.message} An Internet Gateway route alone does not establish public exposure.</Text>
          {securityFindings.map(finding => <VStack key={finding.id} gap={1} padding={2} style={{ borderBlockEnd: "thin solid var(--color-border)" }}>
            <Button label={`Inspect ${finding.title}`} variant="ghost" size="sm" onClick={() => setSelectedId(finding.nodeId)}>{finding.title}</Button>
            <Text type="supporting">{finding.detail}</Text>
          </VStack>)}
          {!securityFindings.length && <Text type="supporting">No configuration observations are available in this snapshot.</Text>}
        </VStack>}
        {selected && <ResourceInspector expanded={compact} selected={selected} selectedId={selectedId} graph={graph} onInspect={setSelectedId} onClose={() => setSelectedId("")} securityNotes={securityOverlay ? securityFindings.filter(f => f.nodeId === selectedId).map(f => `${f.title}: ${f.detail}`) : []} />}
      </HStack>
    </Card>
    {graph.unresolved.length > 0 && <Banner status="warning" title="Architecture references incomplete" description={graph.unresolved.join(" ")} />}
    <Text type="supporting">Select a resource to inspect its routes and connections. Drag to pan; use + / − to zoom.</Text>

  </VStack>;
}

/** Selecting a target includes its upstream users without tracing unrelated
 * outgoing routes from those users. Downstream paths retain all destinations. */
export function traceArchitecture(graph: Pick<NetworkArchitectureGraph, "nodes" | "edges">, selected: string): Set<string> {
  const result = new Set<string>(), seen = new Set<string>(), pending = [selected];
  const kind = graph.nodes.find(n => n.id === selected)?.data.kind;
  if (kind === "RouteTable" || kind === "NATGateway") {
    const incoming = graph.edges.filter(e => e.target === selected && (e.data.relation === "routes_via" || e.data.relation === "forwards_to"));
    for (const edge of incoming) {
      result.add(edge.id);
      if (kind === "NATGateway") for (const association of graph.edges.filter(e => e.target === edge.source && e.data.relation === "routes_via")) result.add(association.id);
    }
  }
  while (pending.length) {
    const source = pending.pop()!;
    if (!source || seen.has(source)) continue;
    seen.add(source);
    for (const edge of graph.edges.filter(e => e.source === source)) { result.add(edge.id); pending.push(edge.target); }
  }
  return result;
}
