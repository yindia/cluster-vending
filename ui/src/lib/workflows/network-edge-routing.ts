import type { ArchitectureEdge, ArchitectureNode } from "./network-topology";

export type Point = { x: number; y: number };
export type Side = "top" | "right" | "bottom" | "left";
export type Obstacle = { x: number; y: number; width: number; height: number; nodeId: string };
export type EdgeRoute = { sourceAnchor: Point; targetAnchor: Point; points: Point[]; sourceSide: Side; targetSide: Side; label: { x: number; y: number; width: number; height: number } };
export type RoutedArchitectureEdge = ArchitectureEdge & { data: ArchitectureEdge["data"] & { route: EdgeRoute } };
export type RoutingOptions = {
  /** Return the complete rendered label box (including padding), in diagram units. */
  measureLabel?: (text: string, edge: ArchitectureEdge) => { width: number; height: number };
  /** Called once per candidate path checked and per grid cell expanded: a
   * deterministic measure of routing work, independent of machine speed. */
  onSearchStep?: () => void;
};
const noStep = () => {};
export class ArchitectureRoutingError extends Error {
  constructor(public readonly edgeId: string, public readonly reason: string) {
    super(`Cannot route edge ${edgeId}: ${reason}`);
    this.name = "ArchitectureRoutingError";
  }
}
const gap = 16;
// Prefer right-facing ports when geometric costs tie, without encoding topology roles.
const sides: Side[] = ["right", "bottom", "left", "top"];
const distance = (a: Point, b: Point) => Math.abs(a.x - b.x) + Math.abs(a.y - b.y);
const inflate = (r: Obstacle, padding: number): Obstacle => ({ ...r, x: r.x - padding, y: r.y - padding, width: r.width + 2 * padding, height: r.height + 2 * padding });
const contains = (r: Obstacle, p: Point) => p.x > r.x && p.x < r.x + r.width && p.y > r.y && p.y < r.y + r.height;
export function absoluteArchitectureNodes(nodes: readonly ArchitectureNode[]): Map<string, Obstacle> {
  const byId = new Map(nodes.map(node => [node.id, node]));
  if (byId.size !== nodes.length) throw new ArchitectureRoutingError("<graph>", "duplicate node IDs");
  const result = new Map<string, Obstacle>();
  for (const node of nodes) {
    let { x, y } = node.position;
    if (![x, y, node.width, node.height].every(Number.isFinite) || node.width <= 0 || node.height <= 0) throw new ArchitectureRoutingError("<graph>", `invalid geometry for ${node.id}`);
    if (node.parentId && !byId.has(node.parentId)) throw new ArchitectureRoutingError("<graph>", `missing parent for ${node.id}`);
    let parent = node.parentId ? byId.get(node.parentId) : undefined;
    const seen = new Set([node.id]);
    while (parent) {
      if (seen.has(parent.id)) throw new ArchitectureRoutingError("<graph>", `parent cycle at ${node.id}`);
      seen.add(parent.id); x += parent.position.x; y += parent.position.y;
      parent = parent.parentId ? byId.get(parent.parentId) : undefined;
    }
    result.set(node.id, { x, y, width: node.width, height: node.height, nodeId: node.id });
  }
  return result;
}
/** With an edge, only its endpoint groups and ancestors are permeable.
 * Without an edge, returns the shared card/header obstacles for diagnostics. */
export function architectureObstacles(nodes: readonly ArchitectureNode[], edge?: ArchitectureEdge): Obstacle[] {
  const boxes = absoluteArchitectureNodes(nodes);
  const byId = new Map(nodes.map(node => [node.id, node]));
  const permeable = new Set<string>();
  if (edge) for (const id of [edge.source, edge.target]) {
    let current = byId.get(id);
    while (current) { permeable.add(current.id); current = byId.get(current.parentId ?? ""); }
  }
  return nodes.map(node => ({ ...boxes.get(node.id)!, width: node.type === "group" && (!edge || permeable.has(node.id)) ? Math.min(node.headerWidth ?? node.width, node.width) : node.width, height: node.type === "group" && (!edge || permeable.has(node.id)) ? Math.min(node.headerHeight ?? 76, node.height) : node.height }));
}
export function segmentHitsRectangle(a: Point, b: Point, r: Obstacle): boolean {
  if (a.x === b.x) return a.x > r.x && a.x < r.x + r.width && Math.max(a.y, b.y) > r.y && Math.min(a.y, b.y) < r.y + r.height;
  return a.y > r.y && a.y < r.y + r.height && Math.max(a.x, b.x) > r.x && Math.min(a.x, b.x) < r.x + r.width;
}
function port(rect: Obstacle, side: Side, offset: number, stubLength = gap): { anchor: Point; stub: Point } {
  const horizontal = side === "top" || side === "bottom";
  const displacement = ((horizontal ? rect.width : rect.height) - Math.min(40, (horizontal ? rect.width : rect.height) / 2)) * (offset - 0.5);
  const anchor = { x: horizontal ? rect.x + rect.width / 2 + displacement : side === "left" ? rect.x : rect.x + rect.width, y: horizontal ? side === "top" ? rect.y : rect.y + rect.height : rect.y + rect.height / 2 + displacement };
  return { anchor, stub: { x: anchor.x + (side === "left" ? -stubLength : side === "right" ? stubLength : 0), y: anchor.y + (side === "top" ? -stubLength : side === "bottom" ? stubLength : 0) } };
}
type QueueItem = { key: number; cost: number; priority: number };
class Heap {
  items: QueueItem[] = [];
  push(item: QueueItem) {
    let index = this.items.length; this.items.push(item);
    while (index) { const parent = (index - 1) >> 1; if (this.items[parent].priority <= item.priority) break; this.items[index] = this.items[parent]; index = parent; }
    this.items[index] = item;
  }
  pop(): QueueItem | undefined {
    const first = this.items[0], last = this.items.pop();
    if (!this.items.length || !last) return first;
    let index = 0;
    while (index * 2 + 1 < this.items.length) {
      let child = index * 2 + 1;
      if (child + 1 < this.items.length && this.items[child + 1].priority < this.items[child].priority) child++;
      if (this.items[child].priority >= last.priority) break;
      this.items[index] = this.items[child]; index = child;
    }
    this.items[index] = last; return first;
  }
}
function simplify(points: Point[]): Point[] {
  const result: Point[] = [];
  for (const point of points) {
    if (result.length && distance(point, result.at(-1)!) === 0) continue;
    while (result.length > 1) {
      const a = result.at(-2)!, b = result.at(-1)!;
      if (!(a.x === b.x && b.x === point.x || a.y === b.y && b.y === point.y) || distance(a, point) < distance(a, b) + distance(b, point)) break;
      result.pop();
    }
    result.push(point);
  }
  return result;
}
type Segment = { a: Point; b: Point };
/** Soft penalties permit necessary crossings while strongly discouraging shared
 * trunks. End contacts count half a crossing so grid subdivision cannot hide it. */
function congestion(a: Point, b: Point, segments: Segment[]): number {
  let cost = 0;
  const vertical = a.x === b.x;
  for (const { a: p, b: q } of segments) {
    const otherVertical = p.x === q.x;
    if (vertical === otherVertical) {
      if (vertical ? a.x !== p.x : a.y !== p.y) continue;
      const length = vertical
        ? Math.min(Math.max(a.y, b.y), Math.max(p.y, q.y)) - Math.max(Math.min(a.y, b.y), Math.min(p.y, q.y))
        : Math.min(Math.max(a.x, b.x), Math.max(p.x, q.x)) - Math.max(Math.min(a.x, b.x), Math.min(p.x, q.x));
      if (length > 0) cost += 400 + length * 4;
    } else {
      const v = vertical ? a : p, endV = vertical ? b : q, h = vertical ? p : a, endH = vertical ? q : b;
      if (v.x >= Math.min(h.x, endH.x) && v.x <= Math.max(h.x, endH.x) && h.y >= Math.min(v.y, endV.y) && h.y <= Math.max(v.y, endV.y)) {
        const interior = v.x > Math.min(h.x, endH.x) && v.x < Math.max(h.x, endH.x) && h.y > Math.min(v.y, endV.y) && h.y < Math.max(v.y, endV.y);
        cost += interior ? 480 : 240;
      }
    }
  }
  return cost;
}
function search(start: Point, end: Point, obstacles: Obstacle[], used: Point[][], quickOnly = false, step: () => void = noStep): { points: Point[]; cost: number } | undefined {
  if (obstacles.some(r => contains(r, start) || contains(r, end))) return;
  const segments = used.flatMap(points => points.slice(1).map((b, i) => ({ a: points[i], b })));
  // At most twelve additional coordinates per axis, independent of edge count.
  const lanes = (vertical: boolean) => [...new Set(segments.filter(({ a, b }) => (a.x === b.x) === vertical).map(({ a }) => vertical ? a.x : a.y))]
    .sort((a, b) => Math.abs(a - (vertical ? start.x + end.x : start.y + end.y) / 2) - Math.abs(b - (vertical ? start.x + end.x : start.y + end.y) / 2) || a - b)
    .slice(0, 6).flatMap(value => [value - 12, value + 12]);
  const laneX = lanes(true), laneY = lanes(false);
  const clearPath = (points: Point[]) => { step(); return points.slice(1).every((p, i) => !obstacles.some(r => segmentHitsRectangle(points[i], p, r))); };
  const quickPaths: Point[][] = [
    [start, { x: start.x, y: end.y }, end],
    [start, { x: end.x, y: start.y }, end],
  ];
  for (const r of obstacles) {
    for (const x of [r.x, r.x + r.width]) quickPaths.push([start, { x, y: start.y }, { x, y: end.y }, end]);
    for (const y of [r.y, r.y + r.height]) quickPaths.push([start, { x: start.x, y }, { x: end.x, y }, end]);
  }
  for (const x of laneX) quickPaths.push([start, { x, y: start.y }, { x, y: end.y }, end]);
  for (const y of laneY) quickPaths.push([start, { x: start.x, y }, { x: end.x, y }, end]);
  const unique = new Map(quickPaths.map(points => { const path = simplify(points); return [JSON.stringify(path), path] as const; }));
  const candidates = [...unique.entries()].filter(([, points]) => clearPath(points)).map(([key, points]) => ({ key, points,
    cost: points.slice(1).reduce((sum, p, i) => sum + distance(points[i], p) + congestion(points[i], p, segments), 0) + Math.max(0, points.length - 2) * 24 }))
    .sort((a, b) => a.cost - b.cost || a.key.localeCompare(b.key));
  if (candidates.length) return candidates[0];
  if (quickOnly) return;
  const xs = new Set([start.x, end.x, ...laneX]), ys = new Set([start.y, end.y, ...laneY]);
  for (const r of obstacles) { xs.add(r.x); xs.add(r.x + r.width); ys.add(r.y); ys.add(r.y + r.height); }
  // Obstacle boundaries and dedicated endpoint coordinates are sufficient for
  // rectilinear connectivity. Prior bends must not multiply the Cartesian grid.

  const x = [...xs].sort((a, b) => a - b), y = [...ys].sort((a, b) => a - b);
  x.unshift(x[0] - 64); x.push(x.at(-1)! + 64); y.unshift(y[0] - 64); y.push(y.at(-1)! + 64);
  const nx = x.length, startCell = y.indexOf(start.y) * nx + x.indexOf(start.x), endCell = y.indexOf(end.y) * nx + x.indexOf(end.x);
  const point = (cell: number) => ({ x: x[cell % nx], y: y[Math.floor(cell / nx)] });
  const queue = new Heap(), costs = new Map<number, number>(), previous = new Map<number, number>();
  const initial = startCell * 3; costs.set(initial, 0); queue.push({ key: initial, cost: 0, priority: distance(start, end) });
  const clear = new Map<string, boolean>(), penalties = new Map<string, number>();
  while (queue.items.length) {
    const current = queue.pop()!;
    if (costs.get(current.key) !== current.cost) continue;
    step();
    const cell = Math.floor(current.key / 3), axis = current.key % 3;
    if (cell === endCell) {
      const points: Point[] = []; let key: number | undefined = current.key;
      while (key !== undefined) { points.push(point(Math.floor(key / 3))); key = previous.get(key); }
      return { points: simplify(points.reverse()), cost: current.cost };
    }
    const cx = cell % nx, cy = Math.floor(cell / nx), a = point(cell);
    for (const [dx, dy, nextAxis] of [[1, 0, 1], [-1, 0, 1], [0, 1, 2], [0, -1, 2]]) {
      if (cx + dx < 0 || cx + dx >= nx || cy + dy < 0 || cy + dy >= y.length) continue;
      const nextCell = (cy + dy) * nx + cx + dx, b = point(nextCell), cacheKey = `${Math.min(cell, nextCell)}:${Math.max(cell, nextCell)}`;
      let open = clear.get(cacheKey);
      if (open === undefined) { open = !obstacles.some(r => segmentHitsRectangle(a, b, r)); clear.set(cacheKey, open); }
      if (!open) continue;
      let penalty = penalties.get(cacheKey);
      if (penalty === undefined) { penalty = congestion(a, b, segments); penalties.set(cacheKey, penalty); }
      const cost = current.cost + distance(a, b) + penalty + (axis && axis !== nextAxis ? 24 : 0);
      const key = nextCell * 3 + nextAxis;
      if (cost >= (costs.get(key) ?? Infinity)) continue;
      costs.set(key, cost); previous.set(key, current.key); queue.push({ key, cost, priority: cost + distance(b, end) });
    }
  }
}
const overlap = (a: { x: number; y: number; width: number; height: number }, b: { x: number; y: number; width: number; height: number }) => a.x < b.x + b.width && a.x + a.width > b.x && a.y < b.y + b.height && a.y + a.height > b.y;
function labelPosition(points: Point[], size: { width: number; height: number }, obstacles: Obstacle[], paths: Point[][]): EdgeRoute["label"] | undefined {
  const { width, height } = size;
  if (!width && !height) return { ...points[0], width: 0, height: 0 };
  const segments = points.slice(1).map((b, i) => ({ a: points[i], b, index: i, length: distance(points[i], b) }))
    .filter(({ a, b, length }) => a.y === b.y && length >= width + 16).sort((a, b) => b.length - a.length);
  for (const { a, b, index } of segments) for (const fraction of [0.5, 0.25, 0.75]) {
    const cx = Math.max(Math.min(a.x, b.x) + width / 2 + 8, Math.min(Math.max(a.x, b.x) - width / 2 - 8, a.x + (b.x - a.x) * fraction));
    const rect = { x: cx - width / 2, y: a.y - height / 2, width, height, nodeId: "label" };
    if (obstacles.some(r => overlap(rect, inflate(r, 5))) || paths.some(path => path.slice(1).some((point, i) => segmentHitsRectangle(path[i], point, inflate(rect, 4))))) continue;
    if (points.slice(1).some((point, i) => i !== index && segmentHitsRectangle(points[i], point, inflate(rect, 4)))) continue;
    return { x: cx, y: a.y, width, height };
  }
}
/** Reserve a horizontal label corridor and route both ends to it. The bounded
 * candidate search may fail explicitly; it never emits a detached label. */
function viaLabelCorridor(start: Point, end: Point, size: { width: number; height: number }, obstacles: Obstacle[], used: Point[][], accept: (points: Point[]) => boolean, step: () => void = noStep) {
  const xs = new Set([(start.x + end.x) / 2, start.x, end.x]);
  const ys = new Set([(start.y + end.y) / 2, start.y, end.y]);
  for (const r of obstacles) {
    xs.add(r.x - size.width / 2 - 16); xs.add(r.x + r.width + size.width / 2 + 16);
    ys.add(r.y - size.height / 2 - 16); ys.add(r.y + r.height + size.height / 2 + 16);
  }
  const candidates = [...xs].flatMap(x => [...ys].map(y => ({ x, y })))
    .sort((a, b) => distance(start, a) + distance(a, end) - distance(start, b) - distance(b, end) || a.y - b.y || a.x - b.x);
  let attempts = 0;
  for (const center of candidates) {
    const rect = { x: center.x - size.width / 2, y: center.y - size.height / 2, ...size, nodeId: "label-corridor" };
    if (obstacles.some(r => overlap(inflate(rect, 8), r)) || used.some(path => path.slice(1).some((p, i) => segmentHitsRectangle(path[i], p, inflate(rect, 4))))) continue;
    if (++attempts > 64) break;
    for (const direction of [1, -1]) {
      const a = { x: center.x - direction * (size.width / 2 + 12), y: center.y };
      const b = { x: center.x + direction * (size.width / 2 + 12), y: center.y };
      const reserved = [...obstacles, inflate(rect, 8)];
      const first = search(start, a, reserved, used, true, step);
      if (!first) continue;
      const last = search(b, end, reserved, used, true, step);
      if (last) {
        const points = simplify([...first.points, b, ...last.points]);
        if (accept(points)) return points;
      }
    }
  }
}
/** Deterministic rectilinear routing around resource cards and boundary headers.
 * Graph relationships are unchanged; geometry and labels are presentation only. */
export function routeArchitectureEdges(nodes: readonly ArchitectureNode[], edges: readonly ArchitectureEdge[], options: RoutingOptions = {}): RoutedArchitectureEdge[] {
  const boxes = absoluteArchitectureNodes(nodes), used: Point[][] = [], labels: Obstacle[] = [], routed: RoutedArchitectureEdge[] = [];
  const ordered = [...edges].sort((a, b) => a.id < b.id ? -1 : a.id > b.id ? 1 : 0);
  const incident = new Map<string, string[]>();
  for (const edge of ordered) for (const id of [edge.source, edge.target]) incident.set(id, [...(incident.get(id) ?? []), edge.id]);
  const fraction = (nodeId: string, edgeId: string) => { const ids = incident.get(nodeId)!; return (ids.indexOf(edgeId) + 1) / (ids.length + 1); };
  if (new Set(edges.map(e => e.id)).size !== edges.length) throw new ArchitectureRoutingError("<graph>", "duplicate edge IDs");
  // Reserve short local connections before long routes can cross their narrow
  // corridors. Port allocation above remains sorted by immutable edge ID.
  const span = (edge: ArchitectureEdge) => {
    const a = boxes.get(edge.source), b = boxes.get(edge.target);
    return a && b ? distance({ x: a.x + a.width / 2, y: a.y + a.height / 2 }, { x: b.x + b.width / 2, y: b.y + b.height / 2 }) : Infinity;
  };
  for (const edge of [...ordered].sort((a, b) => span(a) - span(b))) {
    const raw = architectureObstacles(nodes, edge);
    const source = boxes.get(edge.source), target = boxes.get(edge.target);
    if (!source || !target) throw new ArchitectureRoutingError(edge.id, "missing endpoint");
    if (edge.source === edge.target) throw new ArchitectureRoutingError(edge.id, "self-loop requires distinct endpoint ports");
    const size = edge.label === "" ? { width: 0, height: 0 } : options.measureLabel?.(edge.label, edge) ?? { width: Math.max(40, edge.label.length * 6.5 + 16), height: 22 };
    if (edge.label !== "" && ![size.width, size.height].every(n => Number.isFinite(n) && n > 0)) throw new ArchitectureRoutingError(edge.id, "invalid measured label dimensions");
    const padding = Math.max(10, size.height / 2 + 6);
    const obstacles = [...raw.map(r => inflate(r, padding)), ...labels.map(r => inflate(r, 8))];
    const pairs = sides.flatMap(sourceSide => sides.map(targetSide => {
      const a = port(source, sourceSide, fraction(edge.source, edge.id), padding + 6), b = port(target, targetSide, fraction(edge.target, edge.id), padding + 6);
      return { sourceSide, targetSide, a, b, estimate: distance(a.stub, b.stub) };
    })).sort((a, b) => a.estimate - b.estimate);
    const priorSegments = used.flatMap(points => points.slice(1).map((b, i) => ({ a: points[i], b })));
    let best: { points: Point[]; cost: number; sourceSide: Side; targetSide: Side; hasLabel: boolean } | undefined;
    for (const candidate of pairs) {
      if (best?.hasLabel && candidate.estimate > best.cost) break;
      // The short connector may cross its own padding, but no other card/title.
      if ([...raw, ...labels].some(r => r.nodeId !== edge.source && segmentHitsRectangle(candidate.a.anchor, candidate.a.stub, r) || r.nodeId !== edge.target && segmentHitsRectangle(candidate.b.anchor, candidate.b.stub, r))) continue;
      const found = search(candidate.a.stub, candidate.b.stub, obstacles, used, false, options.onSearchStep);
      if (!found) continue;
      const points = simplify([candidate.a.anchor, ...found.points, candidate.b.anchor]);
      // Include the short port connectors: in dense drawings another route may
      // cross these even when the stub-to-stub search path is clear.
      const cost = found.cost + congestion(candidate.a.anchor, candidate.a.stub, priorSegments) + congestion(candidate.b.stub, candidate.b.anchor, priorSegments);
      const hasLabel = !!labelPosition(points, size, [...raw, ...labels], used);
      if (best && (best.hasLabel && !hasLabel || best.hasLabel === hasLabel && cost >= best.cost)) continue;
      best = { points, cost, sourceSide: candidate.sourceSide, targetSide: candidate.targetSide, hasLabel };
      if (hasLabel && cost <= candidate.estimate + 48) break;
    }
    if (!best) throw new ArchitectureRoutingError(edge.id, "no obstacle-free path");
    let label = labelPosition(best.points, size, [...raw, ...labels], used);
    if (!label) {
      const candidate = pairs.find(p => p.sourceSide === best!.sourceSide && p.targetSide === best!.targetSide)!;
      const detour = viaLabelCorridor(candidate.a.stub, candidate.b.stub, size, obstacles, used,
        points => !!labelPosition(simplify([candidate.a.anchor, ...points, candidate.b.anchor]), size, [...raw, ...labels], used), options.onSearchStep);
      if (detour) {
        best.points = simplify([candidate.a.anchor, ...detour, candidate.b.anchor]);
        label = labelPosition(best.points, size, [...raw, ...labels], used);
      }
    }
    if (!label) throw new ArchitectureRoutingError(edge.id, "no safe horizontal label corridor within search budget");
    if (edge.label !== "") labels.push({ ...label, x: label.x - label.width / 2, y: label.y - label.height / 2, nodeId: `label:${edge.id}` });
    used.push(best.points);
    routed.push({ ...edge, data: { ...edge.data, route: { sourceAnchor: best.points[0], targetAnchor: best.points.at(-1)!, points: best.points, sourceSide: best.sourceSide, targetSide: best.targetSide, label } } });
  }
  return routed.sort((a, b) => a.id < b.id ? -1 : a.id > b.id ? 1 : 0);
}
export function architectureEdgePath(points: readonly Point[], radius = 8): string {
  if (!points.length) return "";
  let path = `M ${points[0].x} ${points[0].y}`;
  for (let i = 1; i < points.length - 1; i++) {
    const a = points[i - 1], b = points[i], c = points[i + 1], r = Math.min(radius, distance(a, b) / 2, distance(b, c) / 2);
    const before = { x: b.x + Math.sign(a.x - b.x) * r, y: b.y + Math.sign(a.y - b.y) * r }, after = { x: b.x + Math.sign(c.x - b.x) * r, y: b.y + Math.sign(c.y - b.y) * r };
    path += ` L ${before.x} ${before.y} Q ${b.x} ${b.y} ${after.x} ${after.y}`;
  }
  const end = points.at(-1)!; return `${path} L ${end.x} ${end.y}`;
}
