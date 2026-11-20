import { expect, test, type Page } from "@playwright/test";

const clusterId = "00000000-0000-4000-8000-000000000003", networkId = "00000000-0000-4000-8000-000000000002";
const COLLECTED = "2026-11-19T10:00:00Z";
const TOTAL = 1000;
type ListRequest = { nodeGroup: string; pageSize?: number; pageToken?: string; filter?: string; nameContains?: string };
type Options = { empty?: boolean; incomplete?: boolean; unavailable?: boolean; failNextToken?: boolean };

// Every fifth node is not ready: 1,000 nodes, 200 of them not ready. Names sort as numbers.
const nodeName = (group: string, i: number) => `${group}-node-${String(i).padStart(4, "0")}`;
const nodeOf = (group: string, i: number) => ({
  name: nodeName(group, i), instanceType: "m7i.large", zone: ["us-east-1a", "us-east-1b"][i % 2], capacityType: i % 3 ? "on-demand" : "spot", architecture: "amd64",
  kubeletVersion: "v1.37.1-eks", ready: i % 5 !== 0, conditions: i % 5 === 0 ? ["MemoryPressure"] : [], taintsCount: 1,
  createdAt: "2026-11-18T10:00:00Z", nodeclaimName: `${group}-claim-${i}`, nodeclaimPhase: "Initialized", nodeclaimReady: i % 5 !== 0, providerId: `aws:///us-east-1a/i-${i}`,
});

test.beforeEach(async ({ page }) => {
  await page.route("**/api/auth/session", route => route.fulfill({ contentType: "application/json", body: JSON.stringify({ enabled: true, authenticated: true, csrfToken: "browser-test-csrf", username: "test-user" }) }));
});

async function mock(page: Page, options: Options = {}) {
  const lists: ListRequest[] = [], unexpected: string[] = [];
  let tokenFailed = false;
  const cluster = {
    id: clusterId, tenantId: "acme", name: "production", etag: "etag-1", generation: "1",
    spec: { region: "us-west-2", network: { networkId }, defaultNodePool: { instanceTypes: ["t3.large"], scalingConfig: { minSize: 2, maxSize: 2 } }, aws: {},
      nodeGroups: [{ name: "batch", instanceFamilies: ["m7i"], architectures: ["ARCHITECTURE_AMD64"], capacityTypes: ["CAPACITY_TYPE_SPOT"], labels: { team: "data" } }] },
    status: { state: "CLUSTER_STATE_RUNNING", observedGeneration: "1", nodeGroups: [
      { name: "system", platformManaged: true, phase: "NODE_GROUP_PHASE_READY", nodes: 2 },
      { name: "batch", phase: "NODE_GROUP_PHASE_READY", nodes: TOTAL },
    ] },
  };
  // The system group is an EKS managed node group: its entry has no YAML.
  const manifests = [{ name: "system", platformManaged: true }, { name: "batch", platformManaged: false, ec2NodeClassYaml: "kind: EC2NodeClass\nmetadata:\n  name: batch\n", nodePoolYaml: "kind: NodePool\nmetadata:\n  name: batch\n" }];
  await page.route("**/api/rpc/**", async route => {
    const method = new URL(route.request().url()).pathname.split("/").at(-1)!;
    const body = route.request().postDataJSON();
    let response: object;
    switch (method) {
      case "WhoAmI": response = { anonymous: true }; break;
      case "ListTenants": response = { tenants: [{ id: "acme" }] }; break;
      case "GetCluster": response = { cluster, asRequested: true }; break;
      case "GetNetwork": response = { network: { id: networkId, tenantId: "acme", name: "production", spec: { region: "us-west-2", tier: "TIER_1" } } }; break;
      case "ListClusterEvents": response = { events: [] }; break;
      case "ListClusterNodeGroupManifests": response = { nodeGroups: manifests }; break;
      case "ListNodeGroupNodes": {
        const req = body as ListRequest;
        lists.push(req);
        if (options.unavailable) { response = { notices: ["results unavailable: access to the results bucket was denied"], complete: false }; break; }
        if (options.empty) { response = { notices: ["the node list has not been collected yet; the controller collects it while it reconciles the cluster's node groups"] }; break; }
        if (options.failNextToken && req.pageToken && !tokenFailed) {
          tokenFailed = true;
          await route.fulfill({ status: 400, contentType: "application/json", body: JSON.stringify({ code: "invalid_argument", message: "page_token does not match this result; restart the list" }) });
          return;
        }
        // The server filters, then pages: offset tokens, a page of at most 100.
        const all = Array.from({ length: req.nodeGroup === "batch" ? TOTAL : 2 }, (_, i) => nodeOf(req.nodeGroup, i))
          .filter(node => req.filter === "NODE_STATUS_FILTER_NOT_READY" ? !node.ready : req.filter === "NODE_STATUS_FILTER_READY" ? node.ready : true)
          .filter(node => !req.nameContains || node.name.includes(req.nameContains.toLowerCase()));
        const start = Number(req.pageToken ?? 0), size = Math.min(req.pageSize ?? 100, 500), end = Math.min(start + size, all.length);
        response = { nodes: all.slice(start, end), nextPageToken: end < all.length ? String(end) : "", total: all.length, collectedAt: COLLECTED, complete: !options.incomplete, notices: options.incomplete ? ["the node list is incomplete: the controller stopped collecting before it had read every node"] : [] };
        break;
      }
      default: unexpected.push(method); await route.fulfill({ status: 501, contentType: "application/json", body: JSON.stringify({ code: "unimplemented", message: `Unexpected RPC ${method}` }) }); return;
    }
    await route.fulfill({ contentType: "application/json", body: JSON.stringify(response) });
  });
  return { lists, unexpected };
}

const nodesTable = (page: Page) => page.getByRole("table", { name: /^Nodes of node group/ });
const bodyRows = (page: Page) => nodesTable(page).getByRole("row").filter({ hasNot: page.getByRole("columnheader") });

test("a node group's header and summary, and its first page of 100 of 1,000 nodes", async ({ page }) => {
  const api = await mock(page);
  await page.goto(`/clusters/${clusterId}/node-groups/batch`);

  await expect(page.getByRole("heading", { level: 1, name: "batch" })).toBeVisible();
  await expect(page.getByRole("region", { name: "Node group details" })).toContainText("Spot");
  await expect(page.getByRole("region", { name: "Node group details" })).toContainText("m7i family");
  await expect(page.getByRole("button", { name: "View YAML for node group batch" })).toBeVisible();
  await expect(page.getByText("Collected 2026-11-19 10:00:00 UTC")).toBeVisible();
  await expect(bodyRows(page)).toHaveCount(100);
  await expect(page.getByRole("status", { name: "Node range" })).toHaveText("1–100 of 1,000");
  await expect(bodyRows(page).first()).toContainText("batch-node-0000");
  await expect(bodyRows(page).first()).toContainText("Not ready");
  await expect(bodyRows(page).first()).toContainText("MemoryPressure");
  await expect(bodyRows(page).nth(1)).toContainText("Ready");
  expect(api.lists[0]).toMatchObject({ nodeGroup: "batch", pageSize: 100 });
  expect(api.lists[0].pageToken ?? "").toBe("");
  expect(api.unexpected).toEqual([]);
});

test("paging is by the server: ten pages of 100, each one request", async ({ page }) => {
  const api = await mock(page);
  await page.goto(`/clusters/${clusterId}/node-groups/batch`);
  await expect(bodyRows(page)).toHaveCount(100);
  await expect(page.getByRole("button", { name: "Previous nodes page" })).toBeDisabled();

  for (let pageNumber = 2; pageNumber <= 10; pageNumber++) {
    await page.getByRole("button", { name: "Next nodes page" }).click();
    await expect(page.getByRole("status", { name: "Node range" })).toHaveText(`${((pageNumber - 1) * 100 + 1).toLocaleString("en-US")}–${(pageNumber * 100).toLocaleString("en-US")} of 1,000`);
    await expect(bodyRows(page)).toHaveCount(100);
    await expect(bodyRows(page).first()).toContainText(nodeName("batch", (pageNumber - 1) * 100));
  }
  await expect(page.getByRole("button", { name: "Next nodes page" })).toBeDisabled();
  const tokens = api.lists.map(list => list.pageToken ?? "");
  // One request per later page; the first page may be asked twice (the dev server mounts effects twice).
  expect(tokens.filter(Boolean)).toEqual(["100", "200", "300", "400", "500", "600", "700", "800", "900"]);
  expect(tokens.filter(token => !token).length).toBeLessThanOrEqual(2);
  expect(api.lists.every(list => list.pageSize === 100)).toBe(true);

  await page.getByRole("button", { name: "Previous nodes page" }).click();
  await expect(page.getByRole("status", { name: "Node range" })).toHaveText("801–900 of 1,000");
  await expect(bodyRows(page).first()).toContainText("batch-node-0800");
});

test("the status filter is the server's, and starts again at the first page", async ({ page }) => {
  const api = await mock(page);
  await page.goto(`/clusters/${clusterId}/node-groups/batch`);
  await page.getByRole("button", { name: "Next nodes page" }).click();
  await expect(page.getByRole("status", { name: "Node range" })).toHaveText("101–200 of 1,000");

  await page.getByRole("radio", { name: "Not ready" }).click();
  await expect(page.getByRole("status", { name: "Node range" })).toHaveText("1–100 of 200");
  await expect(bodyRows(page)).toHaveCount(100);
  await expect(bodyRows(page).filter({ hasText: "Not ready" })).toHaveCount(100);
  expect(api.lists.at(-1)).toMatchObject({ filter: "NODE_STATUS_FILTER_NOT_READY" });
  expect(api.lists.at(-1)?.pageToken ?? "").toBe("");

  await page.getByRole("radio", { name: "Ready", exact: true }).click();
  await expect(page.getByRole("status", { name: "Node range" })).toHaveText("1–100 of 800");
  await expect(bodyRows(page).filter({ hasText: "Not ready" })).toHaveCount(0);
  await page.getByRole("radio", { name: "All" }).click();
  await expect(page.getByRole("status", { name: "Node range" })).toHaveText("1–100 of 1,000");
});

test("name search is the server's too", async ({ page }) => {
  const api = await mock(page);
  await page.goto(`/clusters/${clusterId}/node-groups/batch`);
  await expect(bodyRows(page)).toHaveCount(100);

  await page.getByRole("searchbox", { name: "Search nodes" }).fill("node-0042");
  await expect(page.getByRole("status", { name: "Node range" })).toHaveText("1–1 of 1");
  await expect(bodyRows(page)).toHaveCount(1);
  await expect(bodyRows(page).first()).toContainText("batch-node-0042");
  expect(api.lists.at(-1)).toMatchObject({ nameContains: "node-0042" });

  await page.getByRole("searchbox", { name: "Search nodes" }).fill("zzz");
  await expect(page.getByText("No nodes match this filter.")).toBeVisible();
  await page.getByRole("searchbox", { name: "Search nodes" }).fill("");
  await expect(page.getByRole("status", { name: "Node range" })).toHaveText("1–100 of 1,000");
});

test("a list that changed under the token starts again with a notice", async ({ page }) => {
  await mock(page, { failNextToken: true });
  await page.goto(`/clusters/${clusterId}/node-groups/batch`);
  await expect(bodyRows(page)).toHaveCount(100);
  await page.getByRole("button", { name: "Next nodes page" }).click();
  await expect(page.getByText("The node list changed")).toBeVisible();
  await expect(page.getByRole("status", { name: "Node range" })).toHaveText("1–100 of 1,000");
});

test("nodes not collected yet is its own state", async ({ page }) => {
  await mock(page, { empty: true });
  await page.goto(`/clusters/${clusterId}/node-groups/batch`);
  await expect(page.getByText("Nodes have not been collected yet.")).toBeVisible();
  await expect(page.getByText("Not collected yet", { exact: true })).toBeVisible();
  await expect(page.getByText("the node list has not been collected yet;", { exact: false })).toBeVisible();
  await expect(page.getByText("batch-node-")).toHaveCount(0);
});

test("an unreadable results bucket is a notice, and an incomplete collection says so", async ({ page }) => {
  await mock(page, { unavailable: true });
  await page.goto(`/clusters/${clusterId}/node-groups/batch`);
  await expect(page.getByText("results unavailable: access to the results bucket was denied")).toBeVisible();
  await expect(page.getByRole("heading", { level: 1, name: "batch" })).toBeVisible();

  await page.unroute("**/api/rpc/**");
  await mock(page, { incomplete: true });
  await page.reload();
  await expect(page.getByText("the node list is incomplete")).toBeVisible();
  await expect(bodyRows(page)).toHaveCount(100);
});

test("the system group opens, marked as managed by the platform, with its nodes; there is no platform group", async ({ page }) => {
  const api = await mock(page);
  await page.goto(`/clusters/${clusterId}/node-groups/system`);
  await expect(page.getByRole("heading", { level: 1, name: "system" })).toBeVisible();
  await expect(page.getByText("Managed by platform", { exact: true })).toBeVisible();
  await expect(page.getByText("t3.large")).toBeVisible();
  await expect(page.getByText("2–2 (autoscaled)")).toBeVisible();
  await expect(page.getByRole("button", { name: /View YAML/ })).toHaveCount(0);
  await expect(bodyRows(page)).toHaveCount(2);
  expect(api.unexpected).toEqual([]);

  await page.goto(`/clusters/${clusterId}/node-groups/platform`);
  await expect(page.getByText("Node group not found")).toBeVisible();
});

test("a name the cluster does not have is not found", async ({ page }) => {
  await mock(page);
  await page.goto(`/clusters/${clusterId}/node-groups/ghost`);
  await expect(page.getByText("Node group not found")).toBeVisible();
});

test("View YAML opens the group's manifests", async ({ page }) => {
  await mock(page);
  await page.goto(`/clusters/${clusterId}/node-groups/batch`);
  await page.getByRole("button", { name: "View YAML for node group batch" }).click();
  await expect(page.getByRole("dialog", { name: "Node group batch YAML" })).toBeVisible();
});

test("the Node groups tab links every group to its page", async ({ page }) => {
  await mock(page);
  await page.goto(`/clusters/${clusterId}`);
  await page.getByRole("tab", { name: "Infrastructure", exact: true }).click();
  const panel = page.getByRole("tabpanel", { name: "Node groups", exact: true });
  for (const name of ["system", "batch"]) await expect(panel.getByRole("link", { name: `Open node group ${name}` })).toHaveAttribute("href", `/clusters/${clusterId}/node-groups/${name}`);
  await panel.getByRole("link", { name: "Open node group batch" }).click();
  await expect(page).toHaveURL(new RegExp(`/clusters/${clusterId}/node-groups/batch$`));
  await expect(page.getByRole("heading", { level: 1, name: "batch" })).toBeVisible();
  await page.getByRole("link", { name: "Cluster node groups" }).click();
  await expect(page.getByRole("tabpanel", { name: "Node groups", exact: true })).toBeVisible();
});

test("a narrow viewport does not overflow the page", async ({ page }) => {
  await mock(page);
  for (const width of [320, 375, 768]) {
    await page.setViewportSize({ width, height: 800 });
    await page.goto(`/clusters/${clusterId}/node-groups/batch`);
    await expect(bodyRows(page)).toHaveCount(100);
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
    expect(overflow, `horizontal overflow at ${width}px`).toBeLessThanOrEqual(0);
    await expect(page.getByRole("button", { name: "Next nodes page" })).toBeVisible();
    await expect(bodyRows(page).first()).toContainText("batch-node-0000");
  }
});
