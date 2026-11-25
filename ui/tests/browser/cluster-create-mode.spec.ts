import { expect, test, type Locator, type Page } from "@playwright/test";
import { stringify } from "yaml";

const accountId = "00000000-0000-4000-8000-000000000001";
const networkId = "00000000-0000-4000-8000-000000000002";
const clusterId = "00000000-0000-4000-8000-000000000003";
const account = { id: accountId, tenantId: "acme", accountName: "Production", aws: { accountId: "111122223333", role: "cluster-vending-provisioner" } };
const network = { id: networkId, tenantId: "acme", name: "production", displayName: "Production network", spec: { bootstrapId: accountId, region: "us-west-1", tier: "TIER_1" } };
const spec = {
  region: "us-west-1", network: { networkId }, releaseChannel: "RELEASE_CHANNEL_STABLE", upgradeMode: "UPGRADE_MODE_MANUAL",
  controlPlane: { kubernetesVersion: "1.35", endpointAccess: { publicAccessCidrs: ["203.0.113.0/24"] } },
  aws: { irsa: false },
};
const manifest = (tenant = "acme") => stringify({ apiVersion: "cluster-vending.io/v1", kind: "Cluster", metadata: { name: "yaml-production", tenant }, spec });

async function mockClusterCreate(page: Page, failFirstCreate = false) {
  const calls: { method: string; body: Record<string, unknown> }[] = [];
  const unexpected: string[] = [];
  await page.route("**/api/rpc/**", async (route) => {
    const method = new URL(route.request().url()).pathname.split("/").at(-1)!;
    const body = route.request().postDataJSON() as Record<string, unknown>;
    calls.push({ method, body });
    let response: object;
    switch (method) {
      case "WhoAmI": response = { anonymous: true }; break;
      case "ListTenants": response = { tenants: [{ id: "acme" }], canCreateTenants: true }; break;
      case "ListCloudAccounts": response = { accounts: [account] }; break;
      case "GetCloudAccount": response = { account }; break;
      case "ListNetworks": response = { networks: [network] }; break;
      case "GetNetwork": response = { network }; break;
      case "ListClusters": response = { clusters: [] }; break;
      case "CreateCluster":
        if (failFirstCreate && calls.filter((call) => call.method === method).length === 1) {
          await route.fulfill({ status: 503, contentType: "application/json", body: JSON.stringify({ code: "unavailable", message: "Retry the same reviewed request" }) });
          return;
        }
        response = { cluster: { id: clusterId, tenantId: "acme", name: "yaml-production", spec } };
        break;
      default:
        unexpected.push(method);
        await route.fulfill({ status: 501, contentType: "application/json", body: JSON.stringify({ code: "unimplemented", message: `Unmocked RPC: ${method}` }) });
        return;
    }
    await route.fulfill({ contentType: "application/json", body: JSON.stringify(response) });
  });
  return { calls, unexpected };
}

async function addToken(dialog: Locator, label: string, value: string) {
  await dialog.getByRole("combobox", { name: label, exact: true }).fill(value);
  await dialog.page().getByRole("option", { name: new RegExp(value.replace(/\./g, "\\.")) }).first().click();
}

test("form and YAML are two views of one create draft, and YAML edits come back to the form", async ({ page }) => {
  const mock = await mockClusterCreate(page);
  await page.goto("/clusters");
  await page.getByRole("button", { name: "Create cluster", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Create cluster", exact: true });
  await expect(dialog.getByRole("radiogroup", { name: "Edit as", exact: true })).toBeVisible();
  await expect(dialog.getByRole("radio", { name: "Form", exact: true })).toBeChecked();
  await dialog.getByRole("textbox", { name: /^Cluster name\b/ }).fill("guided-orders");
  await dialog.getByRole("button", { name: /^Existing network\b/ }).click();
  await page.getByRole("option", { name: /Production network/ }).click();
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await expect(dialog.getByRole("heading", { name: "Access", exact: true })).toBeFocused();
  await expect(dialog.getByRole("complementary")).toContainText("111122223333");
  await addToken(dialog, "Allowed IPv4 CIDRs", "198.51.100.0/24");
  await dialog.getByRole("radio", { name: "YAML", exact: true }).click();
  const editor = dialog.getByRole("textbox", { name: "Cluster YAML", exact: true });
  await expect(editor).toContainText("name: guided-orders");
  await expect(editor).toContainText("198.51.100.0/24");
  await expect(editor).not.toContainText("defaultNodePool");
  await expect(editor).not.toContainText("nodeGroups");
  await expect(editor).toContainText(`networkId: ${networkId}`);
  // A mistake keeps the YAML open and names its line.
  await editor.fill(`${await editor.innerText()}\n  colour: blue\n`);
  await dialog.getByRole("radio", { name: "Form", exact: true }).click();
  await expect(dialog.getByRole("radio", { name: "YAML", exact: true })).toBeChecked();
  await expect(dialog).toContainText(/Line \d+: .*colour/);
  // Node groups are the platform's: YAML that defines one is refused with its line.
  await editor.fill(stringify({ apiVersion: "cluster-vending.io/v1", kind: "Cluster", metadata: { name: "yaml-orders" }, spec: { ...spec, nodeGroups: [{ name: "batch" }] } }));
  await dialog.getByRole("radio", { name: "Form", exact: true }).click();
  await expect(dialog.getByRole("radio", { name: "YAML", exact: true })).toBeChecked();
  await expect(dialog).toContainText(/Line \d+: Node groups are managed by the platform\. Remove spec\.nodeGroups\./);
  await editor.fill(stringify({ apiVersion: "cluster-vending.io/v1", kind: "Cluster", metadata: { name: "yaml-orders" }, spec: { ...spec, controlPlane: { ...spec.controlPlane, endpointAccess: { publicAccessCidrs: ["192.0.2.0/24"] } } } }));
  await dialog.getByRole("radio", { name: "Form", exact: true }).click();
  await expect(dialog.getByRole("radio", { name: "Form", exact: true })).toBeChecked();
  await expect(dialog).toContainText("192.0.2.0/24");
  await dialog.getByRole("button", { name: "Back", exact: true }).click();
  await expect(dialog.getByRole("textbox", { name: /^Cluster name\b/ })).toHaveValue("yaml-orders");
  await page.screenshot({ path: "/tmp/cv-cluster-create-guided-mode.png" });
  await dialog.getByRole("radio", { name: "YAML", exact: true }).click();
  await dialog.getByRole("button", { name: "Review", exact: true }).click();
  await expect(dialog.getByRole("heading", { name: "Review", exact: true })).toBeVisible();
  expect(mock.calls.filter((call) => call.method === "CreateCluster")).toHaveLength(0);
  await dialog.getByRole("button", { name: "Back", exact: true }).click();
  await expect(editor).toContainText("yaml-orders");
  await page.screenshot({ path: "/tmp/cv-cluster-create-yaml-mode.png" });
  expect(mock.unexpected).toEqual([]);
});

test("a retried create from YAML reuses the reviewed request ID", async ({ page }) => {
  const mock = await mockClusterCreate(page, true);
  await page.goto("/clusters");
  await page.getByRole("button", { name: "Create cluster", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Create cluster", exact: true });
  await dialog.getByRole("radio", { name: "YAML", exact: true }).click();
  await dialog.getByRole("textbox", { name: "Cluster YAML", exact: true }).fill(manifest());
  await dialog.getByRole("button", { name: "Review", exact: true }).click();
  await expect(dialog.getByRole("heading", { name: "Review", exact: true })).toBeVisible();
  expect(mock.calls.filter((call) => call.method === "CreateCluster")).toHaveLength(0);
  await dialog.getByRole("button", { name: "Create cluster", exact: true }).click();
  await expect(dialog).toContainText("Retry the same reviewed request");
  await dialog.getByRole("button", { name: "Create cluster", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  const requests = mock.calls.filter((call) => call.method === "CreateCluster").map((call) => call.body);
  expect(requests).toHaveLength(2);
  expect(requests[0].requestId).toMatch(/^[a-f0-9-]{36}$/);
  expect(requests[1]).toEqual(requests[0]);
  expect(requests[0].tenantId).toBe("acme");
  expect(requests[0].cluster).toMatchObject({ name: "yaml-production", spec: { aws: { irsa: false } } });
  expect(mock.unexpected).toEqual([]);
});

test("a YAML manifest for another tenant is refused with its line", async ({ page }) => {
  const mock = await mockClusterCreate(page);
  await page.goto("/clusters");
  await page.getByRole("button", { name: "Create cluster", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Create cluster", exact: true });
  await dialog.getByRole("radio", { name: "YAML", exact: true }).click();
  await dialog.getByRole("textbox", { name: "Cluster YAML", exact: true }).fill(manifest("foreign"));
  await dialog.getByRole("button", { name: "Review", exact: true }).click();
  await expect(dialog).toContainText(/Line \d+: metadata\.tenant must match the active tenant/);
  expect(mock.calls.filter((call) => call.method === "CreateCluster")).toHaveLength(0);
});

test("cluster creation mode chooser supports keyboard selection and fits mobile", async ({ page }) => {
  const mock = await mockClusterCreate(page);
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto("/clusters");
  await page.getByRole("button", { name: "Create cluster", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Create cluster", exact: true });
  await dialog.getByRole("radio", { name: "Form", exact: true }).focus();
  await page.keyboard.press("ArrowRight");
  await expect(dialog.getByRole("radio", { name: "YAML", exact: true })).toBeChecked();
  await expect(dialog.getByRole("radio", { name: "YAML", exact: true })).toBeFocused();
  await page.keyboard.press("ArrowLeft");
  await expect(dialog.getByRole("radio", { name: "Form", exact: true })).toBeChecked();
  await expect(dialog.getByRole("radio", { name: "Form", exact: true })).toBeFocused();
  const bounds = (await dialog.boundingBox())!;
  expect(bounds.x).toBeGreaterThanOrEqual(0);
  expect(bounds.x + bounds.width).toBeLessThanOrEqual(375);
  expect(bounds.y + bounds.height).toBeLessThanOrEqual(812);
  await expect(dialog.getByRole("radiogroup", { name: "Edit as", exact: true })).toBeInViewport({ ratio: 1 });
  await expect(dialog.getByRole("complementary")).toBeHidden();
  const fields = dialog.locator("[data-workflow-body]");
  const next = dialog.getByRole("button", { name: "Next", exact: true });
  const footerPosition = (await next.boundingBox())!;
  await fields.evaluate(element => element.scrollTop = element.scrollHeight);
  expect(await fields.evaluate(element => element.scrollTop)).toBeGreaterThan(0);
  const afterScroll = (await next.boundingBox())!;
  expect(Math.abs(afterScroll.y - footerPosition.y)).toBeLessThan(1);
  expect(Math.abs(afterScroll.x - footerPosition.x)).toBeLessThan(1);
  await fields.evaluate(element => element.scrollTop = 0);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(375);
  await page.screenshot({ path: "/tmp/cv-cluster-create-mode-mobile.png" });
  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
  expect(mock.calls.filter((call) => call.method === "CreateCluster")).toHaveLength(0);
  expect(mock.unexpected).toEqual([]);
});
