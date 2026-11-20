import { create, toJsonString } from "@bufbuild/protobuf";
import { expect, test, type Locator, type Page } from "@playwright/test";
import { ClusterArchitectureSchema, ClusterSchema, ClusterState } from "../../src/gen/cloud/v1/cluster_pb";
import { ConditionStatus } from "../../src/gen/cloud/v1/conditions_pb";
import { ManagedResourceState } from "../../src/gen/cloud/v1/managed_resource_pb";
import { NetworkSchema, SubnetRole } from "../../src/gen/cloud/v1/network_pb";
import { createNetworkTopologyFixture } from "../../src/lib/workflows/network-topology-fixtures";

const accountId = "00000000-0000-4000-8000-000000000001";
const networkId = "00000000-0000-4000-8000-000000000002";
const clusterId = "00000000-0000-4000-8000-000000000003";
const controlArn = "arn:aws:eks:us-west-2:111122223333:cluster/payments-physical";
const nodeGroupArn = "arn:aws:eks:us-west-2:111122223333:nodegroup/payments-physical/workers/fixture";
const architecture = create(ClusterArchitectureSchema, {
  resources: [
    { kind: "Cluster", name: "payments-control", cloudId: controlArn },
    { kind: "NodeGroup", name: "payments-workers", cloudId: nodeGroupArn },
    { kind: "Addon", name: "vpc-cni", cloudId: "arn:aws:eks:us-west-2:111122223333:addon/payments-physical/vpc-cni/fixture" },
    { kind: "Release", name: "metrics-server", cloudId: "metrics-server" },
    { kind: "LaunchTemplate", name: "workers-template", cloudId: "lt-cluster-topology" },
    { kind: "Subnet", name: "private-a", cloudId: "subnet-cluster-a", zoneId: "usw2-az1", subnetRole: SubnetRole.PRIVATE, cidrBlock: "10.0.10.0/24", referenceOnly: true },
    { kind: "Subnet", name: "private-b", cloudId: "subnet-cluster-b", zoneId: "usw2-az2", subnetRole: SubnetRole.PRIVATE, cidrBlock: "10.0.11.0/24", referenceOnly: true },
    { kind: "SecurityGroup", name: "external-security-group", cloudId: "sg-cluster-reference", referenceOnly: true },
  ],
  connections: [
    { sourceKind: "Cluster", sourceName: "payments-control", targetKind: "NodeGroup", targetName: "payments-workers", relation: "has_node_group", label: "Node group", evidence: "observed" },
    { sourceKind: "Cluster", sourceName: "payments-control", targetKind: "Addon", targetName: "vpc-cni", relation: "has_addon", label: "Managed add-on", evidence: "observed" },
    { sourceKind: "Cluster", sourceName: "payments-control", targetKind: "Release", targetName: "metrics-server", relation: "has_component", label: "Helm component", evidence: "configured" },
    { sourceKind: "NodeGroup", sourceName: "payments-workers", targetKind: "LaunchTemplate", targetName: "workers-template", relation: "uses_launch_template", label: "Launch template", evidence: "observed" },
    { sourceKind: "Cluster", sourceName: "payments-control", targetKind: "SecurityGroup", targetName: "external-security-group", relation: "uses_security_group", label: "Security group", evidence: "observed" },
    ...["private-a", "private-b"].flatMap(name => [
      { sourceKind: "Cluster", sourceName: "payments-control", targetKind: "Subnet", targetName: name, relation: "uses_subnet", label: "Control plane subnet", evidence: "observed" },
      { sourceKind: "NodeGroup", sourceName: "payments-workers", targetKind: "Subnet", targetName: name, relation: "uses_subnet", label: "Node group subnet", evidence: "configured" },
    ]),
  ],
});
const cluster = create(ClusterSchema, {
  id: clusterId, tenantId: "acme", name: "payments", displayName: "Payments", generation: 1n, etag: "cluster-topology-etag",
  spec: { region: "us-west-2", network: { networkId }, defaultNodePool: { instanceTypes: ["m6i.large"], scalingConfig: { minSize: 2, maxSize: 6, desiredSize: 3 }, diskSizeGb: 80, labels: { workload: "payments" } }, provider: { case: "aws", value: { irsa: true } } },
  status: { state: ClusterState.RUNNING, observedGeneration: 1n, architecture, provider: { case: "aws", value: { clusterArn: controlArn, nodeGroupArn } },
    managedResources: architecture.resources.filter(resource => !resource.referenceOnly).map(resource => ({ kind: resource.kind, name: resource.name, externalName: resource.cloudId, generation: 1n, observedGeneration: 1n, state: ManagedResourceState.READY, ready: ConditionStatus.TRUE, synced: ConditionStatus.TRUE })),
  },
});
const wireCluster = JSON.parse(toJsonString(ClusterSchema, cluster)) as Record<string, unknown>;
type Call = { service: string; method: string; body: Record<string, unknown> };
type MockOptions = { missingProjection?: boolean; foreignTenant?: boolean; foreignId?: boolean; failLive?: boolean; networkFixture?: "C" | "D" };

test.beforeEach(async ({ page }) => {
  await page.route("**/api/auth/session", route => route.fulfill({ contentType: "application/json", body: JSON.stringify({ enabled: true, authenticated: true, csrfToken: "cluster-topology-test-csrf", username: "viewer" }) }));
  await page.addInitScript(() => {
    const state = window as unknown as { topologyCopiedValues: string[] };
    state.topologyCopiedValues = [];
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText: async (value: string) => { state.topologyCopiedValues.push(value); } } });
  });
});

async function mockClusterTopology(page: Page, options: MockOptions = {}) {
  const calls: Call[] = [], unexpected: string[] = [];
  const linkedNetwork = createNetworkTopologyFixture(options.networkFixture ?? "D").network;
  linkedNetwork.id = networkId; linkedNetwork.tenantId = "acme"; linkedNetwork.spec!.bootstrapId = accountId;
  const privateSubnets = linkedNetwork.status!.architecture!.resources.filter(resource => resource.kind === "Subnet" && resource.subnetRole === SubnetRole.PRIVATE);
  const responseCluster = structuredClone(wireCluster);
  const clusterStatus = responseCluster.status as Record<string, unknown>;
  const clusterArchitecture = clusterStatus.architecture as { resources: Record<string, unknown>[] };
  for (const [index, name] of ["private-a", "private-b"].entries()) {
    const reference = clusterArchitecture.resources.find(resource => resource.kind === "Subnet" && resource.name === name)!;
    Object.assign(reference, { cloudId: privateSubnets[index].cloudId, zoneId: privateSubnets[index].zoneId, cidrBlock: privateSubnets[index].cidrBlock });
  }
  await page.route("**/api/rpc/**", async route => {
    const parts = new URL(route.request().url()).pathname.split("/");
    const method = parts.at(-1)!, service = parts.at(-2)!;
    const body = route.request().postDataJSON() as Record<string, unknown>;
    calls.push({ method, service, body });
    let response: object;
    switch (method) {
      case "WhoAmI": response = { subject: "cluster-topology-viewer", grants: [{ role: "ACCESS_ROLE_TENANT_VIEWER", tenants: ["acme"] }] }; break;
      case "ListTenants": response = { tenants: [{ id: "acme" }], canCreateTenants: false }; break;
      case "GetCluster": {
        if (body.live && options.failLive) { await route.fulfill({ status: 503, contentType: "application/json", body: JSON.stringify({ code: "unavailable", message: "Live cluster architecture temporarily unavailable" }) }); return; }
        const status = responseCluster.status as Record<string, unknown>;
        response = { cluster: { ...responseCluster,
          ...(body.live && options.foreignTenant ? { tenantId: "foreign", displayName: "Foreign topology" } : {}),
          ...(body.live && options.foreignId ? { id: "00000000-0000-4000-8000-000000000099", displayName: "Foreign topology" } : {}),
          status: { ...status, architecture: body.live && !options.missingProjection ? status.architecture : undefined },
        }, asRequested: true }; break;
      }
      case "GetNetwork": {
        const liveNetwork = JSON.parse(toJsonString(NetworkSchema, linkedNetwork)) as Record<string, unknown>;
        const status = liveNetwork.status as Record<string, unknown>;
        response = { network: { ...liveNetwork, status: { ...status, architecture: body.live ? status.architecture : undefined } }, asRequested: true }; break;
      }
      case "ListNetworks": response = { networks: [] }; break;
      case "ListCloudAccounts": response = { accounts: [{ id: accountId, tenantId: "acme", accountName: "Production account", aws: { accountId: "111122223333" }, status: { state: "CLOUD_ACCOUNT_STATE_READY" } }] }; break;
      case "GetCloudAccount": response = { account: { id: accountId, tenantId: "acme", accountName: "Production account", aws: { accountId: "111122223333" }, status: { state: "CLOUD_ACCOUNT_STATE_READY" } } }; break;
      case "ListClusterEvents": response = { events: [] }; break;
      case "ListComponents": response = { components: [] }; break;
      default:
        unexpected.push(`${service}/${method}`);
        await route.fulfill({ status: 501, contentType: "application/json", body: JSON.stringify({ code: "unimplemented", message: `Unexpected cluster topology RPC ${method}` }) }); return;
    }
    await route.fulfill({ contentType: "application/json", body: JSON.stringify(response) });
  });
  return { calls, unexpected, privateSubnets, linkedNetwork };
}

async function openTopology(page: Page) {
  await page.goto(`/clusters/${clusterId}`);
  await page.getByRole("tab", { name: "Topology", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Cluster architecture", exact: true })).toBeVisible();
  return page.getByLabel("Cluster topology canvas", { exact: true });
}
function assertReadOnly(api: Awaited<ReturnType<typeof mockClusterTopology>>) {
  expect(api.unexpected).toEqual([]);
  expect(api.calls.some(call => call.method === "GetCluster" && call.body.live === true)).toBe(true);
  expect(api.calls.filter(call => call.method === "GetCluster").every(call => call.body.tenantId === "acme" && call.body.clusterId === clusterId)).toBe(true);
  expect(api.calls.filter(call => call.method === "GetNetwork").every(call => call.body.tenantId === "acme" && call.body.networkId === networkId)).toBe(true);
  expect(api.calls.filter(call => /AdminService/.test(call.service) || !/^(WhoAmI|List|Get)/.test(call.method))).toEqual([]);
  expect(api.calls.filter(call => !["WhoAmI", "ListTenants"].includes(call.method)).every(call => call.body.tenantId === "acme")).toBe(true);
}
async function settleDiagram(page: Page, canvas: Locator) {
  await expect(canvas).toBeVisible();
  await page.evaluate(() => document.fonts.ready);
  await expect(canvas.locator(".react-flow__node")).not.toHaveCount(0);
  await expect(canvas.locator(".react-flow__edge-path")).not.toHaveCount(0);
  // A painted straight SVG path can have zero bounding-box width or height.
  await expect.poll(() => canvas.locator(".react-flow__edge-path").first().evaluate(element => {
    const style = getComputedStyle(element);
    return (element as SVGPathElement).getTotalLength() > 0 && style.stroke !== "none" && style.visibility !== "hidden" && style.display !== "none" && Number(style.opacity) > 0;
  })).toBe(true);
  await expect.poll(async () => {
    const viewport = canvas.locator(".react-flow__viewport");
    const first = await viewport.getAttribute("style");
    await page.waitForTimeout(100);
    return first === await viewport.getAttribute("style");
  }).toBe(true);
}
async function assertGeometry(canvas: Locator) {
  const errors = await canvas.evaluate(root => {
    const errors: string[] = [];
    const nodes = [...root.querySelectorAll<HTMLElement>(".react-flow__node")];
    const paths = [...root.querySelectorAll<SVGPathElement>(".react-flow__edge-path")];
    const leafCards = [...root.querySelectorAll<HTMLElement>(".react-flow__node-resource, .react-flow__node-external")].map(node => node.getBoundingClientRect());
    const overlaps = (a: DOMRect, b: DOMRect) => a.left + 1 < b.right && a.right - 1 > b.left && a.top + 1 < b.bottom && a.bottom - 1 > b.top;
    const contains = (a: DOMRect, b: DOMRect) => a.left <= b.left + 1 && a.right >= b.right - 1 && a.top <= b.top + 1 && a.bottom >= b.bottom - 1;
    const canvas = root.getBoundingClientRect();
    if (!nodes.length) errors.push("empty nodes");
    if (!paths.length) errors.push("empty edges");
    if (!leafCards.length) errors.push("empty leaf-card obstacles");
    for (const node of nodes) {
      const box = node.getBoundingClientRect(), style = getComputedStyle(node);
      if (!Number.isFinite(box.x) || !Number.isFinite(box.y) || box.width <= 1 || box.height <= 1 || style.display === "none" || style.visibility === "hidden" || Number(style.opacity) === 0) errors.push(`unpainted node ${node.dataset.id}`);
      let ancestor = node.parentElement;
      while (ancestor) {
        const inherited = getComputedStyle(ancestor);
        if (inherited.display === "none" || inherited.visibility === "hidden" || Number(inherited.opacity) === 0) { errors.push(`node hidden by ancestor ${node.dataset.id}`); break; }
        if (ancestor === root) break;
        ancestor = ancestor.parentElement;
      }
      if (box.right < canvas.left || box.left > canvas.right || box.bottom < canvas.top || box.top > canvas.bottom) errors.push(`node outside fitted canvas ${node.dataset.id}`);
    }
    for (let i = 0; i < nodes.length; i++) for (const b of nodes.slice(i + 1)) {
      const aBox = nodes[i].getBoundingClientRect(), bBox = b.getBoundingClientRect();
      const bothLeaves = nodes[i].classList.contains("react-flow__node-resource") && b.classList.contains("react-flow__node-resource");
      if (overlaps(aBox, bBox) && (bothLeaves || !contains(aBox, bBox) && !contains(bBox, aBox))) errors.push(`overlapping nodes ${nodes[i].dataset.id}/${b.dataset.id}`);
    }
    for (const path of paths) {
      const style = getComputedStyle(path);
      if (path.getTotalLength() <= 0 || style.display === "none" || style.visibility === "hidden" || Number(style.opacity) === 0 || style.stroke === "none") errors.push("unpainted edge");
      const matrix = path.getScreenCTM();
      if (!matrix) { errors.push("edge has no screen coordinate transform"); continue; }
      for (let distance = 1; distance < path.getTotalLength(); distance += 2) {
        const point = path.getPointAtLength(distance).matrixTransform(matrix);
        if (leafCards.some(card => point.x > card.left + 1 && point.x < card.right - 1 && point.y > card.top + 1 && point.y < card.bottom - 1)) { errors.push("edge intersects leaf-card interior"); break; }
      }
    }
    return errors;
  });
  expect(errors).toEqual([]);
}

test("default topology always shows the network with the control plane and node groups", async ({ page }) => {
  await page.setViewportSize({ width: 1920, height: 1200 });
  const api = await mockClusterTopology(page), canvas = await openTopology(page);
  await settleDiagram(page, canvas);
  await page.getByRole("button", { name: /fit view/i }).click();
  await settleDiagram(page, canvas);
  await assertGeometry(canvas);
  await expect(page.getByRole("checkbox", { name: "Show Network", exact: true })).toHaveCount(0);
  await expect.poll(() => api.calls.some(call => call.method === "GetNetwork" && call.body.live === true)).toBe(true);
  const vpc = api.linkedNetwork.status!.architecture!.resources.find(resource => resource.kind === "VPC")!;
  await expect(canvas.getByRole("button", { name: `Inspect ${vpc.name}`, exact: true })).toBeVisible();
  for (const name of ["payments-control", "payments-workers"]) await expect(canvas.getByRole("button", { name: `Inspect ${name}`, exact: true })).toBeVisible();
  const workerCard = canvas.locator(".react-flow__node-resource").filter({ has: page.getByRole("button", { name: "Inspect payments-workers", exact: true }) });
  await expect(workerCard).toContainText("Configured default pool");
  for (const [label, value] of [["Instance types", "m6i.large"], ["Minimum nodes", "2"], ["Maximum nodes", "6"], ["Desired nodes", "Not reported"], ["Current nodes", "Not reported"]]) {
    await expect(workerCard.getByText(label, { exact: true }).locator("..").getByText(value, { exact: true })).toBeVisible();
  }
  await expect(workerCard.getByText("Initial desired nodes", { exact: true })).toHaveCount(0);
  await expect(page.getByRole("table", { name: "Cluster topology resources", exact: true })).toHaveCount(0);
  await expect(page.getByRole("table", { name: "Cluster topology relationships", exact: true })).toHaveCount(0);
  assertReadOnly(api);
});

for (const networkFixture of ["C", "D"] as const) test(`network is always shown and loads actual ${networkFixture} architecture and preserves explicit node-group subnet paths`, async ({ page }) => {
  await page.setViewportSize({ width: 1920, height: 1200 });
  const api = await mockClusterTopology(page, { networkFixture }), canvas = await openTopology(page);
  await settleDiagram(page, canvas);
  await expect.poll(() => api.calls.some(call => call.method === "GetNetwork" && call.body.live === true && call.body.networkId === networkId && call.body.tenantId === "acme")).toBe(true);
  const networkResources = api.linkedNetwork.status!.architecture!.resources;
  const vpc = networkResources.find(resource => resource.kind === "VPC")!;
  await expect(canvas.getByRole("button", { name: `Inspect ${vpc.name}`, exact: true })).toBeVisible();
  await page.getByRole("button", { name: /fit view/i }).click();
  await settleDiagram(page, canvas);
  await assertGeometry(canvas);
  for (const excluded of ["vpc-cni", "metrics-server"]) await expect(canvas.getByRole("button", { name: `Inspect ${excluded}`, exact: true })).toHaveCount(0);
  const workers = canvas.getByRole("button", { name: "Inspect payments-workers", exact: true });
  await expect(workers).toHaveCount(1);
  const source = await canvas.locator(".react-flow__node").filter({ has: page.getByRole("button", { name: "Inspect payments-workers", exact: true }) }).getAttribute("data-id");
  expect(source).toBeTruthy();
  for (const subnet of api.privateSubnets.slice(0, 2)) {
    const inspectSubnet = canvas.getByRole("button", { name: `Inspect ${subnet.name}`, exact: true });
    await expect(inspectSubnet).toHaveCount(1);
    const target = await canvas.locator(".react-flow__node").filter({ has: page.getByRole("button", { name: `Inspect ${subnet.name}`, exact: true }) }).getAttribute("data-id");
    expect(target).toBeTruthy();
    // React Flow's edge ID retains the model's explicit source and target IDs.
    // Check actual rendered connections rather than deriving them from AZ names.
    const paths = await canvas.locator(".react-flow__edge").evaluateAll((edges, endpoints) => edges.filter(element => {
      try {
        const id: unknown = JSON.parse(element.getAttribute("data-id") ?? "null");
        return Array.isArray(id) && id.includes(endpoints.source) && id.includes(endpoints.target);
      } catch { return false; }
    }).length, { source, target });
    expect(paths, `one explicit workers-to-${subnet.name} path`).toBe(1);
  }
  const unrelated = api.privateSubnets[2];
  const unrelatedId = await canvas.locator(".react-flow__node").filter({ has: page.getByRole("button", { name: `Inspect ${unrelated.name}`, exact: true }) }).getAttribute("data-id");
  const invented = await canvas.locator(".react-flow__edge").evaluateAll((edges, endpoints) => edges.some(element => {
    try { const id: unknown = JSON.parse(element.getAttribute("data-id") ?? "null"); return Array.isArray(id) && id.includes(endpoints.source) && id.includes(endpoints.target); }
    catch { return false; }
  }), { source, target: unrelatedId });
  expect(invented, "third AZ does not imply a node-group subnet relationship").toBe(false);
  assertReadOnly(api);
});

test("inspection preserves managed names, actual AWS IDs, relationship details and viewer copy access", async ({ page }) => {
  const api = await mockClusterTopology(page), canvas = await openTopology(page);
  await settleDiagram(page, canvas);
  const inspect = canvas.getByRole("button", { name: "Inspect payments-workers", exact: true });
  await inspect.focus(); await page.keyboard.press("Enter");
  const inspector = page.getByLabel("Selected cluster topology resource", { exact: true });
  await expect(inspector).toContainText("payments-workers");
  await expect(inspector).toContainText(nodeGroupArn);
  for (const [label, value] of [["Instance types", "m6i.large"], ["Minimum nodes", "2"], ["Maximum nodes", "6"], ["Desired nodes", "Not reported"], ["Current nodes", "Not reported"], ["Initial desired nodes", "3"]]) {
    const detail = inspector.locator("dt").filter({ hasText: new RegExp(`^${label}$`) }).locator("xpath=following-sibling::dd[1]");
    await expect(detail.getByText(value, { exact: true })).toBeVisible();
  }
  await expect(inspector).toContainText("Create-time setting; not the current autoscaler target");
  const copyButtons = inspector.getByRole("button", { name: /^Copy / });
  await expect(copyButtons.first()).toBeVisible();
  for (const button of await copyButtons.all()) await button.click();
  await expect.poll(() => page.evaluate(() => (window as unknown as { topologyCopiedValues: string[] }).topologyCopiedValues)).toContain(nodeGroupArn);
  await expect.poll(() => page.evaluate(() => (window as unknown as { topologyCopiedValues: string[] }).topologyCopiedValues)).toContain("payments-workers");
  await expect(inspector.getByRole("button", { name: /^(Edit|Delete|Apply|Install|Freeze)\b/ })).toHaveCount(0);
  assertReadOnly(api);
});

test("reference-only targets remain unverified and retain their exact referenced IDs", async ({ page }) => {
  const api = await mockClusterTopology(page), canvas = await openTopology(page);
  await settleDiagram(page, canvas);
  await expect.poll(() => api.calls.some(call => call.method === "GetNetwork" && call.body.live === true)).toBe(true);
  await canvas.getByRole("button", { name: "Inspect external-security-group", exact: true }).click();
  const inspector = page.getByLabel("Selected cluster topology resource", { exact: true });
  await expect(inspector).toContainText("sg-cluster-reference");
  await expect(inspector).toContainText("Referenced target");
  await expect(inspector.getByText("Ready", { exact: true })).toHaveCount(0);
  assertReadOnly(api);
});

test("missing projection shows an explicit empty state without fabricating topology from inventory", async ({ page }) => {
  const api = await mockClusterTopology(page, { missingProjection: true });
  await openTopology(page);
  await expect(page.getByText("No cluster architecture reported", { exact: true })).toBeVisible();
  await expect(page.locator(".react-flow__node")).toHaveCount(0);
  await expect(page.locator(".react-flow__edge-path")).toHaveCount(0);
  assertReadOnly(api);
});

for (const mismatch of ["foreignTenant", "foreignId"] as const) test(`${mismatch} live projection is rejected without foreign resource disclosure`, async ({ page }) => {
  const api = await mockClusterTopology(page, { [mismatch]: true });
  await openTopology(page);
  await expect(page.getByText("Live architecture unavailable", { exact: true })).toBeVisible();
  await expect(page.getByText("Foreign topology", { exact: true })).toHaveCount(0);
  await expect(page.locator(".react-flow__node")).toHaveCount(0);
  await expect(page.locator(".react-flow__edge-path")).toHaveCount(0);
  assertReadOnly(api);
});

test("live failure retains safe cached detail without displaying an invented graph", async ({ page }) => {
  const api = await mockClusterTopology(page, { failLive: true });
  await openTopology(page);
  await expect(page.getByText("Live architecture unavailable", { exact: true })).toBeVisible();
  await expect(page.getByRole("heading", { level: 1, name: "Payments", exact: true })).toBeVisible();
  await expect(page.locator(".react-flow__node")).toHaveCount(0);
  assertReadOnly(api);
});

for (const theme of ["Light", "Dark"] as const) test(`${theme.toLowerCase()} fullscreen topology captures compute and optional network views`, async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 1920, height: 1200 });
  const api = await mockClusterTopology(page), canvas = await openTopology(page);
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  const settings = page.getByRole("dialog", { name: "Settings", exact: true });
  await settings.getByRole("radio", { name: theme, exact: true }).click();
  await settings.getByRole("button", { name: "Close", exact: true }).last().click();
  await settleDiagram(page, canvas);
  if (theme === "Dark") {
    const vpc = api.linkedNetwork.status!.architecture!.resources.find(resource => resource.kind === "VPC")!;
    await expect(canvas.getByRole("button", { name: `Inspect ${vpc.name}`, exact: true })).toBeVisible();
  }
  await page.getByRole("button", { name: "Enter fullscreen", exact: true }).click();
  await expect.poll(() => page.evaluate(() => document.fullscreenElement !== null)).toBe(true);
  await page.getByRole("button", { name: /fit view/i }).click();
  await settleDiagram(page, canvas);
  await assertGeometry(canvas);
  await expect(canvas).toHaveCSS("color-scheme", theme.toLowerCase());
  const path = testInfo.outputPath(`cluster-topology-${theme.toLowerCase()}.png`);
  await page.getByRole("region", { name: "Cluster topology diagram", exact: true }).screenshot({ path, animations: "disabled" });
  await testInfo.attach(`cluster-topology-${theme.toLowerCase()}`, { path, contentType: "image/png" });
  await page.keyboard.press("Escape");
  await expect.poll(() => page.evaluate(() => document.fullscreenElement === null)).toBe(true);
  assertReadOnly(api);
});

test("mobile inspector remains usable without horizontal page overflow", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  const api = await mockClusterTopology(page), canvas = await openTopology(page);
  await settleDiagram(page, canvas);
  await canvas.getByRole("button", { name: "Inspect payments-workers", exact: true }).focus();
  await page.keyboard.press("Enter");
  const inspector = page.getByRole("region", { name: "Selected cluster topology resource", exact: true });
  await expect(inspector).toBeVisible();
  const close = inspector.getByRole("button", { name: "Close inspector", exact: true });
  await close.scrollIntoViewIfNeeded();
  await expect(close).toBeInViewport();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth + 1)).toBe(true);
  await close.click();
  await expect(canvas).toBeVisible();
  assertReadOnly(api);
});

test("network inspection exposes subnet routes and selection-only NAT egress", async ({ page }) => {
  await page.setViewportSize({ width: 1920, height: 1200 });
  const api = await mockClusterTopology(page, { networkFixture: "C" }), canvas = await openTopology(page);
  await settleDiagram(page, canvas);
  const logical = canvas.locator('.react-flow__edge[aria-label*="nat internet egress"]');
  await expect(logical).toHaveCount(0);
  await canvas.getByRole("button", { name: `Inspect ${api.privateSubnets[1].name}`, exact: true }).click();
  const inspector = page.getByRole("region", { name: "Selected cluster topology resource", exact: true });
  await expect(inspector.getByRole("heading", { name: "Route entries", exact: true })).toBeVisible();
  await expect(inspector.getByRole("table", { name: /^Routes for / })).toContainText("0.0.0.0/0");
  await expect(logical).toHaveCount(1);
  await expect.poll(() => canvas.locator(".react-flow__edge-path").evaluateAll(paths => paths.some(path => Number(getComputedStyle(path).opacity) < 1))).toBe(true);
  await page.getByRole("button", { name: "Close inspector", exact: true }).click();
  await expect(logical).toHaveCount(0);
  assertReadOnly(api);
});
