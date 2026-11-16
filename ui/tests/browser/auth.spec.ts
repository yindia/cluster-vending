import { expect, test, type Page } from "@playwright/test";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

// Real browser login is opt-in and never records credentials in traces/video.
test.use({ trace: "off", screenshot: "off", video: "off" });
test.skip(process.env.CV_AUTH_LIVE !== "1", "Requires the local Keycloak fixture and required-auth API");

function fixtureSecret(key: string) {
  const lines = readFileSync(resolve(process.cwd(), "../hack/.keycloak/secrets.env"), "utf8").split("\n");
  const line = lines.find(value => value.replace(/^export\s+/, "").startsWith(`${key}=`));
  if (!line) throw new Error("Local authentication fixture is incomplete");
  return line.slice(line.indexOf("=") + 1).trim().replace(/^['"]|['"]$/g, "");
}
async function signIn(page: Page, username: string, passwordKey: string) {
  await page.goto("/");
  await page.getByRole("link", { name: "Continue with SSO", exact: true }).click();
  await page.locator('input[name="username"]').fill(username);
  await page.locator('input[name="password"]').fill(fixtureSecret(passwordKey));
  await page.locator("#kc-login").click();
  await page.waitForURL(url => url.origin === "http://localhost:3000", { timeout: 20_000 });
  await expect(page.getByRole("button", { name: "Settings", exact: true })).toBeVisible();
}

test("login screen is centered, responsive and exposes no anonymous RPC fallback", async ({ page }) => {
  await page.goto("/");
  await expect(page.getByRole("heading", { name: "Sign in to your workspace" })).toBeVisible();
  await expect(page.getByRole("link", { name: "Continue with SSO", exact: true })).toBeVisible();
  const response = await page.request.post("/api/rpc/cloud.v1.TenantService/WhoAmI", { data: {}, headers: { Origin: "http://localhost:3000", Authorization: "Bearer browser-forgery" } });
  expect(response.status()).toBe(401);
  await page.screenshot({ path: test.info().outputPath("signin-desktop.png") });
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(page.getByRole("link", { name: "Continue with SSO", exact: true })).toBeInViewport();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  await page.screenshot({ path: test.info().outputPath("signin-mobile.png") });
});

for (const actor of [
  { username: "acme-admin", password: "KEYCLOAK_ACME_ADMIN_PASSWORD", role: "ACCESS_ROLE_TENANT_ADMIN" },
  { username: "acme-viewer", password: "KEYCLOAK_ACME_VIEWER_PASSWORD", role: "ACCESS_ROLE_TENANT_VIEWER" },
]) {
  test(`real SSO for ${actor.username} keeps tokens server-side and enforces tenant permissions`, async ({ page }) => {
    const browserAuthorization: string[] = [];
    page.on("request", request => { if (request.url().includes("/api/rpc/") && request.headers().authorization) browserAuthorization.push("unexpected bearer header"); });
    await signIn(page, actor.username, actor.password);
    const session = await (await page.request.get("/api/auth/session")).json();
    expect(session.authenticated).toBe(true);
    expect(session.username).toBe(actor.username);
    expect(Object.keys(session).sort()).toEqual(["authenticated", "csrfToken", "enabled", "username"]);
    const headers = { Origin: "http://localhost:3000", "X-CV-CSRF": session.csrfToken };
    const who = await page.request.post("/api/rpc/cloud.v1.TenantService/WhoAmI", { data: {}, headers });
    expect(who.status()).toBe(200);
    const identity = await who.json();
    expect(identity.anonymous).not.toBe(true);
    expect(identity.username).toBe(actor.username);
    expect(identity.grants).toContainEqual({ role: actor.role, tenants: ["acme"] });
    expect(browserAuthorization).toEqual([]);
    expect(await page.evaluate(() => Object.keys(localStorage))).toEqual([]);
    const cookies = await page.context().cookies();
    expect(cookies.filter(cookie => cookie.name.startsWith("cv.session")).every(cookie => cookie.httpOnly && cookie.sameSite === "Lax")).toBe(true);
    const otherTenant = await page.request.post("/api/rpc/cloud.v1.TenantService/ListCloudAccounts", { data: { tenantId: "globex" }, headers });
    expect(otherTenant.status()).toBe(403);
    if (actor.username === "acme-viewer") {
      const mutation = await page.request.post("/api/rpc/cloud.v1.SubTenantService/UpdateSubTenant", { data: { tenantId: "acme" }, headers });
      expect(mutation.status()).toBe(403);
    }
    await page.getByRole("button", { name: "Settings", exact: true }).click();
    await expect(page.getByLabel("Access token", { exact: true })).toHaveCount(0);
    await page.getByRole("button", { name: "Sign out", exact: true }).click();
    await page.waitForURL(url => url.origin === "http://localhost:3000", { timeout: 20_000 });
    await expect(page.getByRole("heading", { name: "Sign in to your workspace" })).toBeVisible();
    expect((await (await page.request.get("/api/auth/session")).json()).authenticated).toBe(false);
  });
}
