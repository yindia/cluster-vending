import { expect, test, type Locator, type Page } from "@playwright/test";

type RpcCall = { method: string; body: Record<string, unknown>; authorization: string };

/** Every RPC is intercepted; stale directory results deliberately omit created tenants. */
async function mockTenantHeader(page: Page, options: { returnedId?: string; pauseCreate?: boolean } = {}) {
  const calls: RpcCall[] = [];
  const unexpected: string[] = [];
  let identityChanged = false;
  let grantsRevoked = false;
  let releaseCreate: () => void = () => {};
  const pendingCreate = options.pauseCreate ? new Promise<void>((resolve) => { releaseCreate = resolve; }) : undefined;
  let finishCreate: () => void = () => {};
  const createResponded = new Promise<void>((resolve) => { finishCreate = resolve; });
  await page.route("**/api/rpc/**", async (route) => {
    const request = route.request();
    const method = new URL(request.url()).pathname.split("/").at(-1)!;
    const body = request.postDataJSON() as Record<string, unknown>;
    const authorization = request.headers().authorization ?? "";
    const viewer = authorization === "Bearer viewer-token" || identityChanged || grantsRevoked;
    calls.push({ method, body, authorization });
    let response: object;
    switch (method) {
      case "WhoAmI": response = { subject: identityChanged ? "different-operator" : "same-operator", issuer: "test", grants: [{ role: viewer ? "ACCESS_ROLE_TENANT_VIEWER" : "ACCESS_ROLE_TENANT_ADMIN", tenants: viewer ? ["acme"] : ["acme", "other", "new-team"] }] }; break;
      case "ListTenants": response = { tenants: viewer ? [{ id: "acme" }] : [{ id: "acme" }, { id: "other" }], canCreateTenants: !viewer }; break;
      case "CreateTenant":
        if (pendingCreate) await pendingCreate;
        response = { tenant: { id: options.returnedId ?? body.tenantId, createTime: "2026-11-16T00:00:00Z" } };
        break;
      case "ListCloudAccounts": response = { accounts: [] }; break;
      case "ListNetworks": response = { networks: [] }; break;
      case "ListClusters": response = { clusters: [] }; break;
      case "ListReleaseChannels": response = { channels: [] }; break;
      case "ListReleases": response = { releases: [] }; break;
      default:
        unexpected.push(method);
        await route.fulfill({ status: 501, contentType: "application/json", body: JSON.stringify({ code: "unimplemented", message: `Unmocked RPC: ${method}` }) });
        return;
    }
    // A canceled fetch can outlive its intercepted, deliberately delayed response.
    if (options.pauseCreate && method === "CreateTenant") {
      await route.fulfill({ contentType: "application/json", body: JSON.stringify(response) }).catch(() => {}).finally(finishCreate);
    } else await route.fulfill({ contentType: "application/json", body: JSON.stringify(response) });
  });
  return { calls, unexpected, releaseCreate, createResponded, changeIdentity: () => { identityChanged = true; }, revokeGrants: () => { grantsRevoked = true; } };
}

function tenantControl(page: Page) {
  // Astryx versions expose the selector trigger as either a combobox or a named button.
  return page.getByRole("combobox", { name: /^Tenant\b/ }).or(page.getByRole("button", { name: /^Tenant\b/ })).first();
}

async function box(control: Locator) {
  const bounds = await control.boundingBox();
  expect(bounds).not.toBeNull();
  return bounds!;
}

async function createTenant(page: Page) {
  await page.getByRole("button", { name: "Create tenant", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Create tenant", exact: true });
  await dialog.getByRole("textbox", { name: /^Tenant ID\b/ }).fill("new-team");
  await dialog.getByRole("button", { name: "Review tenant", exact: true }).click();
  await page.getByRole("dialog", { name: "Review tenant", exact: true }).getByRole("button", { name: "Confirm create tenant", exact: true }).click();
}

test("tenant header aligns input and actions on desktop and wraps accessible controls on mobile", async ({ page }) => {
  const mock = await mockTenantHeader(page);
  await page.goto("/releases");
  const tenant = tenantControl(page);
  const create = page.getByRole("button", { name: "Create tenant", exact: true });
  const settings = page.getByRole("button", { name: "Settings", exact: true });
  await expect(create).toBeVisible();
  for (const width of [1440, 768, 375]) {
    await page.setViewportSize({ width, height: 1000 });
    await expect(async () => {
      const controls = await Promise.all([box(tenant), box(create), box(settings)]);
      for (const control of [tenant, create, settings]) await expect(control).toBeInViewport({ ratio: 1 });
      for (const bounds of controls) { expect(bounds.x).toBeGreaterThanOrEqual(0); expect(bounds.x + bounds.width).toBeLessThanOrEqual(width); }
      const center = (bounds: typeof controls[number]) => bounds.y + bounds.height / 2;
      expect(Math.abs(center(controls[1]) - center(controls[2]))).toBeLessThan(2);
      if (width >= 768) expect(Math.abs(center(controls[0]) - center(controls[1]))).toBeLessThan(2);
      const label = page.getByText("Tenant", { exact: true }).filter({ visible: true }).first();
      expect(Math.abs(center(await box(label)) - center(controls[0]))).toBeLessThan(2);
      expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
    }).toPass();
    await page.screenshot({ path: `/tmp/cv-tenant-header-${width}.png`, fullPage: true });
  }
  expect(mock.unexpected).toEqual([]);
});

test("a confirmed created tenant stays selectable across stale directory refreshes, remounts and tenant switches", async ({ page }) => {
  const mock = await mockTenantHeader(page);
  await page.goto("/");
  await createTenant(page);
  await expect(page.getByRole("dialog")).toHaveCount(0);
  await expect(tenantControl(page)).toContainText("new-team");
  await expect(page.getByRole("button", { name: "Create tenant", exact: true })).toBeVisible();
  await tenantControl(page).click();
  await expect(page.getByRole("option", { name: "new-team", exact: true })).toBeEnabled();
  await page.getByRole("option", { name: "acme", exact: true }).click();
  await expect(tenantControl(page)).toContainText("acme");
  await page.getByRole("link", { name: "Releases", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Release inventory", exact: true })).toBeVisible();
  const directoryCalls = mock.calls.filter((call) => call.method === "ListTenants").length;
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  await page.getByRole("dialog", { name: "Settings", exact: true }).getByRole("button", { name: "Connect", exact: true }).click();
  await expect.poll(() => mock.calls.filter((call) => call.method === "ListTenants").length).toBeGreaterThan(directoryCalls);
  await expect(page.getByRole("button", { name: "Create tenant", exact: true })).toBeVisible();
  await tenantControl(page).click();
  await page.getByRole("option", { name: "new-team", exact: true }).click();
  await expect(tenantControl(page)).toContainText("new-team");
  await tenantControl(page).click();
  await page.getByRole("option", { name: "other", exact: true }).click();
  await expect(tenantControl(page)).toContainText("other");
  await expect(page.getByRole("button", { name: "Create tenant", exact: true })).toBeVisible();
  await tenantControl(page).click();
  await expect(page.getByRole("option", { name: "new-team", exact: true })).toBeEnabled();
  expect(mock.calls.filter((call) => call.method === "CreateTenant")).toHaveLength(1);
  expect(mock.unexpected).toEqual([]);
});

test("confirmed tenants do not cross a credential change or grant viewer access", async ({ page }) => {
  const mock = await mockTenantHeader(page);
  await page.goto("/");
  await createTenant(page);
  await expect(tenantControl(page)).toContainText("new-team");
  await tenantControl(page).click();
  await page.getByRole("option", { name: "acme", exact: true }).click();
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  const settings = page.getByRole("dialog", { name: "Settings", exact: true });
  await settings.getByLabel("Access token", { exact: false }).fill("viewer-token");
  await settings.getByRole("button", { name: "Connect", exact: true }).click();
  await expect.poll(() => mock.calls.some((call) => call.method === "WhoAmI" && call.authorization === "Bearer viewer-token")).toBe(true);
  await expect(page.getByRole("button", { name: "Create tenant", exact: true })).toHaveCount(0);
  await tenantControl(page).click();
  await expect(page.getByRole("option", { name: "acme", exact: true })).toBeVisible();
  await expect(page.getByRole("option", { name: "new-team", exact: true })).toHaveCount(0);
  expect(mock.unexpected).toEqual([]);
});

test("a mismatched create response cannot add a tenant or change the active tenant", async ({ page }) => {
  const mock = await mockTenantHeader(page, { returnedId: "unconfirmed-foreign" });
  await page.goto("/");
  await createTenant(page);
  const review = page.getByRole("dialog", { name: "Review tenant", exact: true });
  await expect(review).toContainText("The server did not return the requested tenant");
  await review.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(tenantControl(page)).toContainText("acme");
  await tenantControl(page).click();
  await expect(page.getByRole("option", { name: "unconfirmed-foreign", exact: true })).toHaveCount(0);
  await expect(page.getByRole("option", { name: "new-team", exact: true })).toHaveCount(0);
  expect(mock.unexpected).toEqual([]);
});

test("confirmed tenants do not cross a verified identity change with the same credentials", async ({ page }) => {
  const mock = await mockTenantHeader(page);
  await page.goto("/");
  await createTenant(page);
  await expect(tenantControl(page)).toContainText("new-team");
  await tenantControl(page).click();
  await page.getByRole("option", { name: "acme", exact: true }).click();
  const priorIdentityCalls = mock.calls.filter((call) => call.method === "WhoAmI").length;
  mock.changeIdentity();
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  await page.getByRole("dialog", { name: "Settings", exact: true }).getByRole("button", { name: "Connect", exact: true }).click();
  await expect.poll(() => mock.calls.filter((call) => call.method === "WhoAmI").length).toBeGreaterThan(priorIdentityCalls);
  await expect(page.getByRole("button", { name: "Create tenant", exact: true })).toHaveCount(0);
  await tenantControl(page).click();
  await expect(page.getByRole("option", { name: "acme", exact: true })).toBeVisible();
  await expect(page.getByRole("option", { name: "new-team", exact: true })).toHaveCount(0);
  expect(mock.calls.every((call) => call.authorization === "")).toBe(true);
  expect(mock.unexpected).toEqual([]);
});

test("leaving during a pending create cancels confirmation and never remembers its late response", async ({ page }) => {
  const mock = await mockTenantHeader(page, { pauseCreate: true });
  await page.goto("/");
  await page.getByRole("link", { name: "Releases", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Release inventory", exact: true })).toBeVisible();
  try {
    await createTenant(page);
    await expect.poll(() => mock.calls.filter((call) => call.method === "CreateTenant").length).toBe(1);
    await page.goBack();
    await expect(page).toHaveURL("/");
    mock.releaseCreate();
    await mock.createResponded;
    await expect(page.getByRole("button", { name: "Create tenant", exact: true })).toBeVisible();
    await expect(tenantControl(page)).toContainText("acme");
    await tenantControl(page).click();
    await expect(page.getByRole("option", { name: "acme", exact: true })).toBeVisible();
    await expect(page.getByRole("option", { name: "new-team", exact: true })).toHaveCount(0);
    expect(mock.unexpected).toEqual([]);
  } finally {
    mock.releaseCreate();
  }
});

test("revoking grants hides confirmed tenants for the same principal and credentials", async ({ page }) => {
  const mock = await mockTenantHeader(page);
  await page.goto("/");
  await createTenant(page);
  await expect(tenantControl(page)).toContainText("new-team");
  await expect(page.getByRole("button", { name: "Create tenant", exact: true })).toBeVisible();
  await tenantControl(page).click();
  await expect(page.getByRole("option", { name: "new-team", exact: true })).toBeEnabled();
  await page.getByRole("option", { name: "acme", exact: true }).click();
  const priorIdentityCalls = mock.calls.filter((call) => call.method === "WhoAmI").length;
  mock.revokeGrants();
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  await page.getByRole("dialog", { name: "Settings", exact: true }).getByRole("button", { name: "Connect", exact: true }).click();
  await expect.poll(() => mock.calls.filter((call) => call.method === "WhoAmI").length).toBeGreaterThan(priorIdentityCalls);
  await expect(page.getByRole("button", { name: "Create tenant", exact: true })).toHaveCount(0);
  await tenantControl(page).click();
  await expect(page.getByRole("option", { name: "acme", exact: true })).toBeVisible();
  await expect(page.getByRole("option", { name: "new-team", exact: true })).toHaveCount(0);
  expect(mock.calls.every((call) => call.authorization === "")).toBe(true);
  expect(mock.unexpected).toEqual([]);
});

test("canceling the tenant review never sends a create RPC or remembers the draft", async ({ page }) => {
  const mock = await mockTenantHeader(page);
  await page.goto("/");
  await page.getByRole("button", { name: "Create tenant", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Create tenant", exact: true });
  await dialog.getByRole("textbox", { name: /^Tenant ID\b/ }).fill("new-team");
  await dialog.getByRole("button", { name: "Review tenant", exact: true }).click();
  await page.getByRole("dialog", { name: "Review tenant", exact: true }).getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(page.getByRole("dialog")).toHaveCount(0);
  await expect(tenantControl(page)).toContainText("acme");
  await tenantControl(page).click();
  await expect(page.getByRole("option", { name: "acme", exact: true })).toBeVisible();
  await expect(page.getByRole("option", { name: "new-team", exact: true })).toHaveCount(0);
  expect(mock.calls.filter((call) => call.method === "CreateTenant")).toHaveLength(0);
  expect(mock.unexpected).toEqual([]);
});
