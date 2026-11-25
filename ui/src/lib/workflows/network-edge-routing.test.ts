import assert from "node:assert/strict";
import test from "node:test";
import type { ArchitectureEdge, ArchitectureNode } from "./network-topology";
import { ArchitectureRoutingError, architectureObstacles, routeArchitectureEdges } from "./network-edge-routing";

type Point = { x: number; y: number };
type Rect = Point & { width: number; height: number };

function node(id: string, x: number, y: number, parentId?: string, group = false, width = 256, height = 112): ArchitectureNode {
  return { id, type: group ? "group" : "resource", parentId, position: { x, y }, width, height,
    data: { title: id, kind: group ? "Subnet" : "Route", resourceId: id, cloudId: "", state: "Ready", tone: "success", paused: false, href: "", message: "", presentation: group ? "group" : "resource" } };
}
function edge(id: string, source: string, target: string, label = "0.0.0.0/0"): ArchitectureEdge {
  return { id, source, target, label, data: { relation: "forwards_to", evidence: "observed", derived: false } };
}
const overlaps = (a: Rect, b: Rect) => a.x < b.x + b.width && a.x + a.width > b.x && a.y < b.y + b.height && a.y + a.height > b.y;
function crossesInterior(a: Point, b: Point, r: Rect) {
  if (a.x === b.x) return a.x > r.x && a.x < r.x + r.width && Math.max(a.y, b.y) > r.y && Math.min(a.y, b.y) < r.y + r.height;
  return a.y > r.y && a.y < r.y + r.height && Math.max(a.x, b.x) > r.x && Math.min(a.x, b.x) < r.x + r.width;
}
function absoluteRect(node: ArchitectureNode, nodes: ArchitectureNode[]): Rect {
  let { x, y } = node.position;
  let parent = node.parentId;
  while (parent) {
    const ancestor = nodes.find(candidate => candidate.id === parent)!;
    x += ancestor.position.x; y += ancestor.position.y; parent = ancestor.parentId;
  }
  return { x, y, width: node.width, height: node.height };
}
function assertPort(point: Point, side: "top" | "right" | "bottom" | "left", rect: Rect) {
  assert.ok(Number.isFinite(point.x) && Number.isFinite(point.y));
  if (side === "top" || side === "bottom") {
    assert.equal(point.y, rect.y + (side === "bottom" ? rect.height : 0));
    assert.ok(point.x >= rect.x && point.x <= rect.x + rect.width);
  } else {
    assert.equal(point.x, rect.x + (side === "right" ? rect.width : 0));
    assert.ok(point.y >= rect.y && point.y <= rect.y + rect.height);
  }
}
function assertClearRoutes(nodes: ArchitectureNode[], edges: ArchitectureEdge[], options?: Parameters<typeof routeArchitectureEdges>[2]) {
  const routes = routeArchitectureEdges(nodes, edges, options);
  const obstacles = architectureObstacles(nodes);
  const labels: Rect[] = [];
  assert.equal(routes.length, edges.length);
  for (const routed of routes) {
    const routeObstacles = architectureObstacles(nodes, routed);
    const original = edges.find(candidate => candidate.id === routed.id)!;
    assert.equal(routed.source, original.source);
    assert.equal(routed.target, original.target);
    assert.equal(routed.label, original.label);
    assert.equal(routed.data.evidence, original.data.evidence);
    assert.equal(routed.data.relation, original.data.relation);
    const { points, label, sourceSide, targetSide } = routed.data.route;
    assert.ok(points.length >= 2);
    assert.deepEqual(routed.data.route.sourceAnchor, points[0]);
    assert.deepEqual(routed.data.route.targetAnchor, points.at(-1));
    if (routed.label !== "") assert.ok(points.slice(1).some((b, i) => {
      const a = points[i];
      return a.y === b.y && label.y === a.y && label.x - label.width / 2 >= Math.min(a.x, b.x) && label.x + label.width / 2 <= Math.max(a.x, b.x);
    }), `${routed.id} label must lie on its own horizontal segment`);
    assertPort(points[0], sourceSide, absoluteRect(nodes.find(n => n.id === routed.source)!, nodes));
    assertPort(points.at(-1)!, targetSide, absoluteRect(nodes.find(n => n.id === routed.target)!, nodes));
    for (let i = 1; i < points.length; i++) {
      const a = points[i - 1], b = points[i];
      assert.ok(a.x === b.x || a.y === b.y, `${routed.id} has a diagonal segment`);
      for (const obstacle of routeObstacles) {
        assert.equal(crossesInterior(a, b, obstacle), false, `${routed.id} crosses ${obstacle.nodeId}`);
      }
    }
    if (routed.label === "") { assert.equal(label.width, 0); assert.equal(label.height, 0); continue; }
    assert.ok(label.width > 0 && label.height > 0);
    assert.ok(Number.isFinite(label.x) && Number.isFinite(label.y));
    const labelRect = { ...label, x: label.x - label.width / 2, y: label.y - label.height / 2 };
    for (const obstacle of obstacles) assert.equal(overlaps(labelRect, obstacle), false, `${routed.id} label overlaps ${obstacle.nodeId}`);
    for (const previous of labels) assert.equal(overlaps(labelRect, previous), false, `${routed.id} labels overlap`);
    labels.push(labelRect);
  }
  for (const route of routes) {
    if (!route.label) continue;
    const box = route.data.route.label;
    const rect = { ...box, x: box.x - box.width / 2, y: box.y - box.height / 2 };
    for (const other of routes) {
      if (other.id === route.id) continue;
      for (let index = 1; index < other.data.route.points.length; index++) {
        assert.equal(crossesInterior(other.data.route.points[index - 1], other.data.route.points[index], rect), false, `${other.id} crosses ${route.id} label`);
      }
    }
  }
  return routes;
}

test("obstacles resolve nested coordinates and reserve only group headers", () => {
  const nodes = [node("vpc", 100, 200, undefined, true, 1600, 900), node("zone", 40, 100, "vpc", true, 700, 700), node("subnet", 24, 88, "zone", true, 620, 500), node("route", 24, 96, "subnet")];
  const obstacles = architectureObstacles(nodes);
  assert.equal(obstacles.length, 4);
  assert.deepEqual(obstacles.find(r => r.nodeId === "vpc"), { nodeId: "vpc", x: 100, y: 200, width: 1600, height: 76 });
  assert.deepEqual(obstacles.find(r => r.nodeId === "zone"), { nodeId: "zone", x: 140, y: 300, width: 700, height: 76 });
  assert.deepEqual(obstacles.find(r => r.nodeId === "route"), { nodeId: "route", x: 188, y: 484, width: 256, height: 112 });
});

test("orthogonal paths avoid intervening cards and keep labels clear", () => {
  const nodes = [node("source", 0, 120), node("blocker", 360, 100, undefined, false, 256, 180), node("target", 760, 120)];
  assertClearRoutes(nodes, [edge("forward", "source", "target")]);
});

function twoZones() {
  const nodes = [
    node("vpc", 80, 40, undefined, true, 1500, 1100),
    node("zone-a", 32, 120, "vpc", true, 680, 850), node("zone-b", 760, 120, "vpc", true, 680, 850),
    node("public-a", 24, 96, "zone-a", true, 620, 320), node("private-a", 24, 480, "zone-a", true, 620, 280),
    node("public-b", 24, 96, "zone-b", true, 620, 320), node("private-b", 24, 480, "zone-b", true, 620, 280),
    node("public-table", 24, 96, "public-a"), node("nat", 324, 96, "public-a"),
    node("private-table", 24, 96, "private-b"), node("private-route", 324, 96, "private-b"),
    node("internet-gateway", 1100, 992, "vpc"), node("internet", 1680, 1032),
  ];
  const edges = [edge("table-route", "private-table", "private-route", "Routes"), edge("cross-zone-nat", "private-route", "nat"), edge("nat-public-table", "nat", "public-table", "Public subnet egress"), edge("gateway-internet", "internet-gateway", "internet", "Internet egress (logical)")];
  return { nodes, edges };
}

test("two-zone NAT paths avoid nested group headers and unrelated resources", () => {
  const { nodes, edges } = twoZones();
  assertClearRoutes(nodes, edges);
});

test("routing is deterministic under reordered inputs and does not mutate the model", () => {
  const { nodes, edges } = twoZones();
  const before = structuredClone({ nodes, edges });
  const sorted = (routes: ReturnType<typeof routeArchitectureEdges>) => routes.slice().sort((a, b) => a.id.localeCompare(b.id));
  assert.deepEqual(sorted(routeArchitectureEdges(nodes, edges)), sorted(routeArchitectureEdges([...nodes].reverse(), [...edges].reverse())));
  assert.deepEqual({ nodes, edges }, before);
});

test("empty graphs succeed and missing endpoints fail explicitly", () => {
  assert.deepEqual(architectureObstacles([]), []);
  assert.deepEqual(routeArchitectureEdges([], []), []);
  const nodes = [node("present", 0, 0)];
  assert.throws(() => routeArchitectureEdges(nodes, [edge("missing-target", "present", "absent")]),
    (error: unknown) => error instanceof ArchitectureRoutingError && error.edgeId === "missing-target" && error.reason === "missing endpoint");
});


test("measured labels reserve full corridors, including a vertical relationship", () => {
  const nodes = [node("a", 0, 0), node("b", 0, 360), node("c", 600, 360)];
  const routes = assertClearRoutes(nodes, [edge("a-b", "a", "b", "宽いラベル"), edge("b-c", "b", "c")], {
    measureLabel: () => ({ width: 300, height: 42 }),
  });
  for (const route of routes) assert.equal(route.data.route.label.width, 300);
});

test("high degree and parallel edges receive distinct deterministic anchors", () => {
  const nodes = [node("hub", 0, 300, undefined, false, 256, 600), ...Array.from({ length: 8 }, (_, i) => node(`n${i}`, 700, i * 180))];
  const edges = nodes.slice(1).map((n, i) => edge(`e${i}`, "hub", n.id, `Path ${i}`));
  const routes = assertClearRoutes(nodes, edges);
  assert.equal(new Set(routes.map(e => JSON.stringify(e.data.route.sourceAnchor))).size, edges.length);
  assert.deepEqual(routeArchitectureEdges([...nodes].reverse(), [...edges].reverse()), routes);
  const parallel = assertClearRoutes([node("a", 0, 0), node("b", 700, 0)], [edge("one", "a", "b"), edge("two", "a", "b"), edge("three", "b", "a")]);
  const atA = parallel.map(e => e.source === "a" ? e.data.route.sourceAnchor : e.data.route.targetAnchor);
  assert.equal(new Set(atA.map(p => JSON.stringify(p))).size, 3);
});

test("blocked geometry, self loops and invalid measurements never silently drop edges", () => {
  assert.throws(() => routeArchitectureEdges([node("a", 0, 0), node("cover", -100, -100, undefined, false, 500, 400), node("b", 800, 0)], [edge("blocked", "a", "b")]), /blocked: no obstacle-free path/);
  assert.throws(() => routeArchitectureEdges([node("a", 0, 0)], [edge("loop", "a", "a")]), /loop: self-loop/);
  assert.throws(() => routeArchitectureEdges([node("a", 0, 0), node("b", 700, 0)], [edge("bad", "a", "b")], { measureLabel: () => ({ width: NaN, height: 22 }) }), /invalid measured/);
});


test("invalid node geometry and parent cycles are explicit errors", () => {
  assert.throws(() => routeArchitectureEdges([node("bad", NaN, 0)], []), /invalid geometry/);
  assert.throws(() => routeArchitectureEdges([node("a", 0, 0, "b"), node("b", 0, 0, "a")], []), /parent cycle/);
  assert.throws(() => routeArchitectureEdges([node("a", 0, 0, "absent")], []), /missing parent/);
});


test("measured group header height remains an obstacle", () => {
  const group = { ...node("group", 0, 0, undefined, true, 1000, 600), headerHeight: 130 };
  assert.equal(architectureObstacles([group])[0].height, 130);
  assertClearRoutes([group, node("a", 40, 180, "group"), node("b", 650, 180, "group")], [edge("ab", "a", "b")]);
});

function routeConflicts(routes: ReturnType<typeof routeArchitectureEdges>) {
  let crossings = 0, sharedVertical = 0, unrelatedOverlap = 0;
  for (let i = 0; i < routes.length; i++) for (let j = i + 1; j < routes.length; j++) {
    const first = routes[i], second = routes[j];
    for (let a = 1; a < first.data.route.points.length; a++) for (let b = 1; b < second.data.route.points.length; b++) {
      const p = first.data.route.points[a - 1], q = first.data.route.points[a];
      const r = second.data.route.points[b - 1], s = second.data.route.points[b];
      const vertical = p.x === q.x, otherVertical = r.x === s.x;
      if (vertical === otherVertical) {
        const same = vertical ? p.x === r.x : p.y === r.y;
        const length = vertical ? Math.min(Math.max(p.y, q.y), Math.max(r.y, s.y)) - Math.max(Math.min(p.y, q.y), Math.min(r.y, s.y)) : Math.min(Math.max(p.x, q.x), Math.max(r.x, s.x)) - Math.max(Math.min(p.x, q.x), Math.min(r.x, s.x));
        if (same && length > 0) {
          if (vertical) sharedVertical++;
          if (first.target !== second.target) unrelatedOverlap++;
        }
      } else {
        const v = vertical ? p : r, vEnd = vertical ? q : s, h = vertical ? r : p, hEnd = vertical ? s : q;
        if (v.x > Math.min(h.x, hEnd.x) && v.x < Math.max(h.x, hEnd.x) && h.y > Math.min(v.y, vEnd.y) && h.y < Math.max(v.y, vEnd.y)) crossings++;
      }
    }
  }
  return { crossings, sharedVertical, unrelatedOverlap };
}

// Routing work per fixture, counted in candidate paths checked plus grid cells
// expanded (RoutingOptions.onSearchStep) instead of wall-clock time, so a busy
// machine cannot fail it while a slower algorithm still does. The count is
// deterministic; each budget is about 1.5x today's larger count of the two
// header variants (A 28306, B 2127, C 119599, D 5858, E 32770, F 1207,
// G 26501, H 9458, I 9128). Raise one only for a deliberate routing change.
const routingStepBudgets = { A: 43_000, B: 3_200, C: 180_000, D: 8_800, E: 49_000, F: 1_800, G: 40_000, H: 14_000, I: 13_700 };

test("A–I overview bounds crossings and overlap while retaining safe geometry", async t => {
  // Independent segment-pair counts, with local destinations excluded by the
  // overview. Keep full topology geometry coverage separate below.
  const crossingLimits = { A: 6, B: 2, C: 8, D: 7, E: 8, F: 0, G: 12, H: 1, I: 2 };
  const { createNetworkTopologyFixture, networkTopologyFixtureIds } = await import("./network-topology-fixtures");
  const { buildNetworkArchitectureOverview } = await import("./network-topology");
  for (const id of networkTopologyFixtureIds) for (const headerHeight of [undefined, 64]) {
    await t.test(`fixture ${id}${headerHeight ? " measured full-width headers" : ""}`, t => {
      const { network } = createNetworkTopologyFixture(id);
      const initial = buildNetworkArchitectureOverview(network);
      const dimensions = headerHeight ? new Map(initial.nodes.filter(node => node.type === "group").map(node => [node.id, { width: 288, height: headerHeight, headerHeight }])) : undefined;
      const graph = dimensions ? buildNetworkArchitectureOverview(network, undefined, [], dimensions) : initial;
      assert.ok(graph.edges.length > 0, `${id} must exercise routing`);
      const visibleEdges = graph.edges.filter(edge => edge.data.relation !== "nat_internet_egress");
      let steps = 0;
      const routes = assertClearRoutes(graph.nodes, visibleEdges, { onSearchStep: () => { steps++; } });
      const conflicts = routeConflicts(routes);
      assert.ok(conflicts.crossings <= crossingLimits[id], `${id}: ${conflicts.crossings} crossings exceed ${crossingLimits[id]}`);
      assert.equal(conflicts.sharedVertical, 0, `${id} contains a shared vertical trunk`);
      assert.equal(conflicts.unrelatedOverlap, 0, `${id} contains overlap between different targets`);
      t.diagnostic(JSON.stringify({ ...conflicts, steps }));
      assert.ok(steps <= routingStepBudgets[id], `${id} routing examined ${steps} paths and grid cells, over the budget of ${routingStepBudgets[id]}`);
    });
  }
});


test("empty labels allocate no corridor and do not call the measurer", () => {
  const routes = assertClearRoutes([node("a", 0, 0), node("b", 300, 0)], [edge("unlabeled", "a", "b", "")], {
    measureLabel: () => { throw new Error("Empty labels must not be measured"); },
  });
  assert.equal(routes[0].data.route.label.width, 0);
  assert.equal(routes[0].data.route.label.height, 0);
});

test("routes go around the full body of an unrelated container", () => {
  const nodes = [node("a", 0, 250), node("unrelated", 350, 100, undefined, true, 300, 500), node("b", 800, 250)];
  const connection = edge("ab", "a", "b");
  assert.equal(architectureObstacles(nodes, connection).find(n => n.nodeId === "unrelated")!.height, 500);
  assertClearRoutes(nodes, [connection]);
});


test("full A–I graphs preserve local destinations and route every edge safely", async t => {
  const { createNetworkTopologyFixture, networkTopologyFixtureIds } = await import("./network-topology-fixtures");
  const { buildNetworkArchitecture } = await import("./network-topology");
  for (const id of networkTopologyFixtureIds) for (const headerHeight of [undefined, 64]) await t.test(`full fixture ${id}${headerHeight ? " measured headers" : ""}`, () => {
    const network = createNetworkTopologyFixture(id).network;
    const initial = buildNetworkArchitecture(network);
    const dimensions = headerHeight ? new Map(initial.nodes.filter(node => node.type === "group").map(node => [node.id, { width: 288, height: headerHeight, headerHeight }])) : undefined;
    const graph = dimensions ? buildNetworkArchitecture(network, undefined, [], dimensions) : initial;
    assert.ok(graph.nodes.some(node => node.data.kind === "Local route destination"));
    assertClearRoutes(graph.nodes, graph.edges);
  });
});


test("independent vertical flows use distinct lanes around the same blockers", () => {
  const nodes = [node("a", 0, 0), node("c", 0, 180), node("blocker", 0, 400), node("b", 0, 700), node("d", 0, 900)];
  const routes = assertClearRoutes(nodes, [edge("first", "a", "b", ""), edge("second", "c", "d", "")]);
  assert.equal(routeConflicts(routes).unrelatedOverlap, 0);
});


test("measured header width only narrows ancestor headers, never unrelated containers", () => {
  const group = { ...node("group", 0, 0, undefined, true, 1000, 600), headerHeight: 64, headerWidth: 300 };
  const child = node("child", 24, 88, "group"), outside = node("outside", 1200, 150);
  const nodes = [group, child, outside];
  assert.equal(architectureObstacles(nodes, edge("from-child", "child", "outside"))[0].width, 300);
  assert.equal(architectureObstacles(nodes, edge("unrelated", "outside", "outside"))[0].width, 1000);
  assert.equal(architectureObstacles([{ ...group, headerWidth: undefined }])[0].width, 1000);
  assertClearRoutes(nodes, [edge("from-child", "child", "outside")]);
});
