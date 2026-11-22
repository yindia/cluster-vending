import { toJsonString } from "@bufbuild/protobuf";
import { NetworkSchema } from "../../src/gen/cloud/v1/network_pb";
import { createNetworkTopologyFixture, networkTopologyFixtureIds, type NetworkTopologyFixtureId } from "../../src/lib/workflows/network-topology-fixtures";
import { expect, test, type Page } from "@playwright/test";

const accountId = "00000000-0000-4000-8000-000000000001";
const networkId = "00000000-0000-4000-8000-000000000002";
const clusterId = "00000000-0000-4000-8000-000000000003";
const secondClusterId = "00000000-0000-4000-8000-000000000004";
const readyResource = { kind: "VPC", name: "production-vpc", externalName: "vpc-topology-ready", state: "MANAGED_RESOURCE_STATE_READY", ready: "CONDITION_STATUS_TRUE", synced: "CONDITION_STATUS_TRUE", generation: "1", observedGeneration: "1" };
const pausedResource = { ...readyResource, zoneId: "usw1-az1", role: "private", kind: "Subnet", name: "private-subnet", externalName: "subnet-topology-paused", paused: true };
const failedResource = { ...readyResource, kind: "Route", name: "private-egress", externalName: "route-topology-error", state: "MANAGED_RESOURCE_STATE_FAILED", ready: "CONDITION_STATUS_FALSE", synced: "CONDITION_STATUS_FALSE", reason: "AccessDenied", message: "Route provisioning permission denied" };
const account = { id: accountId, tenantId: "acme", accountName: "Production account", aws: { accountId: "111122223333", role: "cluster-vending-provisioner" }, status: { state: "CLOUD_ACCOUNT_STATE_READY" } };
const publicSubnet = { ...readyResource, kind: "Subnet", name: "public-subnet", externalName: "subnet-topology-public", zoneId: "usw1-az1", role: "public" };
const gateway = { ...readyResource, kind: "InternetGateway", name: "internet-gateway", externalName: "igw-topology" };
const nat = { ...readyResource, kind: "NATGateway", name: "nat-gateway", externalName: "nat-topology", zoneId: "usw1-az1" };
const routeTable = { ...readyResource, kind: "RouteTable", name: "private-routes", externalName: "rtb-topology" };
const ancillary = { ...readyResource, kind: "Role", name: "flow-log-role", externalName: "flow-log-role" };
const publicSubnet2 = { ...publicSubnet, name: "public-subnet-b", externalName: "subnet-topology-public-b", zoneId: "usw1-az2" };
const privateSubnet2 = { ...readyResource, kind: "Subnet", name: "private-subnet-b", externalName: "subnet-topology-private-b", zoneId: "usw1-az2", role: "private" };
const publicRoutes = { ...readyResource, kind: "RouteTable", name: "public-routes", externalName: "rtb-public" };
const publicEgress = { ...readyResource, kind: "Route", name: "public-egress", externalName: "route-public" };
const privateRoutes2 = { ...routeTable, name: "private-routes-b", externalName: "rtb-private-b" };
const privateEgress2 = { ...readyResource, kind: "Route", name: "private-egress-b", externalName: "route-private-b" };
const address = { ...readyResource, kind: "EIP", name: "nat-address", externalName: "eipalloc-topology" };
const architectureResources = [
  { kind: readyResource.kind, name: readyResource.name, cloudId: readyResource.externalName, cidrBlock: "10.0.0.0/16" },
  { kind: publicSubnet.kind, name: publicSubnet.name, cloudId: publicSubnet.externalName, zoneId: "usw1-az1", subnetRole: "SUBNET_ROLE_PUBLIC", cidrBlock: "10.0.0.0/24" },
  { kind: pausedResource.kind, name: pausedResource.name, cloudId: pausedResource.externalName, zoneId: "usw1-az1", subnetRole: "SUBNET_ROLE_PRIVATE", cidrBlock: "10.0.10.0/24" },
  { kind: gateway.kind, name: gateway.name, cloudId: gateway.externalName },
  { kind: nat.kind, name: nat.name, cloudId: nat.externalName, zoneId: "usw1-az1" },
  { kind: routeTable.kind, name: routeTable.name, cloudId: routeTable.externalName },
  { kind: failedResource.kind, name: failedResource.name, cloudId: failedResource.externalName, destinationCidrBlock: "0.0.0.0/0" },
  { kind: publicSubnet2.kind, name: publicSubnet2.name, cloudId: publicSubnet2.externalName, zoneId: "usw1-az2", subnetRole: "SUBNET_ROLE_PUBLIC", cidrBlock: "10.0.1.0/24" },
  { kind: privateSubnet2.kind, name: privateSubnet2.name, cloudId: privateSubnet2.externalName, zoneId: "usw1-az2", subnetRole: "SUBNET_ROLE_PRIVATE", cidrBlock: "10.0.11.0/24" },
  ...[publicRoutes, privateRoutes2].map(resource => ({ kind: resource.kind, name: resource.name, cloudId: resource.externalName })),
  ...[publicEgress, privateEgress2].map(resource => ({ kind: resource.kind, name: resource.name, cloudId: resource.externalName, destinationCidrBlock: "0.0.0.0/0" })),
  { kind: address.kind, name: address.name, cloudId: address.externalName, publicIp: "198.51.100.42" },
];
const architectureConnections = [
  ...[routeTable, privateRoutes2, publicRoutes].map(resource => ({ sourceKind: "RouteTable", sourceName: resource.name, targetKind: "VPC", targetName: "production-vpc", relation: "contained_in", evidence: "observed" })),
  ...[publicSubnet2, privateSubnet2].map(resource => ({ sourceKind: "Subnet", sourceName: resource.name, targetKind: "VPC", targetName: "production-vpc", relation: "contained_in", evidence: "observed" })),
  ...[publicSubnet, publicSubnet2].map(resource => ({ sourceKind: "Subnet", sourceName: resource.name, targetKind: "RouteTable", targetName: "public-routes", relation: "routes_via", evidence: "observed" })),
  { sourceKind: "Route", sourceName: "public-egress", targetKind: "RouteTable", targetName: "public-routes", relation: "contained_in", evidence: "observed" },
  { sourceKind: "Route", sourceName: "public-egress", targetKind: "InternetGateway", targetName: "internet-gateway", relation: "forwards_to", label: "0.0.0.0/0", evidence: "observed" },
  { sourceKind: "Subnet", sourceName: "private-subnet-b", targetKind: "RouteTable", targetName: "private-routes-b", relation: "routes_via", evidence: "observed" },
  { sourceKind: "Route", sourceName: "private-egress-b", targetKind: "RouteTable", targetName: "private-routes-b", relation: "contained_in", evidence: "observed" },
  { sourceKind: "Route", sourceName: "private-egress-b", targetKind: "NATGateway", targetName: "nat-gateway", relation: "forwards_to", label: "0.0.0.0/0", evidence: "observed" },
  { sourceKind: "NATGateway", sourceName: "nat-gateway", targetKind: "EIP", targetName: "nat-address", relation: "uses_address", evidence: "observed" },
  { sourceKind: "Subnet", sourceName: "public-subnet", targetKind: "VPC", targetName: "production-vpc", relation: "contained_in", evidence: "observed" },
  { sourceKind: "Subnet", sourceName: "private-subnet", targetKind: "VPC", targetName: "production-vpc", relation: "contained_in", evidence: "observed" },
  { sourceKind: "InternetGateway", sourceName: "internet-gateway", targetKind: "VPC", targetName: "production-vpc", relation: "contained_in", evidence: "observed" },
  { sourceKind: "NATGateway", sourceName: "nat-gateway", targetKind: "Subnet", targetName: "public-subnet", relation: "placed_in", evidence: "observed" },
  { sourceKind: "Subnet", sourceName: "private-subnet", targetKind: "RouteTable", targetName: "private-routes", relation: "routes_via", evidence: "observed" },
  { sourceKind: "Route", sourceName: "private-egress", targetKind: "RouteTable", targetName: "private-routes", relation: "contained_in", evidence: "configured" },
  { sourceKind: "Route", sourceName: "private-egress", targetKind: "NATGateway", targetName: "nat-gateway", relation: "forwards_to", label: "0.0.0.0/0", evidence: "configured" },
];
const network = { id: networkId, tenantId: "acme", name: "production-network", displayName: "Production network", etag: "network-etag", generation: "1", spec: { bootstrapId: accountId, region: "us-west-1", tier: "TIER_1", layout: { cidrBlock: "10.0.0.0/16" } }, status: { state: "NETWORK_STATE_READY", observedGeneration: "1", clusterIds: [clusterId, secondClusterId], managedResources: [readyResource, publicSubnet, pausedResource, gateway, nat, routeTable, failedResource, publicSubnet2, privateSubnet2, publicRoutes, publicEgress, privateRoutes2, privateEgress2, address, ancillary], architecture: { resources: architectureResources, connections: architectureConnections } } };
const cluster = { id: clusterId, tenantId: "acme", name: "payments", generation: "1", etag: "cluster-etag", spec: { region: "us-west-1", network: { networkId } }, status: { state: "CLUSTER_STATE_RUNNING", observedGeneration: "1", managedResources: [] } };
type Call = { method: string; body: Record<string, unknown> };

test.beforeEach(async ({ page }) => {
  await page.route("**/api/auth/session", route => route.fulfill({ contentType: "application/json", body: JSON.stringify({ enabled: true, authenticated: true, csrfToken: "browser-test-csrf", username: "test-user" }) }));
});

const baselineNetwork = network;
async function mockTopology(page: Page, options: { failClusters?: boolean; failLive?: boolean; foreignLive?: boolean; fixture?: NetworkTopologyFixtureId } = {}) {
  let network = baselineNetwork;
  if (options.fixture) {
    const fixture = createNetworkTopologyFixture(options.fixture).network;
    fixture.id = networkId; fixture.tenantId = "acme"; fixture.spec!.bootstrapId = accountId;
    network = JSON.parse(toJsonString(NetworkSchema, fixture)) as typeof baselineNetwork;
  }
  const calls: Call[] = [], unexpected: string[] = [];
  await page.route("**/api/rpc/**", async route => {
    const method = new URL(route.request().url()).pathname.split("/").at(-1)!;
    const body = route.request().postDataJSON() as Record<string, unknown>;
    calls.push({ method, body });
    let response: object;
    switch (method) {
      case "WhoAmI": response = { subject: "topology-viewer" }; break;
      case "ListTenants": response = { tenants: [{ id: "acme" }], canCreateTenants: false }; break;
      case "ListCloudAccounts": response = { accounts: [account] }; break;
      case "GetCloudAccount": response = { account }; break;
      case "GetNetwork":
        if (body.live && options.failLive) {
          await route.fulfill({ status: 503, contentType: "application/json", body: JSON.stringify({ code: "unavailable", message: "Live architecture temporarily unavailable" }) }); return;
        }
        response = { network: body.live ? options.foreignLive ? { ...network, id: "foreign-network-id", tenantId: "foreign", displayName: "Foreign architecture" } : network : { ...network, status: { ...network.status, architecture: undefined } }, asRequested: true }; break;
      case "ListNetworks": response = { networks: [network] }; break;
      case "GetCluster": response = { cluster, asRequested: true }; break;
      case "ListNetworkEvents": case "ListClusterEvents": response = { events: [] }; break;
      case "ListClusters":
        if (options.failClusters) {
          await route.fulfill({ status: 503, contentType: "application/json", body: JSON.stringify({ code: "unavailable", message: "Topology cluster inventory unavailable" }) }); return;
        }
        response = body.pageToken ? { clusters: [{ ...cluster, id: secondClusterId, name: "analytics", status: { ...cluster.status, managedResources: [] } }] } : {
          clusters: [cluster, { ...cluster, id: "00000000-0000-4000-8000-000000000005", name: "unrelated-cluster", spec: { ...cluster.spec, network: { networkId: "00000000-0000-4000-8000-000000000099" } } }, { ...cluster, id: "00000000-0000-4000-8000-000000000006", tenantId: "foreign", name: "foreign-tenant-cluster" }], nextPageToken: "topology-page-2",
        }; break;
      default:
        unexpected.push(method);
        await route.fulfill({ status: 501, contentType: "application/json", body: JSON.stringify({ code: "unimplemented", message: `Unexpected RPC ${method}` }) }); return;
    }
    await route.fulfill({ contentType: "application/json", body: JSON.stringify(response) });
  });
  return { calls, unexpected };
}

async function openTopology(page: Page) {
  await page.goto(`/networks/${networkId}`);
  await page.getByRole("tab", { name: "Topology", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Network architecture", exact: true })).toBeVisible();
  const graph = page.getByRole("region", { name: "Network topology diagram", exact: true });
  await expect(graph).toBeVisible();
  return graph;
}

function assertReadOnly(api: Awaited<ReturnType<typeof mockTopology>>) {
  expect(api.unexpected).toEqual([]);
  expect(api.calls.filter(({ method }) => !/^(WhoAmI|List|Get)/.test(method))).toEqual([]);
  expect(api.calls.filter(({ method }) => method === "ListClusters").every(({ body }) => body.tenantId === "acme")).toBe(true);
}


async function settleDiagram(page: Page) {
  await page.evaluate(() => document.fonts.ready);
  await expect(page.locator('.react-flow__node')).not.toHaveCount(0);
  await expect(page.getByText('Routing unavailable', { exact: true })).toHaveCount(0);
  await expect(page.locator('.react-flow__edge-path').first()).toBeVisible();
  // ResizeObserver measures content in the live theme before the final layout.
  await expect.poll(async () => {
    const first = await page.locator('.react-flow__viewport').getAttribute('style');
    await page.waitForTimeout(100);
    return first === await page.locator('.react-flow__viewport').getAttribute('style');
  }).toBe(true);
}

async function assertRenderedGeometry(page: Page) {
  const errors = await page.locator('.react-flow').evaluate(root => {
    const errors: string[] = [];
    const overlaps = (a: DOMRect, b: DOMRect) => a.left + 1 < b.right && a.right - 1 > b.left && a.top + 1 < b.bottom && a.bottom - 1 > b.top;
    const contains = (a: DOMRect, b: DOMRect) => a.left <= b.left + 1 && a.right >= b.right - 1 && a.top <= b.top + 1 && a.bottom >= b.bottom - 1;
    const nodes = [...root.querySelectorAll<HTMLElement>('.react-flow__node')];
    const canvas = root.getBoundingClientRect();
    if (!nodes.length) errors.push('empty diagram');
    for (const node of nodes) {
      const rect = node.getBoundingClientRect(), style = getComputedStyle(node);
      if (rect.width <= 1 || rect.height <= 1 || !Number.isFinite(rect.x) || style.visibility === 'hidden' || Number(style.opacity) === 0) errors.push('node not painted');
      if (rect.right < canvas.left || rect.left > canvas.right || rect.bottom < canvas.top || rect.top > canvas.bottom) errors.push('node outside canvas after fit');
    }

    for (let i = 0; i < nodes.length; i++) for (let j = i + 1; j < nodes.length; j++) {
      const a = nodes[i].getBoundingClientRect(), b = nodes[j].getBoundingClientRect();
      if (overlaps(a, b) && !contains(a, b) && !contains(b, a)) errors.push(`node overlap ${nodes[i].dataset.id} / ${nodes[j].dataset.id}`);
    }
    const obstacles = [...root.querySelectorAll('.react-flow__node-resource, .react-flow__node-external, [data-architecture-title]')].map(el => el.getBoundingClientRect());
    for (const group of root.querySelectorAll<HTMLElement>('[data-architecture-boundary]')) {
      const header = group.querySelector<HTMLElement>('[data-architecture-header]')!;
      const outer = group.getBoundingClientRect(), box = header.getBoundingClientRect();
      if (Math.abs(box.width - outer.width) > 2) errors.push('header does not fill container');
      if (box.left < outer.left - 1 || box.right > outer.right + 1) errors.push('header exceeds container');
    }
    const labels = [...root.querySelectorAll<SVGGraphicsElement>('.react-flow__edge-textbg')];
    for (let i = 0; i < labels.length; i++) {
      const box = labels[i].getBoundingClientRect();
      if (obstacles.some(obstacle => overlaps(box, obstacle))) errors.push('label overlaps resource/header');
      for (let j = i + 1; j < labels.length; j++) if (overlaps(box, labels[j].getBoundingClientRect())) errors.push('labels overlap');
    }
    for (const path of root.querySelectorAll<SVGPathElement>('.react-flow__edge-path')) {
      const matrix = path.getScreenCTM(); if (!matrix) continue;
      for (let distance = 1; distance < path.getTotalLength(); distance += 3) {
        const p = path.getPointAtLength(distance).matrixTransform(matrix);
        if (obstacles.some(r => p.x > r.left + 1 && p.x < r.right - 1 && p.y > r.top + 1 && p.y < r.bottom - 1)) { errors.push('edge intersects resource/header'); break; }
      }
    }
    return errors;
  });
  expect(errors).toEqual([]);
}

test('compact architecture keeps route tables visible, EIPs inspectable and clusters in the header', async ({ page }) => {
  const api = await mockTopology(page), graph = await openTopology(page);
  await settleDiagram(page);
  const owner = graph.locator('[data-architecture-boundary="account"]');
  await expect(owner).toContainText('Production account');
  await expect(owner).toContainText('111122223333');
  await expect(owner.getByRole('button', { name: 'Copy AWS account ID', exact: true })).toBeVisible();
  await graph.getByRole('button', { name: /fit view/i }).click();
  for (const title of ['private-routes', 'public-routes', 'production-vpc', 'nat-gateway']) await expect(graph.getByRole('button', { name: `Inspect ${title}`, exact: true })).toBeVisible();
  await expect(graph.locator('.react-flow__node').filter({ hasText: 'Uses this network' })).toHaveCount(0);
  await expect(page.getByRole('link', { name: 'payments', exact: true })).toHaveAttribute('href', `/clusters/${clusterId}`);
  await expect(page.getByRole('link', { name: 'analytics', exact: true })).toHaveAttribute('href', `/clusters/${secondClusterId}`);
  await expect(graph.getByRole('button', { name: /Inspect (unrelated-cluster|foreign-tenant-cluster)/ })).toHaveCount(0);
  await graph.getByRole('button', { name: 'Inspect 198.51.100.42', exact: true }).click();
  const inspector = page.getByRole('region', { name: 'Selected topology resource', exact: true });
  await expect(inspector).toContainText('eipalloc-topology');
  await expect(inspector.getByRole('button', { name: /^Copy / }).first()).toBeVisible();
  await expect(page.getByRole('checkbox', { name: 'Routing details', exact: true })).toHaveCount(0);
  expect(api.calls.some(({ method, body }) => method === 'ListClusters' && body.pageToken === 'topology-page-2')).toBe(true);
  assertReadOnly(api);
});

test('subnet and NAT inspection traces actual route dependencies and dims unrelated edges', async ({ page }) => {
  const api = await mockTopology(page), graph = await openTopology(page);
  await settleDiagram(page); await graph.getByRole('button', { name: /fit view/i }).click();
  await graph.getByRole('button', { name: 'Inspect private-subnet', exact: true }).click();
  const inspector = page.getByRole('region', { name: 'Selected topology resource', exact: true });
  await expect(inspector).toContainText('private-routes');
  await expect(inspector).toContainText('0.0.0.0/0');
  await expect.poll(() => graph.locator('.react-flow__edge-path').evaluateAll(paths => paths.some(p => Number(getComputedStyle(p).opacity) < 0.5) && paths.some(p => Number(getComputedStyle(p).opacity) === 1))).toBe(true);
  await graph.getByRole('button', { name: 'Inspect nat-gateway', exact: true }).click();
  await expect(inspector).toContainText('private-routes-b');
  await expect(inspector).toContainText('private-routes');
  await expect(inspector.getByRole('button', { name: /^(Edit|Delete|Freeze|Install)\b/ })).toHaveCount(0);
  assertReadOnly(api);
});

for (const fixture of networkTopologyFixtureIds) test(`topology ${fixture}: rendered geometry and stable resource layout`, async ({ page }) => {
  await page.setViewportSize({ width: 1920, height: 1200 });
  const api = await mockTopology(page, { fixture }), graph = await openTopology(page);
  await settleDiagram(page);
  await graph.getByRole('button', { name: 'Enter fullscreen', exact: true }).click();
  await graph.getByRole('button', { name: /fit view/i }).click();
  await settleDiagram(page);
  await assertRenderedGeometry(page);
  await expect(graph.locator('[data-architecture-boundary="zone"]')).toHaveCount(createNetworkTopologyFixture(fixture).expected.zoneIds.length);
  await expect(graph.locator('.react-flow__edge-text')).not.toContainText(['Association']);
  if (['A', 'C', 'D', 'I'].includes(fixture)) {
    await graph.screenshot({ path: `/tmp/cv-network-${fixture}-architecture.png`, animations: 'disabled' });
    await expect(graph).toHaveScreenshot(`network-${fixture}-architecture.png`, { animations: 'disabled', maxDiffPixelRatio: 0.005 });
  }
  await page.keyboard.press('Escape');
  await expect.poll(() => page.evaluate(() => document.fullscreenElement === null)).toBe(true);
  assertReadOnly(api);
});

test('local route controls are absent and full resource names are available on hover and inspection', async ({ page }) => {
  const api = await mockTopology(page, { fixture: 'A' }), graph = await openTopology(page);
  await settleDiagram(page);
  await expect(page.getByRole('checkbox', { name: /Local VPC routes/ })).toHaveCount(0);
  await expect(graph.getByRole('button', { name: 'Inspect VPC local network', exact: true })).toHaveCount(0);
  await graph.getByRole('button', { name: /fit view/i }).click();
  await settleDiagram(page);
  const table = graph.getByRole('button', { name: 'Inspect fixture-a-private-1-table', exact: true });
  await table.hover();
  await expect(page.getByRole('tooltip').filter({ hasText: 'fixture-a-private-1-table' }).first()).toBeVisible();
  await table.click();
  await expect(page.getByRole('region', { name: 'Selected topology resource', exact: true })).toContainText('fixture-a-private-1-table');
  assertReadOnly(api);
});

for (const foreignLive of [false, true]) test(foreignLive ? 'foreign live snapshot rejected' : 'live failure preserves cached inventory', async ({ page }) => {
  const api = await mockTopology(page, { foreignLive, failLive: !foreignLive }), graph = await openTopology(page);
  await expect(page.getByText('Live architecture unavailable', { exact: true })).toBeVisible();
  await expect(page.getByRole('table', { name: 'Network topology inventory', exact: true })).toHaveCount(0);
  await expect(graph).toContainText('resources');
  await expect(graph.getByText('Foreign architecture', { exact: true })).toHaveCount(0);
  assertReadOnly(api);
});

test('mobile controls and inspector remain within viewport', async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 812 });
  const api = await mockTopology(page), graph = await openTopology(page);
  await settleDiagram(page);
  await graph.getByRole('button', { name: /fit view/i }).click();
  const node = graph.getByRole('button', { name: 'Inspect production-vpc', exact: true });
  await node.focus(); await page.keyboard.press('Enter');
  await expect(page.getByRole('region', { name: 'Selected topology resource', exact: true })).toContainText('vpc-topology-ready');
  await expect(page.getByRole('button', { name: 'Close resource inspection', exact: true })).toBeInViewport();
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(375);
  await page.screenshot({ path: '/tmp/cv-network-architecture-mobile.png', fullPage: true, animations: 'disabled' });
  assertReadOnly(api);
});


test('dark architecture preserves contrast and full-width headers', async ({ page }) => {
  await page.setViewportSize({ width: 1920, height: 1200 });
  await mockTopology(page, { fixture: 'D' });
  const graph = await openTopology(page);
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  const settings = page.getByRole('dialog', { name: 'Settings', exact: true });
  await settings.getByRole('radio', { name: 'Dark', exact: true }).click();
  await settings.getByRole('button', { name: 'Close', exact: true }).last().click();
  await expect(graph).toHaveCSS('color-scheme', 'dark');
  await settleDiagram(page);
  await graph.getByRole('button', { name: 'Enter fullscreen', exact: true }).click();
  await graph.getByRole('button', { name: /fit view/i }).click();
  await settleDiagram(page); await assertRenderedGeometry(page);
  await graph.screenshot({ path: '/tmp/cv-network-D-dark.png', animations: 'disabled' });
  await expect(graph).toHaveScreenshot('network-D-dark.png', { animations: 'disabled', maxDiffPixelRatio: 0.005 });
  await expect(page.getByRole('button', { name: 'Topology inventory', exact: true })).toHaveCount(0);
});


test('security overlay is optional and distinguishes configuration from exposure', async ({ page }) => {
  await mockTopology(page, { fixture: 'C' });
  const graph = await openTopology(page); await settleDiagram(page);
  const toggle = graph.getByRole('checkbox', { name: 'Security overlay', exact: true });
  await expect(toggle).not.toBeChecked();
  await expect(page.getByRole('region', { name: 'Network security observations', exact: true })).toHaveCount(0);
  await toggle.check();
  const observations = page.getByRole('region', { name: 'Network security observations', exact: true });
  await expect(observations).toContainText('exposure not evaluated');
  await expect(observations).toContainText('Cross-zone route reference');
  await expect(observations).toContainText('Public address association');
  await expect(observations).toContainText('Internet gateway default route');
  await toggle.uncheck();
  await expect(observations).toHaveCount(0);
});
