import type { ArchitectureDimensions, ArchitectureNode, NetworkArchitectureGraph } from "./network-topology";

const compare = (a: ArchitectureNode, b: ArchitectureNode) => a.id.localeCompare(b.id);
const roleRank = (node: ArchitectureNode) => node.data.boundary === "public" ? 0 : node.data.boundary === "private" ? 1 : 2;
/** Hierarchical packing with connection corridors. Every box is sized bottom-up
 * from its measured children. Counts never select a special topology template.
 * AZ columns preserve AWS containment; table ordering follows association
 * barycentres to reduce crossings without moving gateways to another AZ.
 *
 * ELK layered was evaluated: it supports compound graphs and fixed ports, but
 * unrestricted layering moves AWS groups according to routing cycles (private
 * table -> NAT in a public subnet -> public table). We retain semantic bands
 * and use the independent obstacle router for those cross-hierarchy edges.
 */
export function layoutNetworkArchitecture(graph: NetworkArchitectureGraph, dimensions: ArchitectureDimensions = new Map()): NetworkArchitectureGraph {
  const nodes = graph.nodes.map(n => ({ ...n, position: { ...n.position }, data: { ...n.data } }));
  const byId = new Map(nodes.map(n => [n.id, n]));
  const degree = new Map<string, number>();
  for (const edge of graph.edges) for (const id of [edge.source, edge.target]) degree.set(id, (degree.get(id) ?? 0) + 1);
  const padding = 24;
  // Corridors grow with the number of distinct connections using the boundary.
  const gutter = (items: ArchitectureNode[]) => 32 + Math.max(0, ...items.map(n => degree.get(n.id) ?? 0)) * 6;
  const children = (parent: ArchitectureNode) => nodes.filter(n => n.parentId === parent.id).sort(compare);
  for (const node of nodes) {
    const measured = dimensions.get(node.id);
    node.width = Math.max(288, measured?.width ?? 0);
    node.height = Math.max(node.type === "group" ? 112 : node.type === "external" ? 88 : 120 + (node.data.attachments?.length ?? 0) * 36, measured?.height ?? 0);
    if (node.type === "group") { node.headerHeight = Math.max(32, measured?.headerHeight ?? 96); node.headerWidth = measured?.headerWidth; }
  }
  const column = (parent: ArchitectureNode, items: ArchitectureNode[]) => {
    const gap = gutter(items);
    let y = (parent.headerHeight ?? 0) + padding;
    for (const child of items) { child.position = { x: padding, y }; y += child.height + gap; }
    parent.width = Math.max(parent.width, ...items.map(n => n.width + padding * 2));
    parent.height = Math.max(parent.headerHeight! + padding * 2, y - (items.length ? gap : 0) + padding);
  };
  const row = (items: ArchitectureNode[], x: number, y: number) => {
    const gap = gutter(items); let right = x;
    for (const child of items) { child.position = { x: right, y }; right += child.width + gap; }
    return { right: right - (items.length ? gap : 0), bottom: y + Math.max(0, ...items.map(n => n.height)) };
  };
  const laidOut = new Set<string>();
  const arrange = (node: ArchitectureNode) => {
    if (laidOut.has(node.id)) return;
    laidOut.add(node.id);
    const items = children(node);
    for (const child of items) arrange(child);
    if (node.type !== "group") return;
    if (node.data.boundary === "zone") {
      column(node, items.sort((a, b) => roleRank(a) - roleRank(b) || compare(a, b))); return;
    }
    if (node.data.kind === "Subnet") { column(node, items); return; }
    if (node.data.boundary === "vpc") {
      const zones = items.filter(n => n.data.boundary === "zone");
      const tables = items.filter(n => n.data.kind === "RouteTable");
      const direct = items.filter(n => n.data.boundary !== "zone" && n.data.kind !== "RouteTable");
      const gap = gutter(items);
      const zoneOf = (id: string): ArchitectureNode | undefined => {
        let current = byId.get(id);
        while (current && current.parentId !== node.id) current = byId.get(current.parentId ?? "");
        return current?.data.boundary === "zone" ? current : undefined;
      };
      const associations = (table: ArchitectureNode) => graph.edges.filter(e => e.target === table.id && e.data.relation === "routes_via").map(e => byId.get(e.source)).filter((n): n is ArchitectureNode => !!n);
      const lanes = new Map<string, ArchitectureNode[]>();
      const unassociated: ArchitectureNode[] = [];
      for (const table of tables) {
        const candidates = associations(table).map(n => zoneOf(n.id)).filter((n): n is ArchitectureNode => !!n).sort(compare);
        // The median association places a shared table once, close to its users.
        const zone = candidates[Math.floor(candidates.length / 2)];
        if (zone) lanes.set(zone.id, [...lanes.get(zone.id) ?? [], table]); else unassociated.push(table);
      }
      const sharedRow = row([...direct, ...unassociated], padding, node.headerHeight! + padding);
      const top = sharedRow.bottom + (direct.length || unassociated.length ? gap : 0);
      let x = padding, bottom = top;
      for (const zone of zones) {
        zone.position = { x, y: top };
        const lane = lanes.get(zone.id) ?? [];
        const desiredY = (table: ArchitectureNode) => {
          const related = associations(table).filter(n => zoneOf(n.id)?.id === zone.id);
          return related.length ? related.reduce((sum, n) => sum + n.position.y + n.height / 2, 0) / related.length + top - table.height / 2 : top;
        };
        lane.sort((a, b) => desiredY(a) - desiredY(b) || compare(a, b));
        let end = top;
        for (const table of lane) {
          table.position = { x: x + zone.width + gap, y: Math.max(desiredY(table), end) };
          end = table.position.y + table.height + gutter(lane);
        }
        const laneWidth = Math.max(0, ...lane.map(t => t.width));
        x += zone.width + gap + (lane.length ? laneWidth + gap : 0);
        bottom = Math.max(bottom, top + zone.height, end - (lane.length ? gutter(lane) : 0));
      }
      node.width = Math.max(node.width, sharedRow.right + padding, x - (zones.length ? gap : 0) + padding);
      node.height = bottom + padding;
      // IGWs stay near the VPC boundary, centrally aligned with their Internet
      // context rather than collecting every path at the left edge.
      const shared = [...direct, ...unassociated];
      const occupied = shared.reduce((sum, child) => sum + child.width, 0);
      const interval = Math.max(0, (node.width - padding * 2 - occupied) / (shared.length + 1));
      let sharedX = padding + interval;
      for (const child of shared) { child.position.x = sharedX; sharedX += child.width + interval; }
      return;
    }
    if (node.data.boundary === "region") {
      const vpcs = items.filter(n => n.data.kind === "VPC");
      const external = items.filter(n => n.data.kind !== "VPC");
      const externalRow = row(external, padding, node.headerHeight! + padding);
      let y = externalRow.bottom + gutter(external);
      for (const vpc of vpcs) { vpc.position = { x: padding, y }; y += vpc.height + gutter(vpcs); }
      node.width = Math.max(node.width, externalRow.right + padding, ...vpcs.map(n => n.width + padding * 2));
      node.height = y - (vpcs.length ? gutter(vpcs) : 0) + padding;
      if (external.length === 1) external[0].position.x = (node.width - external[0].width) / 2;
      return;
    }
    column(node, items);
  };
  const roots = nodes.filter(n => !n.parentId).sort((a, b) => Number(b.data.boundary === "region") - Number(a.data.boundary === "region") || compare(a, b));
  let y = 0;
  for (const root of roots) { arrange(root); root.position = { x: 0, y }; y += root.height + gutter(roots); }
  const ordered: ArchitectureNode[] = [];
  const visit = (node: ArchitectureNode) => { ordered.push(node); for (const child of children(node)) visit(child); };
  for (const root of roots) visit(root);
  return { ...graph, nodes: ordered };
}
