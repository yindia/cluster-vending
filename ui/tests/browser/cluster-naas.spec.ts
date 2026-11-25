import { expect, test, type Page } from "@playwright/test";
import { stringify } from "yaml";

const accountId = "00000000-0000-4000-8000-000000000001";
const networkId = "00000000-0000-4000-8000-000000000002";
const clusterId = "00000000-0000-4000-8000-000000000003";
const controlName = "Namespace as a Service (NaaS)";
const account = { id: accountId, tenantId: "acme", accountName: "Production", aws: { accountId: "111122223333", role: "cluster-vending-provisioner" } };
const network = { id: networkId, tenantId: "acme", name: "production", displayName: "Production network", spec: { bootstrapId: accountId, region: "us-west-1", tier: "TIER_1" } };
const spec = {
  region: "us-west-1", network: { networkId }, releaseChannel: "RELEASE_CHANNEL_STABLE", upgradeMode: "UPGRADE_MODE_MANUAL",
  controlPlane: { kubernetesVersion: "1.35", endpointAccess: { endpointPrivateAccess: true, endpointPublicAccess: false } },
  aws: { irsa: true },
};

async function mockNaaS(page: Page, initiallyEnabled = false) {
  const calls: { method: string; body: Record<string, unknown> }[] = [];
  const unexpected: string[] = [];
  let enabled = initiallyEnabled;
  const cluster = () => ({ id: clusterId, tenantId: "acme", name: "payments", etag: "naas-etag", generation: "1", spec: { ...spec, naas: enabled }, status: { state: "CLUSTER_STATE_RUNNING", observedGeneration: "1", currentKubernetesVersion: "1.35" } });
  await page.route("**/api/rpc/**", async (route) => {
    const method = new URL(route.request().url()).pathname.split("/").at(-1)!;
    const body = route.request().postDataJSON() as Record<string, unknown>;
    calls.push({ method, body });
    let response: object;
    switch (method) {
      case "WhoAmI": response = { anonymous: true }; break;
      case "ListTenants": response = { tenants: [{ id: "acme" }] }; break;
      case "ListCloudAccounts": response = { accounts: [account] }; break;
      case "GetCloudAccount": response = { account }; break;
      case "ListNetworks": response = { networks: [network] }; break;
      case "GetNetwork": response = { network }; break;
      case "ListClusters": response = { clusters: [cluster()] }; break;
      case "GetCluster": response = { cluster: cluster(), asRequested: true }; break;
      case "ListSubTenants": response = { subtenants: [] }; break;
      case "ListClusterEvents": response = { events: [] }; break;
      case "CreateCluster": response = { cluster: { ...cluster(), ...body.cluster as object } }; break;
      case "UpdateCluster":
        if (body.validateOnly) response = { plan: { planId: "naas-reviewed-plan", applicable: true, changes: [{ action: "PLANNED_ACTION_DELETE", kind: "Release", name: "capsule", detail: "Turn off namespace tenancy controls. Existing namespaces, workloads, and data are not deleted.", retainsCloudResource: false }] } };
        else { enabled = false; response = { updateRecorded: true, cluster: cluster() }; }
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

test("guided NaaS starts off, survives mode switching and is enabled only after final confirmation", async ({ page }) => {
  const mock = await mockNaaS(page);
  await page.goto("/clusters");
  await page.getByRole("button", { name: "Create cluster", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Create cluster", exact: true });
  const checkbox = dialog.getByRole("checkbox", { name: controlName, exact: true });
  await expect(checkbox).not.toBeChecked();
  await expect(dialog).toContainText("team namespaces with separate access, quotas, and policies");
  await expect(dialog).not.toContainText("cert-manager");
  await checkbox.check();
  await dialog.getByRole("textbox", { name: /^Cluster name\b/ }).fill("naas-enabled");
  await dialog.getByRole("button", { name: /^Existing network\b/ }).click();
  await page.getByRole("option", { name: /Production network/ }).click();
  await dialog.getByRole("radio", { name: "YAML", exact: true }).click();
  await expect(dialog.getByRole("textbox", { name: "Cluster YAML", exact: true })).toContainText("naas: true");
  await dialog.getByRole("radio", { name: "Form", exact: true }).click();
  await expect(checkbox).toBeChecked();
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await dialog.getByRole("checkbox", { name: "Public endpoint", exact: true }).uncheck();
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await expect(dialog.locator("dt").filter({ hasText: controlName }).locator("xpath=following-sibling::dd[1]")).toHaveText("Enabled");
  expect(mock.calls.filter((call) => call.method === "CreateCluster")).toHaveLength(0);
  await dialog.getByRole("button", { name: "Create cluster", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  const request = mock.calls.find((call) => call.method === "CreateCluster")!.body;
  expect(request.cluster).toMatchObject({ name: "naas-enabled", spec: { naas: true } });
  expect(request.requestId).toMatch(/^[a-f0-9-]{36}$/);
  expect(mock.unexpected).toEqual([]);
});

test("disabling NaaS previews Capsule removal with retained data and applies only the reviewed false leaf", async ({ page }) => {
  const mock = await mockNaaS(page, true);
  await page.goto(`/clusters/${clusterId}`);
  await expect(page.getByRole("link", { name: "View Sub Tenants" })).toHaveAttribute("href", `/clusters/${clusterId}?tab=subtenants`);
  await page.getByRole("button", { name: "Edit cluster", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Edit cluster", exact: true });
  const checkbox = dialog.getByRole("checkbox", { name: controlName, exact: true });
  await expect(checkbox).toBeChecked();
  await checkbox.uncheck();
  for (let next = 0; next < 2; next++) await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await dialog.getByRole("button", { name: "Preview update plan", exact: true }).click();
  await expect(dialog.getByRole("heading", { name: "Review", exact: true })).toBeVisible();
  await expect(dialog).toContainText("Turn off namespace tenancy controls");
  await expect(dialog).toContainText("Existing namespaces, workloads, and data will remain");
  await expect(dialog).not.toContainText("cert-manager");
  const preview = mock.calls.find((call) => call.method === "UpdateCluster")!.body;
  expect(preview.validateOnly).toBe(true);
  expect(preview.updateMask).toBe("spec.naas");
  const patch = preview.cluster as { id: string; etag: string; spec: { naas?: boolean } };
  // Optional generated bool preserves explicit false; the leaf mask makes the clear intentional.
  expect(patch.spec.naas).toBe(false);
  expect(Object.keys(patch.spec).filter((key) => key !== "naas")).toEqual([]);
  expect(patch.etag).toBe("naas-etag");
  expect(mock.calls.filter((call) => call.method === "UpdateCluster")).toHaveLength(1);
  await dialog.getByRole("button", { name: "Apply reviewed plan", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  const apply = mock.calls.filter((call) => call.method === "UpdateCluster").at(-1)!.body;
  expect(apply.planId).toBe("naas-reviewed-plan");
  expect(apply.validateOnly ?? false).toBe(false);
  expect(apply.cluster).toEqual(preview.cluster);
  expect(apply.updateMask).toBe("spec.naas");
  await expect(page.locator("dt").filter({ hasText: "Namespace as a Service" }).locator("+ dd")).toHaveText("Disabled");
  await expect(page.getByRole("link", { name: "View Sub Tenants" })).toHaveCount(0);
  expect(mock.unexpected).toEqual([]);
});

for (const naas of [false, true]) {
  test(`cluster YAML reviews and creates generated NaaS ${naas}`, async ({ page }) => {
    const mock = await mockNaaS(page);
    await page.goto("/clusters");
    await page.getByRole("button", { name: "Create cluster", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "Create cluster", exact: true });
    await dialog.getByRole("radio", { name: "YAML", exact: true }).click();
    await dialog.getByRole("textbox", { name: "Cluster YAML", exact: true }).fill(stringify({ apiVersion: "cluster-vending.io/v1", kind: "Cluster", metadata: { tenant: "acme", name: "yaml-naas" }, spec: { ...spec, naas } }));
    await dialog.getByRole("button", { name: "Review", exact: true }).click();
    await expect(dialog.getByRole("heading", { name: "Review", exact: true })).toBeVisible();
    expect(mock.calls.filter((call) => call.method === "CreateCluster")).toHaveLength(0);
    await dialog.getByRole("button", { name: "Create cluster", exact: true }).click();
    await expect(dialog).toHaveCount(0);
    const request = mock.calls.find((call) => call.method === "CreateCluster")!.body;
    expect((request.cluster as { spec: { naas?: boolean } }).spec.naas).toBe(naas);
    expect(mock.unexpected).toEqual([]);
  });
}
