import { expect, test, type Locator, type Page } from "@playwright/test";
import { stringify } from "yaml";

// Create, update and import take a manifest only. Every RPC is mocked; nothing reaches an API.
const accountId = "00000000-0000-4000-8000-000000000001";
const networkId = "00000000-0000-4000-8000-000000000002";
const clusterId = "00000000-0000-4000-8000-000000000003";
const account = { id: accountId, tenantId: "acme", accountName: "Production", aws: { accountId: "111122223333", role: "cluster-vending-provisioner" }, status: { state: "CLOUD_ACCOUNT_STATE_READY" } };
const network = { id: networkId, tenantId: "acme", name: "prod-network", displayName: "Production network", etag: "network-etag", generation: "1", spec: { region: "us-west-1", bootstrapId: accountId, tier: "TIER_1", layout: { cidrBlock: "10.1.0.0/16", availabilityZoneCount: 2 } }, status: { state: "NETWORK_STATE_READY", observedGeneration: "1" } };
const cluster = { id: clusterId, tenantId: "acme", name: "payments", displayName: "Payments", etag: "cluster-etag", generation: "1", spec: { region: "us-west-1", network: { networkId }, releaseChannel: "RELEASE_CHANNEL_STABLE", upgradeMode: "UPGRADE_MODE_MANUAL", controlPlane: { kubernetesVersion: "1.35", endpointAccess: { endpointPrivateAccess: true, endpointPublicAccess: false } }, access: { adminPrincipals: ["arn:aws:iam::111122223333:role/admin"] }, aws: { irsa: true } }, status: { state: "CLUSTER_STATE_RUNNING", observedGeneration: "1", currentKubernetesVersion: "1.35" } };
const plan = { planId: "reviewed-plan", applicable: true, changes: [{ kind: "Cluster", name: "payments", action: "PLANNED_ACTION_UPDATE_IN_PLACE", detail: "Display name changes" }] };

type Call = { method: string; body: Record<string, unknown> };
/** `refuse` answers that RPC with a server error, as the API would. */
async function mockAPI(page: Page, refuse: Record<string, string> = {}) {
  const calls: Call[] = [];
  const unexpected: string[] = [];
  await page.route("**/api/rpc/**", async (route) => {
    const method = new URL(route.request().url()).pathname.split("/").at(-1)!;
    const body = route.request().postDataJSON() as Record<string, unknown>;
    calls.push({ method, body });
    if (refuse[method] && !(method.startsWith("Update") && body.validateOnly && !refuse[`${method}Preview`])) {
      return route.fulfill({ status: 400, contentType: "application/json", body: JSON.stringify({ code: "invalid_argument", message: refuse[method] }) });
    }
    let response: unknown;
    switch (method) {
      case "WhoAmI": response = { anonymous: true }; break;
      case "ListTenants": response = { tenants: [{ id: "acme" }] }; break;
      case "ListCloudAccounts": response = { accounts: [account] }; break;
      case "GetCloudAccount": response = { account }; break;
      case "ListNetworks": response = { networks: [network] }; break;
      case "GetNetwork": response = { network, asRequested: true }; break;
      case "ListClusters": response = { clusters: [cluster] }; break;
      case "GetCluster": response = { cluster, asRequested: true }; break;
      case "ListClusterEvents": case "ListNetworkEvents": response = { events: [] }; break;
      case "ListReleaseChannels": response = { channels: [{ channel: "RELEASE_CHANNEL_STABLE", release: "release-stable" }] }; break;
      case "GetRelease": response = { release: { name: "release-stable", kubernetes: [{ minor: "1.35", channel: "RELEASE_CHANNEL_STABLE", default: true }] } }; break;
      case "GetAccountOnboarding": response = { onboarding: { accountId: "111122223333", roleName: "cluster-vending-provisioner" } }; break;
      case "BootstrapTenant": response = { account: { ...account, ...(body.account as object) } }; break;
      case "CreateNetwork": response = { network: { ...network, id: "00000000-0000-4000-8000-000000000012" } }; break;
      case "CreateCluster": response = { cluster: { ...cluster, id: "00000000-0000-4000-8000-000000000013" } }; break;
      case "ImportNetwork": response = { network: { ...network, name: body.name, status: { state: "NETWORK_STATE_IMPORTED" } } }; break;
      case "ImportCluster": response = { cluster: { ...cluster, name: body.name, status: { state: "CLUSTER_STATE_IMPORTED" } } }; break;
      case "UpdateNetwork": response = { network, updateRecorded: !body.validateOnly, plan: { ...plan, changes: [{ ...plan.changes[0], kind: "VPC" }] } }; break;
      case "UpdateCluster": response = { cluster, updateRecorded: !body.validateOnly, plan }; break;
      default: unexpected.push(method); response = { code: "unimplemented", message: `No fixture for ${method}` };
    }
    await route.fulfill({ status: unexpected.includes(method) ? 501 : 200, contentType: "application/json", body: JSON.stringify(response) });
  });
  return { calls, unexpected, writes: (method: string) => calls.filter((call) => call.method === method && !call.body.validateOnly) };
}

function editorOf(dialog: Locator, kind: string) { return dialog.getByRole("textbox", { name: `${kind} manifest`, exact: true }); }
/** The whole document: CodeMirror renders only the lines in view. */
async function editorText(editor: Locator) {
  return editor.evaluate((element) => (element as HTMLElement & { cmTile?: { root: { view: { state: { doc: { toString(): string } } } } } }).cmTile!.root.view.state.doc.toString());
}
const manifest = (kind: string, metadata: Record<string, unknown>, spec: Record<string, unknown>) => stringify({ apiVersion: "cluster-vending.io/v1", kind, metadata: { tenant: "acme", ...metadata }, spec });

test("cluster create starts from the commented example, refuses its placeholder ID on its line, and creates the reviewed manifest", async ({ page }) => {
  const api = await mockAPI(page);
  await page.goto("/clusters");
  await page.getByRole("button", { name: "Create cluster", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Create cluster", exact: true });
  const editor = editorOf(dialog, "Cluster");
  await expect(editor).toContainText("tenant: acme");
  expect(await editorText(editor)).toContain("add your own IAM principal here, or no one else can use kubectl");
  await expect(dialog.getByRole("complementary", { name: "Create cluster guidance" })).toContainText("Production network");
  await dialog.getByRole("button", { name: "Review", exact: true }).click();
  const alert = dialog.getByRole("alert");
  await expect(alert).toContainText(/^Line \d+: Replace the placeholder ID/);
  await expect(editor.locator(".cv-error-line")).toContainText("networkId: 00000000-0000-0000-0000-000000000000");
  await expect(editor).toBeFocused();
  await page.screenshot({ path: "/tmp/cv-manifest-cluster-error.png", animations: "disabled" });
  await editor.fill((await editorText(editor)).replace("00000000-0000-0000-0000-000000000000", networkId).replace("adminPrincipals: []", "adminPrincipals: [arn:aws:iam::111122223333:role/admin]"));
  await dialog.getByRole("button", { name: "Review", exact: true }).click();
  await expect(dialog.getByRole("heading", { name: "Review", exact: true })).toBeFocused();
  await expect(dialog).toContainText("prod-eks");
  await expect(dialog).toContainText("Production network");
  expect(api.writes("CreateCluster")).toHaveLength(0);
  await page.screenshot({ path: "/tmp/cv-manifest-cluster-review.png", animations: "disabled" });
  await dialog.getByRole("button", { name: "Create cluster", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  const [created] = api.writes("CreateCluster");
  expect(created.body).toMatchObject({ tenantId: "acme", requestId: expect.any(String), cluster: { name: "prod-eks", spec: { network: { networkId }, access: { adminPrincipals: ["arn:aws:iam::111122223333:role/admin"] } } } });
  expect(api.unexpected).toEqual([]);
});

test("a server refusal on create returns to the editor with the message on the field's line", async ({ page }) => {
  const api = await mockAPI(page, { CreateNetwork: "spec.layout.cidr_block overlaps another network of the account" });
  await page.goto("/networks");
  await page.getByRole("button", { name: "Create network", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Create network", exact: true });
  const editor = editorOf(dialog, "Network");
  await editor.fill(manifest("Network", { name: "second" }, { bootstrapId: accountId, region: "us-west-1", layout: { cidrBlock: "10.1.0.0/16", availabilityZoneCount: 2 } }));
  await dialog.getByRole("button", { name: "Review", exact: true }).click();
  await dialog.getByRole("button", { name: "Create network", exact: true }).click();
  await expect(dialog.getByRole("alert")).toContainText("overlaps another network");
  await expect(editor.locator(".cv-error-line")).toContainText("cidrBlock: 10.1.0.0/16");
  await expect(dialog.getByRole("heading", { name: "Edit YAML", exact: true })).toBeVisible();
  expect(api.writes("CreateNetwork")).toHaveLength(1);
  expect(api.unexpected).toEqual([]);
});

test("cluster update opens the cluster's manifest, previews the server plan and applies exactly that plan", async ({ page }) => {
  const api = await mockAPI(page);
  await page.goto(`/clusters/${clusterId}`);
  await page.getByRole("button", { name: "Edit cluster", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Edit cluster", exact: true });
  const editor = editorOf(dialog, "Cluster");
  await expect(editor).toContainText(`id: ${clusterId}`);
  await dialog.getByRole("button", { name: "Preview changes", exact: true }).click();
  await expect(dialog.getByRole("alert")).toContainText("Nothing to change");
  // A field left out stays as it is: only the changed display name is sent.
  await editor.fill((await editorText(editor)).replace("displayName: Payments", "displayName: Payments API").replace(/aws:\n\s+irsa: true/, "aws: {}"));
  await dialog.getByRole("button", { name: "Preview changes", exact: true }).click();
  await expect(dialog.getByRole("table", { name: "Fields this update changes" })).toContainText("Payments API");
  await expect(dialog.getByRole("table", { name: "Planned resource changes" })).toContainText("Display name changes");
  await page.screenshot({ path: "/tmp/cv-manifest-cluster-update.png", animations: "disabled" });
  await dialog.getByRole("button", { name: "Apply reviewed plan", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  const updates = api.calls.filter((call) => call.method === "UpdateCluster");
  expect(updates.map((call) => [call.body.validateOnly ?? false, call.body.planId ?? "", call.body.updateMask])).toEqual([[true, "", "displayName"], [false, "reviewed-plan", "displayName"]]);
  expect((updates[1].body.cluster as { etag: string }).etag).toBe("cluster-etag");
  await expect(page.getByText("Update recorded.", { exact: false })).toBeVisible();
  expect(api.unexpected).toEqual([]);
});

test("cluster update refuses a create-time change on its line and shows a server refusal of the plan", async ({ page }) => {
  const api = await mockAPI(page, { UpdateCluster: "spec.control_plane.kubernetes_version 1.99 is not offered by release-stable", UpdateClusterPreview: "yes" });
  await page.goto(`/clusters/${clusterId}`);
  await page.getByRole("button", { name: "Edit cluster", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Edit cluster", exact: true });
  const editor = editorOf(dialog, "Cluster");
  await expect(editor).toContainText("region: us-west-1");
  const opened = await editorText(editor);
  await editor.fill(opened.replace("region: us-west-1", "region: eu-west-1"));
  await dialog.getByRole("button", { name: "Preview changes", exact: true }).click();
  await expect(dialog.getByRole("alert")).toContainText("spec.region cannot be changed (it is set when the object is created)");
  await expect(editor.locator(".cv-error-line")).toContainText("region: eu-west-1");
  await editor.fill(opened.replace('kubernetesVersion: "1.35"', 'kubernetesVersion: "1.99"'));
  await dialog.getByRole("button", { name: "Preview changes", exact: true }).click();
  await expect(dialog.getByRole("alert")).toContainText("1.99 is not offered");
  await expect(editor.locator(".cv-error-line")).toContainText("kubernetesVersion");
  expect(api.writes("UpdateCluster")).toHaveLength(0);
  expect(api.unexpected).toEqual([]);
});

test("network update and import go through the manifest editor", async ({ page }) => {
  const api = await mockAPI(page);
  await page.goto(`/networks/${networkId}`);
  await page.getByRole("button", { name: "Edit network", exact: true }).click();
  const edit = page.getByRole("dialog", { name: "Edit network", exact: true });
  const editor = editorOf(edit, "Network");
  await expect(editor).toContainText("tier: TIER_1");
  const opened = await editorText(editor);
  await editor.fill(opened.replace("cidrBlock: 10.1.0.0/16", "cidrBlock: 10.9.0.0/16"));
  await edit.getByRole("button", { name: "Preview changes", exact: true }).click();
  await expect(edit.getByRole("alert")).toContainText("spec.layout cannot be changed");
  await editor.fill(opened.replace("tier: TIER_1", "tier: TIER_2"));
  await edit.getByRole("button", { name: "Preview changes", exact: true }).click();
  await edit.getByRole("button", { name: "Apply reviewed plan", exact: true }).click();
  await expect(edit).toHaveCount(0);
  expect(api.writes("UpdateNetwork").map((call) => [call.body.updateMask, call.body.planId])).toEqual([["spec.tier", "reviewed-plan"]]);

  await page.goto("/networks");
  await page.getByRole("button", { name: "Import network", exact: true }).click();
  const imported = page.getByRole("dialog", { name: "Import network", exact: true });
  const importEditor = editorOf(imported, "NetworkImport");
  await importEditor.fill(manifest("NetworkImport", { name: "existing", labels: { team: "a" } }, { bootstrapId: accountId, region: "us-west-1", aws: { vpcId: "vpc-0123456789abcdef0", subnetIds: ["subnet-0123456789abcdef0"] } }));
  await imported.getByRole("button", { name: "Review", exact: true }).click();
  await expect(imported.getByRole("alert")).toContainText("metadata.labels cannot be set on import");
  await expect(importEditor.locator(".cv-error-line")).toContainText("labels");
  await importEditor.fill(manifest("NetworkImport", { name: "existing" }, { bootstrapId: accountId, region: "us-west-1", aws: { vpcId: "vpc-0123456789abcdef0", subnetIds: ["subnet-0123456789abcdef0"] } }));
  await imported.getByRole("button", { name: "Review", exact: true }).click();
  await expect(imported).toContainText("Import stays read-only");
  await imported.getByRole("button", { name: "Import network", exact: true }).click();
  await expect(imported).toHaveCount(0);
  expect(api.writes("ImportNetwork")[0].body).toMatchObject({ tenantId: "acme", name: "existing", bootstrapId: accountId, aws: { vpcId: "vpc-0123456789abcdef0" } });
  expect(api.unexpected).toEqual([]);
});

test("cluster import refuses an unknown network on its line, then records discovery", async ({ page }) => {
  const api = await mockAPI(page, { GetNetwork: "network not found" });
  await page.goto("/clusters");
  await page.getByRole("button", { name: "Import cluster", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Import cluster", exact: true });
  const editor = editorOf(dialog, "ClusterImport");
  await editor.fill(manifest("ClusterImport", { name: "existing-eks" }, { networkId: "00000000-0000-4000-8000-000000000099", aws: { defaultNodeGroupName: "workers" } }));
  await dialog.getByRole("button", { name: "Review", exact: true }).click();
  await expect(dialog.getByRole("alert")).toContainText("network not found");
  await expect(editor.locator(".cv-error-line")).toContainText("networkId");
  expect(api.writes("ImportCluster")).toHaveLength(0);
  await page.unroute("**/api/rpc/**");
  const retry = await mockAPI(page);
  await editor.fill(manifest("ClusterImport", { name: "existing-eks" }, { networkId, aws: { defaultNodeGroupName: "workers" } }));
  await dialog.getByRole("button", { name: "Review", exact: true }).click();
  await dialog.getByRole("button", { name: "Import cluster", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  expect(retry.writes("ImportCluster")[0].body).toMatchObject({ tenantId: "acme", networkId, name: "existing-eks", aws: { defaultNodeGroupName: "workers" } });
});

test("account connect and edit use the CloudAccount manifest; the AWS account ID cannot change", async ({ page }) => {
  const api = await mockAPI(page, { BootstrapTenant: "spec.aws.role is not assumable by the platform" });
  await page.goto("/accounts");
  await page.getByRole("button", { name: "Connect account", exact: true }).click();
  const connect = page.getByRole("dialog", { name: "Connect cloud account", exact: true });
  const editor = editorOf(connect, "CloudAccount");
  await expect(editor).toContainText('accountId: "111122223333"');
  await editor.fill((await editorText(editor)).replace('"111122223333"', '"1234"'));
  await connect.getByRole("button", { name: "Review", exact: true }).click();
  await expect(connect.getByRole("alert")).toContainText(/Line \d+: .*account_id|accountId/);
  await expect(editor.locator(".cv-error-line")).toContainText("accountId");
  await connect.getByRole("button", { name: "Reset to example", exact: true }).click();
  await connect.getByRole("button", { name: "Review", exact: true }).click();
  await connect.getByRole("button", { name: "Connect account", exact: true }).click();
  await expect(connect.getByRole("alert")).toContainText("not assumable by the platform");
  await expect(editor.locator(".cv-error-line")).toContainText("role:");
  await connect.getByRole("button", { name: "Cancel", exact: true }).click();

  await page.unroute("**/api/rpc/**");
  const ok = await mockAPI(page);
  await page.goto(`/accounts/${accountId}`);
  await page.getByRole("button", { name: "Edit account", exact: true }).click();
  const edit = page.getByRole("dialog", { name: "Edit account", exact: true });
  const accountEditor = editorOf(edit, "CloudAccount");
  await expect(accountEditor).toContainText(`id: ${accountId}`);
  const opened = await editorText(accountEditor);
  await accountEditor.fill(opened.replace("111122223333", "444455556666"));
  await edit.getByRole("button", { name: "Review changes", exact: true }).click();
  await expect(edit.getByRole("alert")).toContainText("spec.aws.accountId cannot change");
  await accountEditor.fill(opened.replace("role: cluster-vending-provisioner", "role: cluster-vending-updater"));
  await edit.getByRole("button", { name: "Review changes", exact: true }).click();
  await expect(edit.getByRole("table", { name: "Fields this update changes" })).toContainText("cluster-vending-updater");
  await edit.getByRole("button", { name: "Save account", exact: true }).click();
  await expect(edit).toHaveCount(0);
  expect(ok.writes("BootstrapTenant")[0].body).toMatchObject({ tenantId: "acme", account: { accountName: "Production", aws: { accountId: "111122223333", role: "cluster-vending-updater" } } });
  expect(api.unexpected).toEqual([]);
});

test("the manifest dialog fits a phone, keeps its actions in view and closes with Escape", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await mockAPI(page);
  await page.goto("/networks");
  const trigger = page.getByRole("button", { name: "Create network", exact: true });
  await trigger.click();
  const dialog = page.getByRole("dialog", { name: "Create network", exact: true });
  await expect(editorOf(dialog, "Network")).toBeVisible();
  const bounds = (await dialog.boundingBox())!;
  expect(bounds.x).toBeGreaterThanOrEqual(0);
  expect(bounds.x + bounds.width).toBeLessThanOrEqual(390);
  await expect(dialog.getByRole("button", { name: "Review", exact: true })).toBeInViewport();
  await page.screenshot({ path: "/tmp/cv-manifest-mobile.png", animations: "disabled" });
  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
  await expect(trigger).toBeFocused();
});
