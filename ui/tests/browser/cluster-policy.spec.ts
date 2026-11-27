import { expect, test, type Locator, type Page } from "@playwright/test";

const clusterId = "00000000-0000-4000-8000-000000000003";
const otherClusterId = "00000000-0000-4000-8000-000000000004";
const networkId = "00000000-0000-4000-8000-000000000002";
type Call = { method: string; body: Record<string, unknown> };
type Fixture = { violations?: object; events?: object; fail?: "ListClusterViolations" | "ListClusterViolationEvents"; pages?: Record<string, object>; byCluster?: Record<string, object> };
const clusterRow = (id: string, name: string) => ({ id, tenantId: "acme", name, spec: { region: "us-west-2", network: { networkId }, aws: {} }, status: { state: "CLUSTER_STATE_RUNNING" } });
async function mockPolicy(page: Page, fixture: Fixture = {}) {
  const calls: Call[] = [], unexpected: string[] = [];
  await page.route("**/api/auth/session", route => route.fulfill({ contentType: "application/json", body: JSON.stringify({ enabled: false, authenticated: false }) }));
  await page.route("**/api/rpc/**", async route => {
    const method = new URL(route.request().url()).pathname.split("/").at(-1)!;
    const body = route.request().postDataJSON() as Record<string, unknown>;
    calls.push({ method, body });
    let response: object;
    switch (method) {
      case "WhoAmI": response = { subject: "policy-viewer" }; break;
      case "ListTenants": response = { tenants: [{ id: "acme" }] }; break;
      case "GetCluster": response = { cluster: body.clusterId === otherClusterId ? clusterRow(otherClusterId, "billing") : clusterRow(clusterId, "payments"), asRequested: true }; break;
      case "ListClusters": response = { clusters: [clusterRow(clusterId, "payments"), clusterRow(otherClusterId, "billing")] }; break;
      case "GetNetwork": response = { network: { id: networkId, tenantId: "acme", name: "production", spec: { region: "us-west-2" }, status: { state: "NETWORK_STATE_READY" } } }; break;
      case "ListNetworks": response = { networks: [{ id: networkId, tenantId: "acme", name: "production", spec: { region: "us-west-2" }, status: { state: "NETWORK_STATE_READY" } }] }; break;
      case "ListClusterEvents": response = { events: [] }; break;
      case "ListClusterViolations":
      case "ListClusterViolationEvents":
        if (fixture.fail === method) { await route.fulfill({ status: 503, contentType: "application/json", body: JSON.stringify({ code: "unavailable", message: "Policy snapshot temporarily unavailable" }) }); return; }
        response = method === "ListClusterViolations"
          ? fixture.byCluster?.[String(body.clusterId)] ?? fixture.pages?.[String(body.pageToken ?? "")] ?? fixture.violations ?? {}
          : fixture.events ?? {};
        break;
      default: unexpected.push(method); await route.fulfill({ status: 501, contentType: "application/json", body: JSON.stringify({ code: "unimplemented", message: `Unexpected RPC ${method}` }) }); return;
    }
    await route.fulfill({ contentType: "application/json", body: JSON.stringify(response) });
  });
  return { calls, unexpected };
}
async function openPolicy(page: Page, section: "Current violations" | "Violation events") {
  await page.goto(`/clusters/${clusterId}?tab=violations`);
  return page.getByRole("tabpanel", { name: "Policy violations", exact: true }).getByRole("group", { name: section, exact: true });
}
function readOnly(api: Awaited<ReturnType<typeof mockPolicy>>) {
  expect(api.unexpected).toEqual([]);
  const reads = api.calls.filter(call => ["ListClusterViolations", "ListClusterViolationEvents"].includes(call.method));
  expect(reads.length).toBeGreaterThan(0);
  expect(reads.every(call => call.body.tenantId === "acme" && [clusterId, otherClusterId].includes(String(call.body.clusterId)))).toBe(true);
  expect(api.calls.every(call => /^(Get|List|WhoAmI)/.test(call.method))).toBe(true);
}

for (const section of ["Current violations", "Violation events"] as const) test(`${section} reports an RPC failure while the other section stays available`, async ({ page }) => {
  const findingsFailed = section === "Current violations";
  const api = await mockPolicy(page, {
    fail: findingsFailed ? "ListClusterViolations" : "ListClusterViolationEvents",
    violations: { summary }, events: { summary },
  });
  const panel = await openPolicy(page, section);
  const other = page.getByRole("group", { name: findingsFailed ? "Violation events" : "Current violations", exact: true });
  await expect(other).toContainText("Recorded snapshot");
  await expect(panel).toContainText("Policy snapshot temporarily unavailable");
  await expect(panel.locator('input[type="date"], input[type="datetime-local"]')).toHaveCount(0);
  readOnly(api);
});

const summary = { collectedAt: "2026-11-19T02:00:00Z", notices: [], incomplete: false, truncated: false };
const finding = (id: string, name: string, namespace: string, result: string) => ({
  id, engine: "kyverno", policy: { apiVersion: "policies.kyverno.io/v1", kind: "ValidatingPolicy", name: "require-resource-limits" },
  resource: { apiVersion: "v1", kind: "Pod", namespace, name }, rule: "resource-limits", result, severity: "high", enforcement: "unknown",
  message: `Resource ${name} must declare memory and CPU limits before deployment`, observedAt: "2026-11-19T01:55:00Z",
});
const findings = [finding("one", "payment-worker", "payments", "fail"), finding("two", "invoice-worker", "billing", "warn")];

test("violations expose real report findings with search, filters and explicit refresh", async ({ page }) => {
  const api = await mockPolicy(page, { violations: { summary, violations: findings } });
  const panel = await openPolicy(page, "Current violations");
  const table = panel.getByRole("table", { name: "Cluster violations", exact: true });
  await expect(table).toContainText("payment-worker");
  await expect(table).toContainText("invoice-worker");
  await expect(panel.getByRole("button", { name: "Kind All kinds", exact: true })).toBeVisible();
  const filters = panel.getByRole("search", { name: "Violation filters" });
  expect(await filters.evaluate(row => row.scrollWidth <= row.clientWidth + 1)).toBe(true);
  const bottoms = await filters.locator('input[type="search"], button[aria-haspopup]').evaluateAll(
    controls => controls.map(control => control.getBoundingClientRect().bottom),
  );
  expect(bottoms.length).toBe(7);
  expect(Math.max(...bottoms) - Math.min(...bottoms)).toBeLessThan(5);
  await panel.getByRole("searchbox", { name: "Search violations", exact: true }).fill("invoice-worker");
  await expect(table).toContainText("invoice-worker");
  await expect(table).not.toContainText("payment-worker");
  const readsBeforeRefresh = api.calls.filter(call => call.method === "ListClusterViolations").length;
  const eventReads = api.calls.filter(call => call.method === "ListClusterViolationEvents").length;
  await page.getByRole("button", { name: "Refresh policy violations and events", exact: true }).click();
  await expect.poll(() => api.calls.filter(call => call.method === "ListClusterViolations").length).toBe(readsBeforeRefresh + 1);
  await expect.poll(() => api.calls.filter(call => call.method === "ListClusterViolationEvents").length).toBe(eventReads + 1);
  await expect(table).not.toContainText("payment-worker");
  await expect(panel.locator('input[type="date"]')).toHaveCount(0);
  readOnly(api);
});

test("incomplete report reads keep coverage notices instead of a clean-compliance claim", async ({ page }) => {
  const api = await mockPolicy(page, { violations: { summary: { ...summary, incomplete: true, notices: ["PolicyReports cannot be listed: access forbidden"] }, violations: [] } });
  const panel = await openPolicy(page, "Current violations");
  await expect(panel).toContainText("PolicyReports cannot be listed: access forbidden");
  await expect(panel).toContainText("Incomplete policy coverage");
  await expect(panel).toContainText("Coverage");
  await expect(panel).toContainText("Limited");
  await expect(panel).toContainText("does not establish that no violations occurred");
  await expect(panel).not.toContainText("Snapshot is stale");
  readOnly(api);
});

test("one Violations tab shows both current findings and retained events", async ({ page }) => {
  const api = await mockPolicy(page, { violations: { summary, violations: findings }, events: { summary, retentionNotice: "Events expire from Kubernetes; this is recent history, not an audit log.", events: [{
    id: "kyverno/denial-1", engine: "kyverno", policy: findings[0].policy, resource: findings[0].resource,
    message: "Admission denied: resource limits are required", reason: "PolicyViolation", action: "Resource Blocked", enforcement: "enforce",
    lastObservedAt: "2026-11-19T01:59:00Z", count: 3,
  }] } });
  const panel = await openPolicy(page, "Violation events");
  await expect(panel.getByRole("table", { name: "Cluster violation events", exact: true })).toContainText("Admission denied");
  await expect(panel).toContainText("not an audit log");
  await expect(page.getByRole("tab", { name: "Violation events", exact: true })).toHaveCount(0);
  const combined = page.getByRole("tabpanel", { name: "Policy violations", exact: true });
  await expect(combined.getByRole("table", { name: "Cluster violations", exact: true })).toContainText("invoice-worker");
  await combined.getByRole("searchbox", { name: "Search violations", exact: true }).fill("no-match");
  await expect(combined.getByText("No retained rows match your search and filters.")).toBeVisible();
  await expect(panel.getByRole("table")).toContainText("Admission denied");
  readOnly(api);
});

async function expectTableFits(table: Locator) {
  // The table can overflow its own viewport without widening the document.
  const excess = await table.evaluate(el => {
    const panel = el.closest('[role="group"]')!;
    const widths = [el, ...Array.from(el.querySelectorAll("td"))].map(node => node.scrollWidth - node.clientWidth);
    for (let ancestor = el.parentElement; ancestor && ancestor !== panel; ancestor = ancestor.parentElement) {
      widths.push(ancestor.scrollWidth - ancestor.clientWidth);
    }
    return Math.max(...widths, el.getBoundingClientRect().width - panel.clientWidth);
  });
  expect(excess).toBeLessThanOrEqual(1);
}

test("both policy tables and their filters fit the panel with long unbroken values", async ({ page }, testInfo) => {
  const name = "require-security-context-" + "a".repeat(100);
  const resourceName = "account-reconciliation-" + "b".repeat(100);
  const message = "Invalid configuration " + "c".repeat(280);
  const item = { ...findings[0], policy: { ...findings[0].policy, name }, resource: { ...findings[0].resource, name: resourceName }, message };
  const api = await mockPolicy(page, { violations: { summary, violations: [item] }, events: { summary, events: [
    { id: "ev-1", policy: item.policy, resource: item.resource, message, engine: "kyverno", reason: "PolicyViolation", count: 3, action: "Blocked" },
  ] } });
  const panel = await openPolicy(page, "Current violations");
  const eventPanel = page.getByRole("group", { name: "Violation events", exact: true });
  for (const width of [1440, 900, 640, 390, 320]) {
    await page.setViewportSize({ width, height: 844 });
    for (const section of [panel, eventPanel]) {
      const table = section.getByRole("table");
      await expect(table).toContainText(resourceName);
      await expectTableFits(table);
      expect(await section.getByRole("search").evaluate(el => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);
    }
  }
  const filters = panel.getByRole("search", { name: "Violation filters" });
  const toggle = filters.getByRole("button", { name: "Filters", exact: true });
  await toggle.click();
  await expect(filters.getByRole("button", { name: "Engine All engines" })).toBeVisible();
  expect(await filters.evaluate(el => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);
  await toggle.click();
  const eventTable = eventPanel.getByRole("table");
  await eventTable.getByRole("button", { name }).click();
  const detail = page.getByRole("dialog", { name: "Violation event details" });
  await expect(detail).toContainText("Blocked");
  await expect(detail).toContainText(resourceName);
  await expect(detail.getByRole("button", { name: "Copy Policy" })).toBeVisible();
  expect(await detail.evaluate(el => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);
  await page.keyboard.press("Escape");
  await expect(detail).toHaveCount(0);
  await expect(eventTable.getByRole("button", { name })).toBeFocused();
  await page.setViewportSize({ width: 390, height: 844 });
  await panel.screenshot({ path: testInfo.outputPath("cluster-violations-mobile.png") });
  readOnly(api);
});

for (const theme of ["Light", "Dark"] as const) test(`${theme} violation table visual review`, async ({ page }, testInfo) => {
  const api = await mockPolicy(page, { violations: { summary, violations: findings } });
  const panel = await openPolicy(page, "Current violations");
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  const settings = page.getByRole("dialog", { name: "Settings", exact: true });
  await settings.getByRole("radio", { name: theme, exact: true }).click();
  await settings.getByRole("button", { name: "Close", exact: true }).last().click();
  await expect(panel.getByRole("table", { name: "Cluster violations", exact: true })).toContainText("payment-worker");
  await panel.getByRole("table", { name: "Cluster violations", exact: true }).scrollIntoViewIfNeeded();
  await panel.screenshot({ path: testInfo.outputPath(`cluster-violations-${theme.toLowerCase()}.png`) });
  readOnly(api);
});

test("uncollected snapshots show unavailable instead of zero violations", async ({ page }) => {
  const api = await mockPolicy(page, { violations: { summary: { incomplete: true, notices: ["Workload policy collection unavailable"] }, violations: [] } });
  const panel = await openPolicy(page, "Current violations");
  await expect(panel).toContainText("Collection unavailable");
  await expect(panel).toContainText("Results will appear after collection becomes available.");
  await expect(panel).not.toContainText("0 retained findings");
  readOnly(api);
});

test("stale snapshots show a distinct banner instead of incomplete coverage", async ({ page }) => {
  const api = await mockPolicy(page, { violations: { summary: { ...summary, stale: true, notices: ["Policy snapshot is stale; the controller has not collected recently."] }, violations: findings } });
  const panel = await openPolicy(page, "Current violations");
  await expect(panel).toContainText("Snapshot is stale — last collected 2026-11-19 02:00:00.000 UTC");
  await expect(panel).not.toContainText("Incomplete policy coverage");
  await expect(panel).not.toContainText("Some policy sources could not be read");
  await expect(panel.getByRole("table", { name: "Cluster violations", exact: true })).toContainText("payment-worker");
  readOnly(api);
});

test("every stored page is loaded before filtering and counting", async ({ page }) => {
  const api = await mockPolicy(page, { pages: {
    "": { summary, violations: [findings[0]], nextPageToken: "page-2" },
    "page-2": { summary, violations: [findings[1]], nextPageToken: "" },
  } });
  const panel = await openPolicy(page, "Current violations");
  const table = panel.getByRole("table", { name: "Cluster violations", exact: true });
  await expect(table).toContainText("payment-worker");
  await expect(table).toContainText("invoice-worker");
  await expect(panel.getByRole("status").filter({ hasText: "2 retained findings" })).toBeVisible();
  const tokens = api.calls.filter(call => call.method === "ListClusterViolations").map(call => call.body.pageToken ?? "");
  expect(tokens).toContain("");
  expect(tokens).toContain("page-2");
  readOnly(api);
});

test("switching cluster drops the previous cluster's rows and filters", async ({ page }) => {
  const other = { ...findings[0], id: "other", message: "Resource ledger-worker lacks limits", resource: { ...findings[0].resource, name: "ledger-worker" } };
  const api = await mockPolicy(page, { byCluster: {
    [clusterId]: { summary, violations: findings },
    [otherClusterId]: { summary, violations: [other] },
  } });
  const panel = await openPolicy(page, "Current violations");
  await expect(panel.getByRole("table", { name: "Cluster violations", exact: true })).toContainText("payment-worker");
  await panel.getByRole("searchbox", { name: "Search violations", exact: true }).fill("payment");
  await page.getByRole("link", { name: "Clusters", exact: true }).first().click();
  await page.getByRole("link", { name: "billing", exact: true }).click();
  await page.getByRole("tab", { name: "Operations", exact: true }).click();
  await page.getByRole("tab", { name: "Policy violations", exact: true }).click();
  const next = page.getByRole("tabpanel", { name: "Policy violations", exact: true }).getByRole("group", { name: "Current violations", exact: true });
  await expect(next.getByRole("table", { name: "Cluster violations", exact: true })).toContainText("ledger-worker");
  await expect(next).not.toContainText("payment-worker");
  await expect(next.getByRole("searchbox", { name: "Search violations", exact: true })).toHaveValue("");
  readOnly(api);
});

test("long messages stay readable without hover", async ({ page }) => {
  const long = "Resource must declare limits ".repeat(12).trim() + " END-OF-MESSAGE";
  const api = await mockPolicy(page, { violations: { summary, violations: [{ ...findings[0], message: long }] } });
  const panel = await openPolicy(page, "Current violations");
  await expect(panel.getByRole("table", { name: "Cluster violations", exact: true })).toContainText("END-OF-MESSAGE");
  await panel.getByRole("button", { name: findings[0].policy.name }).click();
  const detail = page.getByRole("dialog", { name: "Violation details", exact: true });
  await expect(detail.getByText(long, { exact: true })).toBeVisible();
  await expect(detail).toContainText("resource-limits");
  await expect(detail.getByRole("button", { name: "Copy Resource" })).toBeVisible();
  readOnly(api);
});

test("cluster sections keep their URLs across reload, history and mobile navigation", async ({ page }) => {
  const api = await mockPolicy(page, { violations: { summary }, events: { summary } });
  await page.goto(`/clusters/${clusterId}?source=inventory`);
  const nav = page.getByRole("navigation", { name: "Cluster navigation" });
  await expect(nav.getByRole("tablist", { name: "Cluster sections" }).getByRole("tab")).toHaveCount(5);
  await expect(page.getByRole("table", { name: "Cluster health conditions" })).toBeVisible();
  expect(api.calls.filter(call => call.method === "ListClusterEvents")).toHaveLength(0);
  await nav.getByRole("tab", { name: "Operations", exact: true }).click();
  await nav.getByRole("tab", { name: "Policy violations", exact: true }).click();
  await expect(page).toHaveURL(/source=inventory&tab=violations$/);
  await page.reload();
  await expect(page.getByRole("tabpanel", { name: "Policy violations" })).toBeVisible();
  await page.goBack();
  await expect(page.getByRole("tabpanel", { name: "Activity" })).toBeVisible();
  await page.goForward();
  await expect(nav.getByRole("tab", { name: "Policy violations" })).toHaveAttribute("aria-selected", "true");
  await page.setViewportSize({ width: 390, height: 844 });
  const section = nav.getByRole("combobox", { name: "Cluster section" });
  await expect(section).toBeInViewport();
  await section.click();
  await page.getByRole("option", { name: "Overview", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Cluster summary" })).toBeInViewport();
  await expect(page).toHaveURL(/\?source=inventory$/);
  await expect(page.getByRole("button", { name: "Actions", exact: true })).toHaveCount(0);
  expect(api.unexpected).toEqual([]);
});
