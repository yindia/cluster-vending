import { expect, test, type Page } from "@playwright/test";

const clusterId = "00000000-0000-4000-8000-000000000003";
const subtenantId = "00000000-0000-4000-8000-000000000004";
test.beforeEach(async ({ page }) => {
  await page.clock.install();
  await page.route("**/api/auth/session", (route) => route.fulfill({ contentType: "application/json", body: JSON.stringify({ enabled: true, authenticated: true, csrfToken: "browser-test-csrf", username: "test-user" }) }));
});
async function mockStatus(page: Page, terminal = "SUB_TENANT_STATE_READY") {
  const calls: { method: string; body: Record<string, unknown> }[] = [], unexpected: string[] = [];
  let phase = "FREEZE_PHASE_UNFROZEN", revision = "initial", updated = false, observed = false;
  const allocation = () => ({ tenantId: "acme", clusterId, id: subtenantId, etag: updated ? "updated-etag" : "original-etag", generation: updated ? "2" : "1", spec: { name: "payments", displayName: updated ? "Updated payments" : "Payments", namespaceKeys: ["app"], policyProfile: "restricted-ambient-v1", quota: { namespaceCount: 1, hard: { pods: "10" } } }, status: { state: terminal, observedGeneration: updated && observed ? "2" : "1", message: observed ? "Current generation checked" : "Previous generation verdict" } });
  await page.route("**/api/rpc/**", async (route) => {
    const method = new URL(route.request().url()).pathname.split("/").at(-1)!;
    const body = route.request().postDataJSON() as Record<string, unknown>;
    calls.push({ method, body });
    let response: object;
    switch (method) {
      case "WhoAmI": response = { anonymous: true }; break;
      case "ListTenants": response = { tenants: [{ id: "acme" }] }; break;
      case "GetFreeze": response = { status: { target: body.target, phase, revision, totalResources: 1, pausedResources: phase === "FREEZE_PHASE_FROZEN" ? 1 : 0 } }; break;
      case "Freeze": phase = "FREEZE_PHASE_FREEZING"; revision = "accepted"; response = { revision }; break;
      case "GetCluster": response = { cluster: { tenantId: "acme", id: clusterId, name: "production", spec: { naas: true }, status: { state: "CLUSTER_STATE_RUNNING" } } }; break;
      case "ListClusterEvents": response = { events: [] }; break;
      case "ListSubTenants": response = { subtenants: [allocation()] }; break;
      case "GetSubTenant": response = { subtenant: allocation() }; break;
      case "UpdateSubTenant": updated = true; response = { subtenant: allocation() }; break;
      default: unexpected.push(method); await route.fulfill({ status: 501, contentType: "application/json", body: JSON.stringify({ code: "unimplemented", message: `Unexpected RPC ${method}` }) }); return;
    }
    await route.fulfill({ contentType: "application/json", body: JSON.stringify(response) });
  });
  return { calls, unexpected, count: (method: string) => calls.filter((call) => call.method === method).length, converge: () => { phase = "FREEZE_PHASE_FROZEN"; }, expire: () => { phase = "FREEZE_PHASE_UNFROZEN"; revision = "expired"; }, observe: () => { observed = true; } };
}
async function liveUpdates(page: Page, enabled: boolean) {
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  await page.getByRole("dialog", { name: "Settings", exact: true }).getByRole("checkbox", { name: "Live updates", exact: true }).setChecked(enabled);
  await page.getByRole("dialog", { name: "Settings", exact: true }).getByRole("button", { name: "Close", exact: true }).last().click();
}
async function freeze(page: Page) {
  await page.getByRole("textbox", { name: /^Reason\b/ }).fill("Maintenance");
  await page.getByRole("button", { name: "Review freeze", exact: true }).click();
  await page.getByRole("textbox", { name: "Type exactly: tenant acme", exact: true }).fill("tenant acme");
  await page.getByRole("button", { name: "Confirm freeze for tenant acme", exact: true }).click();
  await expect(page.getByText("Request accepted; awaiting convergence", { exact: true })).toBeVisible();
}
for (const enabled of [false, true]) {
  test(`freeze observation converges with Live updates ${enabled ? "on and continues until switched off" : "off and then stops"}`, async ({ page }) => {
    const mock = await mockStatus(page);
    await page.goto("/freeze"); await page.getByRole("button", { name: "Inspect selected scope", exact: true }).click();
    await expect(page.getByText("Unfrozen", { exact: true })).toBeVisible();
    const idle = mock.count("GetFreeze"); await page.clock.runFor(6_000); expect(mock.count("GetFreeze")).toBe(idle);
    if (enabled) await liveUpdates(page, true);
    await freeze(page); mock.converge(); await page.clock.runFor(5_500);
    await expect(page.getByText("Convergence confirmed", { exact: true })).toBeVisible();
    await expect(page.getByText("Frozen", { exact: true })).toBeVisible();
    await page.clock.runFor(500); const complete = mock.count("GetFreeze");
    mock.expire(); await page.clock.runFor(6_000);
    if (enabled) {
      await expect(page.getByText("Unfrozen", { exact: true })).toBeVisible();
      expect(mock.count("GetFreeze")).toBeGreaterThan(complete);
      await expect(page.getByText("Convergence confirmed", { exact: true })).toBeVisible();
      await expect(page.getByRole("button", { name: "Review freeze", exact: true })).toBeEnabled();
      await liveUpdates(page, false); await page.clock.runFor(500); const stopped = mock.count("GetFreeze");
      await page.clock.runFor(11_000); expect(mock.count("GetFreeze")).toBe(stopped);
    } else {
      expect(mock.count("GetFreeze")).toBe(complete);
      await expect(page.getByText("Frozen", { exact: true })).toBeVisible();
      await page.getByRole("button", { name: "Refresh freeze status", exact: true }).click(); await expect(page.getByText("Unfrozen", { exact: true })).toBeVisible();
    }
    expect(mock.unexpected).toEqual([]);
  });
}
for (const [terminal, label] of [["READY", "Ready"], ["FAILED", "Failed"], ["DEGRADED", "Degraded"]]) {
  test(`stale ${label} is pending in inventory, detail and update banner until the new generation is observed`, async ({ page }) => {
    const mock = await mockStatus(page, `SUB_TENANT_STATE_${terminal}`);
    await page.goto(`/clusters/${clusterId}`); await page.getByRole("tab", { name: "Sub Tenants", exact: true }).click();
    const table = page.getByRole("table", { name: "Cluster Sub Tenants", exact: true });
    await expect(table.getByRole("cell", { name: label, exact: true })).toBeVisible();
    await table.getByRole("button", { name: `Actions for Sub Tenant Payments (${subtenantId})`, exact: true }).click(); await page.getByRole("menuitem", { name: "Edit", exact: true }).click();
    const edit = page.getByRole("dialog", { name: "Edit Sub Tenant", exact: true });
    await edit.getByRole("textbox", { name: /^Display name\b/ }).fill("Updated payments"); await edit.getByRole("button", { name: "Next", exact: true }).click(); await edit.getByRole("button", { name: "Review Sub Tenant", exact: true }).click(); await edit.getByRole("button", { name: "Apply Sub Tenant update", exact: true }).click(); await expect(edit).toHaveCount(0);
    await expect(table.getByRole("cell", { name: "Awaiting observation", exact: true })).toBeVisible();
    await expect(table.getByRole("cell", { name: label, exact: true })).toHaveCount(0);
    await expect(table).not.toContainText("Previous generation verdict");
    await expect(page.getByText("Sub Tenant update saved · Awaiting observation", { exact: true })).toBeVisible();
    await table.getByRole("button", { name: `Actions for Sub Tenant Updated payments (${subtenantId})`, exact: true }).click(); await page.getByRole("menuitem", { name: "View", exact: true }).click();
    const detail = page.getByRole("dialog", { name: "Sub Tenant details", exact: true }); await expect(detail).toContainText("Awaiting observation"); await expect(detail).not.toContainText("Previous generation verdict"); await detail.getByRole("button", { name: "Close", exact: true }).last().click();
    mock.observe(); await page.clock.runFor(2_500);
    await expect(table.getByRole("cell", { name: label, exact: true })).toBeVisible(); await expect(table).toContainText("Current generation checked");
    const done = mock.count("GetSubTenant"); await page.clock.runFor(8_000); expect(mock.count("GetSubTenant")).toBe(done);
    expect(mock.calls.find((call) => call.method === "UpdateSubTenant")!.body.updateMask).toBe("spec.displayName");
    expect(mock.unexpected).toEqual([]);
  });
}
