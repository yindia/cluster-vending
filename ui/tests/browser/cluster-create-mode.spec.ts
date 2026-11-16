import { expect, test, type Page } from "@playwright/test";
import { stringify } from "yaml";

const accountId = "00000000-0000-4000-8000-000000000001";
const networkId = "00000000-0000-4000-8000-000000000002";
const clusterId = "00000000-0000-4000-8000-000000000003";
const account = { id: accountId, tenantId: "acme", accountName: "Production", aws: { accountId: "111122223333", role: "cluster-vending-provisioner" } };
const network = { id: networkId, tenantId: "acme", name: "production", displayName: "Production network", spec: { bootstrapId: accountId, region: "us-west-1", tier: "TIER_1" } };
const spec = {
  region: "us-west-1", network: { networkId }, releaseChannel: "RELEASE_CHANNEL_STABLE", upgradeMode: "UPGRADE_MODE_MANUAL",
  controlPlane: { kubernetesVersion: "1.35", endpointAccess: { publicAccessCidrs: ["203.0.113.0/24"] } },
  defaultNodePool: { instanceTypes: ["m6i.large"], scalingConfig: { minSize: 1, maxSize: 3 } }, aws: { irsa: false },
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

test("cluster creation mode chooser preserves both drafts and keeps tenant validation and final review", async ({ page }) => {
  const mock = await mockClusterCreate(page);
  await page.goto("/clusters");
  await page.getByRole("button", { name: "Create cluster", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Create cluster", exact: true });
  await expect(dialog.getByRole("radiogroup", { name: "Creation method", exact: true })).toBeVisible();
  await expect(dialog.getByRole("radio", { name: "Guided form", exact: true })).toBeChecked();
  await dialog.getByRole("textbox", { name: /^Cluster name\b/ }).fill("guided-orders");
  await dialog.getByRole("button", { name: /^Existing network\b/ }).click();
  await page.getByRole("option", { name: /Production network/ }).click();
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await expect(dialog.getByRole("heading", { name: "Compute", exact: true })).toBeFocused();
  await dialog.getByRole("textbox", { name: /^Instance types\b/ }).fill("c7i.large");
  await dialog.getByRole("radio", { name: "Paste YAML", exact: true }).click();
  const editor = dialog.getByRole("textbox", { name: "Cluster manifest", exact: true });
  await editor.fill(manifest("foreign"));
  await dialog.getByRole("button", { name: "Review", exact: true }).click();
  await expect(dialog).toContainText("must match the active tenant");
  await expect(editor).toBeFocused();
  await dialog.getByRole("radio", { name: "Guided form", exact: true }).click();
  await expect(dialog.getByRole("textbox", { name: /^Instance types\b/ })).toHaveValue("c7i.large");
  await dialog.getByRole("button", { name: "Back", exact: true }).click();
  await expect(dialog.getByRole("textbox", { name: /^Cluster name\b/ })).toHaveValue("guided-orders");
  await expect(dialog.getByRole("button", { name: /^Existing network\b/ })).toContainText("Production network");
  await page.screenshot({ path: "/tmp/cv-cluster-create-guided-mode.png" });
  await dialog.getByRole("radio", { name: "Paste YAML", exact: true }).click();
  await expect(editor).toContainText("foreign");
  await editor.fill(manifest());
  await dialog.getByRole("button", { name: "Review", exact: true }).click();
  await expect(dialog.getByRole("heading", { name: "Review creation", exact: true })).toBeVisible();
  expect(mock.calls.filter((call) => call.method === "CreateCluster")).toHaveLength(0);
  await dialog.getByRole("button", { name: "Back", exact: true }).click();
  await expect(editor).toContainText("yaml-production");
  await page.screenshot({ path: "/tmp/cv-cluster-create-yaml-mode.png" });
  expect(mock.unexpected).toEqual([]);
});

test("switching cluster creation modes after a failed apply retains the reviewed YAML request ID", async ({ page }) => {
  const mock = await mockClusterCreate(page, true);
  await page.goto("/clusters");
  await page.getByRole("button", { name: "Create cluster", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Create cluster", exact: true });
  await dialog.getByRole("radio", { name: "Paste YAML", exact: true }).click();
  await dialog.getByRole("textbox", { name: "Cluster manifest", exact: true }).fill(manifest());
  await dialog.getByRole("button", { name: "Review", exact: true }).click();
  await expect(dialog.getByRole("heading", { name: "Review creation", exact: true })).toBeVisible();
  expect(mock.calls.filter((call) => call.method === "CreateCluster")).toHaveLength(0);
  await dialog.getByRole("button", { name: "Create cluster", exact: true }).click();
  await expect(dialog).toContainText("Retry the same reviewed request");
  await dialog.getByRole("radio", { name: "Guided form", exact: true }).click();
  await dialog.getByRole("radio", { name: "Paste YAML", exact: true }).click();
  await expect(dialog.getByRole("textbox", { name: "Cluster manifest", exact: true })).toContainText("yaml-production");
  await dialog.getByRole("button", { name: "Review", exact: true }).click();
  await expect(dialog.getByRole("heading", { name: "Review creation", exact: true })).toBeVisible();
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

test("cluster creation mode chooser supports keyboard selection and fits mobile", async ({ page }) => {
  const mock = await mockClusterCreate(page);
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto("/clusters");
  await page.getByRole("button", { name: "Create cluster", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Create cluster", exact: true });
  await dialog.getByRole("radio", { name: "Guided form", exact: true }).focus();
  await page.keyboard.press("ArrowRight");
  await expect(dialog.getByRole("radio", { name: "Paste YAML", exact: true })).toBeChecked();
  await expect(dialog.getByRole("radio", { name: "Paste YAML", exact: true })).toBeFocused();
  await page.keyboard.press("ArrowLeft");
  await expect(dialog.getByRole("radio", { name: "Guided form", exact: true })).toBeChecked();
  await expect(dialog.getByRole("radio", { name: "Guided form", exact: true })).toBeFocused();
  const bounds = (await dialog.boundingBox())!;
  expect(bounds.x).toBeGreaterThanOrEqual(0);
  expect(bounds.x + bounds.width).toBeLessThanOrEqual(375);
  expect(bounds.y + bounds.height).toBeLessThanOrEqual(812);
  await expect(dialog.getByRole("radiogroup", { name: "Creation method", exact: true })).toBeInViewport({ ratio: 1 });
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(375);
  await page.screenshot({ path: "/tmp/cv-cluster-create-mode-mobile.png" });
  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
  expect(mock.calls.filter((call) => call.method === "CreateCluster")).toHaveLength(0);
  expect(mock.unexpected).toEqual([]);
});
