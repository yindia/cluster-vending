import { expect, test, type Page } from "@playwright/test";

test.beforeEach(async ({ page }) => {
  await page.clock.install();
  await page.route("**/api/auth/session", (route) => route.fulfill({ contentType: "application/json", body: JSON.stringify({ enabled: true, authenticated: true, csrfToken: "browser-test-csrf", username: "test-user" }) }));
});
async function mockStatus(page: Page) {
  const calls: { method: string; body: Record<string, unknown> }[] = [], unexpected: string[] = [];
  let phase = "FREEZE_PHASE_UNFROZEN", revision = "initial";
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
      default: unexpected.push(method); await route.fulfill({ status: 501, contentType: "application/json", body: JSON.stringify({ code: "unimplemented", message: `Unexpected RPC ${method}` }) }); return;
    }
    await route.fulfill({ contentType: "application/json", body: JSON.stringify(response) });
  });
  return { calls, unexpected, count: (method: string) => calls.filter((call) => call.method === method).length, converge: () => { phase = "FREEZE_PHASE_FROZEN"; }, expire: () => { phase = "FREEZE_PHASE_UNFROZEN"; revision = "expired"; } };
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
