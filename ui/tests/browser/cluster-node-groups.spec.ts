import { expect, test, type Locator, type Page } from "@playwright/test";

const networkId = "00000000-0000-4000-8000-000000000002", clusterId = "00000000-0000-4000-8000-000000000003";
type Call = { method: string; body: Record<string, unknown> };
type Options = { emptyStatus?: boolean; imported?: boolean; leftover?: boolean };

const nodeGroupStatus = [
  { name: "platform-pool", platformManaged: true, phase: "NODE_GROUP_PHASE_FAILED", message: "No capacity in the selected zones", nodes: 0 },
  { name: "system", platformManaged: true, phase: "NODE_GROUP_PHASE_READY", message: "", nodes: 2 },
];

test.beforeEach(async ({ page }) => {
  await page.route("**/api/auth/session", route => route.fulfill({ contentType: "application/json", body: JSON.stringify({ enabled: true, authenticated: true, csrfToken: "browser-test-csrf", username: "test-user" }) }));
});

async function mock(page: Page, options: Options = {}) {
  const calls: Call[] = [], unexpected: string[] = [];
  const network = { id: networkId, tenantId: "acme", name: "production", spec: { region: "us-west-2", tier: "TIER_1" } };
  const leftover = options.leftover ? [{ name: "batch", phase: "NODE_GROUP_PHASE_DELETING", message: "Karpenter is draining the nodes", nodes: 1 }] : [];
  const cluster = {
    id: clusterId, tenantId: "acme", name: "production", etag: "etag-1", generation: "1",
    spec: { region: "us-west-2", network: { networkId }, aws: {} },
    status: { state: "CLUSTER_STATE_RUNNING", observedGeneration: "1", nodeGroups: options.emptyStatus ? [] : [...nodeGroupStatus, ...leftover] },
  };
  // What the server renders: the system group (an EKS node group: no YAML), then the Karpenter platform pool.
  const manifests = options.imported ? [] : [{ name: "system", platformManaged: true }, {
    name: "platform-pool", platformManaged: true,
    ec2NodeClassYaml: "apiVersion: karpenter.k8s.aws/v1\nkind: EC2NodeClass\nmetadata:\n  name: platform-pool\nspec:\n  role: production-karpenter-node\n",
    nodePoolYaml: "apiVersion: karpenter.sh/v1\nkind: NodePool\nmetadata:\n  name: platform-pool\n",
  }];
  await page.route("**/api/rpc/**", async route => {
    const method = new URL(route.request().url()).pathname.split("/").at(-1)!;
    calls.push({ method, body: route.request().postDataJSON() as Record<string, unknown> });
    let response: object;
    switch (method) {
      case "WhoAmI": response = { anonymous: true }; break;
      case "ListTenants": response = { tenants: [{ id: "acme" }] }; break;
      case "GetCluster": response = { cluster, asRequested: true }; break;
      case "GetNetwork": response = { network }; break;
      case "ListClusterEvents": response = { events: [] }; break;
      case "ListClusterNodeGroupManifests": response = { nodeGroups: manifests }; break;
      default: unexpected.push(method); await route.fulfill({ status: 501, contentType: "application/json", body: JSON.stringify({ code: "unimplemented", message: `Unexpected RPC ${method}` }) }); return;
    }
    await route.fulfill({ contentType: "application/json", body: JSON.stringify(response) });
  });
  return { calls, unexpected };
}

async function open(page: Page) {
  await page.goto(`/clusters/${clusterId}?tab=node-groups`);
  return page.getByRole("tabpanel", { name: "Node groups", exact: true });
}
const row = (panel: Locator, name: string) => panel.getByRole("row").filter({ has: panel.page().getByRole("link", { name: `Open node group ${name}`, exact: true }) });

test("the system group and the platform pool are listed read-only and labelled as managed by the platform", async ({ page }) => {
  const api = await mock(page), panel = await open(page);
  const table = panel.getByRole("table", { name: "Cluster node groups", exact: true });
  await expect(table.getByRole("row")).toHaveCount(3); // header, system, platform-pool
  await expect(panel.getByText(/^Managed by the platform\. The system node group runs Karpenter/)).toBeVisible();
  for (const name of ["system", "platform-pool"]) {
    await expect(row(panel, name)).toContainText("Managed by the platform");
    await expect(row(panel, name).getByRole("link", { name: `Open node group ${name}`, exact: true })).toHaveAttribute("href", `/clusters/${clusterId}/node-groups/${name}`);
  }
  await expect(row(panel, "system")).toContainText("Ready");
  await expect(row(panel, "system")).toContainText("2");
  await expect(row(panel, "platform-pool")).toContainText("Failed");
  await expect(row(panel, "platform-pool")).toContainText("No capacity in the selected zones");
  // Nothing to create, edit or remove.
  await expect(panel.getByRole("button", { name: "Add node group", exact: true })).toHaveCount(0);
  await expect(panel.getByRole("button", { name: /^Actions for node group/ })).toHaveCount(0);
  await expect(table.getByRole("columnheader", { name: "Actions", exact: true })).toHaveCount(0);
  // The system group is an EKS node group, not Karpenter objects: no YAML to show for it.
  await expect(panel.getByRole("button", { name: "View YAML for node group system", exact: true })).toHaveCount(0);
  expect(api.calls.some(call => call.method === "UpdateCluster")).toBe(false);
  expect(api.unexpected).toEqual([]);
});

test("the system row is always shown, Pending until the controller reports it, and the platform pool appears from its manifest", async ({ page }) => {
  const api = await mock(page, { emptyStatus: true }), panel = await open(page);
  await expect(row(panel, "system")).toContainText("Managed by the platform");
  await expect(row(panel, "system")).toContainText("Pending — waiting for the controller");
  await expect(row(panel, "platform-pool")).toContainText("Pending");
  await expect(row(panel, "platform")).toHaveCount(0);
  expect(api.calls.some(call => call.method === "ListClusterNodeGroupManifests" && call.body.clusterId === clusterId)).toBe(true);
  expect(api.unexpected).toEqual([]);
});

test("a group an earlier layout left behind stays visible while it drains, without actions", async ({ page }) => {
  await mock(page, { leftover: true });
  const panel = await open(page);
  await expect(row(panel, "batch")).toContainText("Deleting");
  await expect(row(panel, "batch")).toContainText("Karpenter is draining the nodes");
  await expect(row(panel, "batch")).not.toContainText("Managed by the platform");
  await expect(panel.getByRole("button", { name: /^Actions for node group/ })).toHaveCount(0);
});

test("an imported cluster has no platform node group rows", async ({ page }) => {
  await mock(page, { emptyStatus: true, imported: true });
  const panel = await open(page);
  await expect(panel.getByRole("table", { name: "Cluster node groups", exact: true })).toContainText("Node groups will appear after the controller reports them.");
  await expect(row(panel, "system")).toHaveCount(0);
});

test("View YAML shows the platform pool's EC2NodeClass and NodePool, with copy", async ({ page }) => {
  const api = await mock(page), panel = await open(page);
  await panel.getByRole("button", { name: "View YAML for node group platform-pool", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Node group platform-pool YAML", exact: true });
  await expect(dialog.getByRole("textbox", { name: "EC2NodeClass YAML", exact: true })).toBeVisible();
  await expect(dialog.getByRole("textbox", { name: "NodePool YAML", exact: true })).toBeVisible();
  await expect(dialog).toContainText("kind: EC2NodeClass");
  await expect(dialog).toContainText("kind: NodePool");
  await expect(dialog.getByRole("button", { name: "Copy EC2NodeClass YAML", exact: true })).toBeVisible();
  await expect(dialog.getByRole("button", { name: "Copy NodePool YAML", exact: true })).toBeVisible();
  await dialog.getByRole("button", { name: "Close YAML", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  expect(api.unexpected).toEqual([]);
});

test("a narrow viewport keeps the node groups table free of horizontal overflow", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 812 });
  await mock(page);
  const panel = await open(page);
  await expect(row(panel, "platform-pool")).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)).toBeLessThanOrEqual(0);
});
