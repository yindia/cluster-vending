import { expect, test, type Page } from "@playwright/test";
import { stringify } from "yaml";

const accountId = "00000000-0000-4000-8000-000000000001", networkId = "00000000-0000-4000-8000-000000000002", clusterId = "00000000-0000-4000-8000-000000000003";
const logsUrl = "https://us-west-1.console.aws.amazon.com/cloudwatch/home?region=us-west-1#logsV2:log-groups/log-group/$252Faws$252Feks$252Fproduction$252Fcluster";
// Stable offers release r-1: 1.35 on alpha, 1.34 on beta, 1.33 on stable, and 1.31 on no channel (extended support).
const release = { name: "r-1", valid: true, kubernetes: [
  { minor: "1.35", channel: "RELEASE_CHANNEL_ALPHA", endOfSupport: "2027-12-01T00:00:00Z" },
  { minor: "1.34", channel: "RELEASE_CHANNEL_BETA", endOfSupport: "2027-08-01T00:00:00Z" },
  { minor: "1.33", channel: "RELEASE_CHANNEL_STABLE", default: true, endOfSupport: "2027-03-01T00:00:00Z" },
  { minor: "1.31", endOfSupport: "2026-12-15T00:00:00Z", ending: true },
] };
const channels = ["RELEASE_CHANNEL_ALPHA", "RELEASE_CHANNEL_BETA", "RELEASE_CHANNEL_STABLE"].map((channel) => ({ channel, release: "r-1" }));

test.beforeEach(async ({ page }) => {
  await page.route("**/api/auth/session", (route) => route.fulfill({ contentType: "application/json", body: JSON.stringify({ enabled: true, authenticated: true, csrfToken: "browser-test-csrf", username: "test-user" }) }));
});

async function mock(page: Page, options: { admins?: string[]; logs?: boolean } = {}) {
  const calls: { method: string; body: Record<string, unknown> }[] = [], unexpected: string[] = [];
  const account = { id: accountId, tenantId: "acme", accountName: "Production", aws: { accountId: "111122223333", role: "cluster-vending-provisioner" } };
  const network = { id: networkId, tenantId: "acme", name: "production", displayName: "Production network", spec: { bootstrapId: accountId, region: "us-west-1", tier: "TIER_1" } };
  const cluster = {
    id: clusterId, tenantId: "acme", name: "production", etag: "cluster-etag", generation: "1",
    spec: { region: "us-west-1", network: { networkId }, upgradeMode: "UPGRADE_MODE_MANUAL", controlPlane: { kubernetesVersion: "1.33", endpointAccess: { endpointPrivateAccess: true } }, access: { adminPrincipals: options.admins ?? [] }, aws: {} },
    status: { state: "CLUSTER_STATE_RUNNING", observedGeneration: "1", currentKubernetesVersion: "1.33", aws: { clusterArn: "arn:aws:eks:us-west-1:111122223333:cluster/production", ...(options.logs ? { controlPlaneLogGroup: "/aws/eks/production/cluster", controlPlaneLogsUrl: logsUrl } : {}) } },
  };
  await page.route("**/api/rpc/**", async (route) => {
    const method = new URL(route.request().url()).pathname.split("/").at(-1)!;
    const body = route.request().postDataJSON() as Record<string, unknown>; calls.push({ method, body });
    let response: object;
    switch (method) {
      case "WhoAmI": response = { anonymous: true }; break;
      case "ListTenants": response = { tenants: [{ id: "acme" }] }; break;
      case "ListCloudAccounts": response = { accounts: [account] }; break;
      case "GetCloudAccount": response = { account }; break;
      case "ListNetworks": response = { networks: [network] }; break;
      case "GetNetwork": response = { network }; break;
      case "ListClusters": response = { clusters: [] }; break;
      case "GetCluster": response = { cluster, asRequested: true }; break;
      case "ListClusterEvents": response = { events: [] }; break;
      case "ListReleaseChannels": response = { channels }; break;
      case "GetRelease": response = { release }; break;
      case "CreateCluster": response = { cluster: { ...cluster, ...body.cluster as object } }; break;
      default: unexpected.push(method); await route.fulfill({ status: 501, contentType: "application/json", body: JSON.stringify({ code: "unimplemented", message: `Unexpected RPC ${method}` }) }); return;
    }
    await route.fulfill({ contentType: "application/json", body: JSON.stringify(response) });
  });
  return { calls, unexpected };
}

async function createWith(page: Page, spec: Record<string, unknown>) {
  await page.goto("/clusters"); await page.getByRole("button", { name: "Create cluster", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Create cluster", exact: true });
  const editor = dialog.getByRole("textbox", { name: "Cluster manifest", exact: true });
  await expect(editor).toContainText("kind: Cluster");
  await editor.fill(stringify({ apiVersion: "cluster-vending.io/v1", kind: "Cluster", metadata: { name: "legacy", tenant: "acme" }, spec: {
    region: "us-west-1", network: { networkId }, controlPlane: { kubernetesVersion: "1.31", endpointAccess: { endpointPrivateAccess: true, endpointPublicAccess: false } }, ...spec,
  } }));
  await dialog.getByRole("button", { name: "Review", exact: true }).click();
  return { dialog, editor };
}

test("an extended minor with automatic upgrades is refused on the upgradeMode line, against the channel's release", async ({ page }) => {
  const api = await mock(page);
  const { dialog, editor } = await createWith(page, { upgradeMode: "UPGRADE_MODE_AUTOMATIC", aws: {} });
  await expect(dialog.getByRole("alert")).toContainText(/Line \d+: Kubernetes 1\.31 is on extended support: it runs with UPGRADE_MODE_MANUAL only/);
  await expect(editor.locator(".cv-error-line")).toContainText("upgradeMode: UPGRADE_MODE_AUTOMATIC");
  await expect(editor).toBeFocused();
  expect(api.calls.find((call) => call.method === "GetRelease")?.body).toMatchObject({ tenantId: "acme", name: "r-1" });
  expect(api.calls.filter((call) => call.method === "CreateCluster")).toHaveLength(0);
  expect(api.unexpected).toEqual([]);
});

test("an extended minor with Standard support is refused; Manual and Extended create", async ({ page }) => {
  const api = await mock(page);
  const { dialog, editor } = await createWith(page, { upgradeMode: "UPGRADE_MODE_MANUAL", aws: { upgradePolicy: { supportType: "AWS_SUPPORT_TYPE_STANDARD" } } });
  await expect(dialog.getByRole("alert")).toContainText("AWS_SUPPORT_TYPE_STANDARD would have AWS upgrade the control plane by force");
  await expect(editor.locator(".cv-error-line")).toContainText("supportType");
  const text = await editor.evaluate((element) => (element as HTMLElement & { cmTile?: { root: { view: { state: { doc: { toString(): string } } } } } }).cmTile!.root.view.state.doc.toString());
  await editor.fill(text.replace("AWS_SUPPORT_TYPE_STANDARD", "AWS_SUPPORT_TYPE_EXTENDED"));
  await dialog.getByRole("button", { name: "Review", exact: true }).click();
  await expect(dialog.getByRole("heading", { name: "Review", exact: true })).toBeFocused();
  await expect(dialog).toContainText("No administrators: only the platform's own role is an admin by default");
  await dialog.getByRole("button", { name: "Create cluster", exact: true }).click(); await expect(dialog).toHaveCount(0);
  const body = api.calls.find((call) => call.method === "CreateCluster")!.body;
  expect(body.cluster).toMatchObject({ spec: { upgradeMode: "UPGRADE_MODE_MANUAL", controlPlane: { kubernetesVersion: "1.31" }, aws: { upgradePolicy: { supportType: "AWS_SUPPORT_TYPE_EXTENDED" } } } });
  expect(api.unexpected).toEqual([]);
});

test("the kubeconfig dialog says only the platform's role has access and opens the cluster manifest", async ({ page }) => {
  const api = await mock(page);
  await page.goto(`/clusters/${clusterId}`);
  await page.getByRole("button", { name: "Get kubeconfig", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Get kubeconfig", exact: true });
  await expect(dialog).toContainText("No one but the platform can use kubectl yet");
  await expect(dialog).not.toContainText("your own AWS session");
  await dialog.getByRole("button", { name: "Edit cluster administrators", exact: true }).click();
  const editor = page.getByRole("dialog", { name: "Edit cluster", exact: true });
  await expect(editor.getByRole("textbox", { name: "Cluster manifest", exact: true })).toContainText(`id: ${clusterId}`);
  await expect(editor.locator("[data-manifest-help]")).toContainText("spec.access.adminPrincipals");
  expect(api.unexpected).toEqual([]);
});

test("with administrators the dialog explains who kubectl works for, without the edit link", async ({ page }) => {
  const api = await mock(page, { admins: ["arn:aws:iam::111122223333:role/admin"] });
  await page.goto(`/clusters/${clusterId}`);
  await page.getByRole("button", { name: "Get kubeconfig", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Get kubeconfig", exact: true });
  await expect(dialog.locator("[data-kubectl-access]")).toContainText("its administrator, or an access entry");
  await expect(dialog.getByRole("button", { name: "Edit cluster administrators", exact: true })).toHaveCount(0);
  expect(api.unexpected).toEqual([]);
});

test("the overview links the control plane log group to CloudWatch in a new tab", async ({ page }) => {
  const api = await mock(page, { logs: true });
  await page.goto(`/clusters/${clusterId}`);
  const link = page.getByRole("link", { name: /View in CloudWatch/ });
  await expect(link).toHaveAttribute("href", logsUrl);
  await expect(link).toHaveAttribute("target", "_blank");
  await expect(page.getByText("/aws/eks/production/cluster", { exact: true })).toBeVisible();
  expect(api.unexpected).toEqual([]);
});

test("with control plane logging off the overview says so", async ({ page }) => {
  const api = await mock(page);
  await page.goto(`/clusters/${clusterId}`);
  await expect(page.getByText("Off · no control plane log types enabled", { exact: true })).toBeVisible();
  await expect(page.getByRole("link", { name: /View in CloudWatch/ })).toHaveCount(0);
  expect(api.unexpected).toEqual([]);
});
