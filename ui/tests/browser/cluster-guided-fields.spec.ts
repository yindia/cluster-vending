import { expect, test, type Locator, type Page } from "@playwright/test";
import { stringify } from "yaml";

const accountId = "00000000-0000-4000-8000-000000000001", networkId = "00000000-0000-4000-8000-000000000002", clusterId = "00000000-0000-4000-8000-000000000003";
const keyArn = "arn:aws:kms:us-west-1:111122223333:key/00000000-0000-4000-8000-000000000007";
const baselineSpec = {
  region: "us-west-1", network: { networkId, zoneIds: ["usw1-az1", "usw1-az2"], serviceCidr: "172.20.0.0/16" }, releaseChannel: "RELEASE_CHANNEL_STABLE", upgradeMode: "UPGRADE_MODE_MANUAL",
  maintenanceWindow: { days: ["WEEKDAY_MONDAY"], start: "02:00", duration: "4h" },
  controlPlane: { kubernetesVersion: "1.35", endpointAccess: { endpointPrivateAccess: true, endpointPublicAccess: false }, secretsEncryption: { enabled: true, kmsKeyArn: keyArn } },
  aws: { irsa: true, additionalSecurityGroupIds: ["sg-0123456789abcdef0"], accessConfig: { authenticationMode: "AWS_AUTHENTICATION_MODE_API" } },
};
test.beforeEach(async ({ page }) => {
  await page.route("**/api/auth/session", (route) => route.fulfill({ contentType: "application/json", body: JSON.stringify({ enabled: true, authenticated: true, csrfToken: "browser-test-csrf", username: "test-user" }) }));
});
async function mock(page: Page) {
  const calls: { method: string; body: Record<string, unknown> }[] = [], unexpected: string[] = [];
  const account = { id: accountId, tenantId: "acme", accountName: "Production", aws: { accountId: "111122223333", role: "cluster-vending-provisioner" } };
  const network = { id: networkId, tenantId: "acme", name: "production", displayName: "Production network", spec: { bootstrapId: accountId, region: "us-west-1", tier: "TIER_1" } };
  const cluster = { id: clusterId, tenantId: "acme", name: "production", etag: "cluster-etag", generation: "1", labels: { environment: "production" }, spec: baselineSpec, status: { state: "CLUSTER_STATE_RUNNING", observedGeneration: "1" } };
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
      case "CreateCluster": response = { cluster: { ...cluster, ...body.cluster as object } }; break;
      case "UpdateCluster": response = body.validateOnly ? { plan: { planId: "guided-plan", applicable: true, changes: [{ action: "PLANNED_ACTION_UPDATE_IN_PLACE", kind: "Cluster", name: "production", detail: "Reviewed configuration changes" }] } } : { cluster, updateRecorded: true }; break;
      default: unexpected.push(method); await route.fulfill({ status: 501, contentType: "application/json", body: JSON.stringify({ code: "unimplemented", message: `Unexpected RPC ${method}` }) }); return;
    }
    await route.fulfill({ contentType: "application/json", body: JSON.stringify(response) });
  });
  return { calls, unexpected };
}
const input = (dialog: Locator, name: string) => dialog.getByRole("textbox", { name, exact: true });
// Required fields append "Required" to their accessible name.
const required = (dialog: Locator, name: string) => dialog.getByRole("textbox", { name: new RegExp(`^${name.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}( Required)?$`) });
async function addToken(dialog: Locator, label: string, value: string) {
  await dialog.getByRole("combobox", { name: label, exact: true }).fill(value);
  await dialog.page().getByRole("option", { name: new RegExp(value.replace(/\./g, "\\.")) }).first().click();
}
async function choose(page: Page, dialog: Locator, name: string, option: string) {
  await dialog.getByRole("combobox", { name, exact: true }).or(dialog.getByRole("button", { name, exact: true })).click();
  await page.getByRole("option", { name: option, exact: true }).click();
}
async function create(page: Page) {
  await page.goto("/clusters"); await page.getByRole("button", { name: "Create cluster", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Create cluster", exact: true });
  await dialog.getByRole("textbox", { name: /^Cluster name\b/ }).fill("guided-production");
  await dialog.getByRole("button", { name: /^Existing network\b/ }).or(dialog.getByRole("combobox", { name: /^Existing network\b/ })).click(); await page.getByRole("option", { name: /Production network/ }).click();
  return dialog;
}
async function edit(page: Page) {
  await page.goto(`/clusters/${clusterId}`); await page.getByRole("button", { name: "Edit cluster", exact: true }).click();
  return page.getByRole("dialog", { name: "Edit cluster", exact: true });
}

test("guided create reviews and sends tags and every typed spec field, and no node groups", async ({ page }) => {
  test.setTimeout(90_000);
  const api = await mock(page), d = await create(page);
  await d.getByRole("button", { name: "Add cloud tag", exact: true }).click();
  await input(d, "Cloud tag 1 key").fill("team"); await expect(input(d, "Cloud tag 1 key")).toBeFocused(); await input(d, "Cloud tag 1 value").fill("platform");
  await choose(page, d, "AWS support policy", "Extended");
  await d.getByRole("checkbox", { name: "Monday", exact: true }).check(); await input(d, "Maintenance start (UTC)").fill("02:00"); await input(d, "Maintenance duration").fill("90m");
  await expect(d.getByRole("region", { name: "Node groups", exact: true })).toContainText("Managed by the platform");
  await d.getByRole("button", { name: "Next", exact: true }).click(); await expect(d.getByRole("heading", { name: "Access", exact: true })).toBeFocused();
  await d.getByRole("checkbox", { name: "Public endpoint", exact: true }).uncheck();
  await addToken(d, "Availability zone IDs", "usw1-az1"); await addToken(d, "Availability zone IDs", "usw1-az2"); await input(d, "Kubernetes service CIDR").fill("172.20.0.0/16"); await addToken(d, "Additional cluster security groups", "sg-0123456789abcdef0");
  await choose(page, d, "Encrypt Kubernetes Secrets", "Enabled"); await input(d, "Encryption KMS key ARN").fill(keyArn);
  await choose(page, d, "AWS authentication mode", "Api");
  await d.getByRole("checkbox", { name: "Use an OIDC identity provider", exact: true }).check();
  await required(d, "Provider name").fill("workforce"); await required(d, "Issuer URL").fill("https://issuer.example.com"); await required(d, "Client ID").fill("cluster-users"); await input(d, "Groups claim").fill("groups");
  await d.getByRole("button", { name: "Add access entry", exact: true }).click(); await required(d, "Access entry 1 principal ARN").fill("arn:aws:iam::111122223333:role/developers");
  await d.getByRole("button", { name: "Add policy to access entry 1", exact: true }).click(); await required(d, "Access entry 1 policy 1 ARN").fill("arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy");
  await d.getByRole("button", { name: "Next", exact: true }).click(); await expect(d.getByRole("heading", { name: "Add-ons and logging", exact: true })).toBeFocused();
  await choose(page, d, "Managed control plane log group", "Enabled"); await input(d, "Control plane log retention (days)").fill("30"); await d.getByRole("checkbox", { name: "Api", exact: true }).check(); await d.getByRole("checkbox", { name: "Audit", exact: true }).check();
  await d.getByRole("button", { name: "Add AWS add-on", exact: true }).click(); await required(d, "AWS add-on 1 name").fill("metrics-server"); await d.getByRole("button", { name: "Next", exact: true }).click();
  await expect(d.getByRole("heading", { name: "Review", exact: true })).toBeFocused(); await expect(d).toContainText("team=platform"); await expect(d).toContainText("Managed by the platform: the system node group and the Karpenter platform pool");
  expect(api.calls.filter((call) => call.method === "CreateCluster")).toHaveLength(0); await d.getByRole("button", { name: "Create cluster", exact: true }).click(); await expect(d).toHaveCount(0);
  const body = api.calls.find((call) => call.method === "CreateCluster")!.body;
  expect(body.cluster).toMatchObject({ labels: { team: "platform" }, spec: { maintenanceWindow: { days: ["WEEKDAY_MONDAY"], start: "02:00", duration: "90m" }, network: { zoneIds: ["usw1-az1", "usw1-az2"], serviceCidr: "172.20.0.0/16" }, controlPlane: { secretsEncryption: { enabled: true, kmsKeyArn: keyArn } }, access: { oidc: { name: "workforce", issuerUrl: "https://issuer.example.com", clientId: "cluster-users", groupsClaim: "groups" }, accessEntries: [{ principalArn: "arn:aws:iam::111122223333:role/developers", policies: [{ policyArn: "arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy" }] }] }, aws: { additionalSecurityGroupIds: ["sg-0123456789abcdef0"], enabledClusterLogTypes: ["AWS_CLUSTER_LOG_TYPE_API", "AWS_CLUSTER_LOG_TYPE_AUDIT"], upgradePolicy: { supportType: "AWS_SUPPORT_TYPE_EXTENDED" }, controlPlaneLogGroup: { enabled: true, retentionDays: 30 }, addons: [{ name: "metrics-server" }] } } });
  const spec = (body.cluster as { spec: Record<string, unknown> & { aws: Record<string, unknown> } }).spec;
  expect(spec.defaultNodePool).toBeUndefined(); expect(spec.nodeGroups).toBeUndefined(); expect(spec.aws.defaultNodePool).toBeUndefined();
  expect(api.unexpected).toEqual([]);
});

test("edit preserves platform-managed placement and encryption while sending sparse tag and maintenance masks", async ({ page }) => {
  const api = await mock(page), d = await edit(page);
  await input(d, "Cloud tag 1 value").fill("staging"); await input(d, "Maintenance start (UTC)").fill("03:00"); await d.getByRole("button", { name: "Next", exact: true }).click();
  for (const label of ["Availability zone IDs", "Kubernetes service CIDR", "Additional cluster security groups"]) await expect(input(d, label)).toHaveAttribute("readonly", "");
  await expect(input(d, "Encryption KMS key ARN")).toHaveAttribute("readonly", ""); await expect(input(d, "Encryption KMS key ARN")).toHaveValue(keyArn); await expect(d.getByRole("combobox", { name: "Encrypt Kubernetes Secrets", exact: true })).toBeDisabled();
  await expect(d.getByRole("combobox", { name: "AWS authentication mode", exact: true })).toBeDisabled();
  await d.getByRole("button", { name: "Next", exact: true }).click();
  await d.getByRole("button", { name: "Preview update plan", exact: true }).click(); await expect(d).toContainText("environment=staging");
  const preview = api.calls.find((call) => call.method === "UpdateCluster")!.body;
  expect(String(preview.updateMask).split(",").sort()).toEqual(["labels", "spec.maintenanceWindow"].sort());
  expect(preview.cluster).toEqual({ id: clusterId, etag: "cluster-etag", labels: { environment: "staging" }, spec: { maintenanceWindow: { days: ["WEEKDAY_MONDAY"], start: "03:00", duration: "4h" } } });
  await d.getByRole("button", { name: "Apply reviewed plan", exact: true }).click(); await expect(d).toHaveCount(0); const applied = api.calls.filter((call) => call.method === "UpdateCluster")[1].body; expect(applied.cluster).toEqual(preview.cluster); expect(applied.updateMask).toBe(preview.updateMask); expect(applied.planId).toBe("guided-plan"); expect(api.unexpected).toEqual([]);
});

test("advanced edits use their own masks and an incomplete OIDC provider stays on the Access step", async ({ page }) => {
  const api = await mock(page), d = await edit(page); await d.getByRole("button", { name: "Next", exact: true }).click();
  await d.getByRole("checkbox", { name: "Use an OIDC identity provider", exact: true }).check(); await required(d, "Provider name").fill("workforce");
  await d.getByRole("button", { name: "Next", exact: true }).click(); await expect(d.getByRole("heading", { name: "Access", exact: true })).toBeVisible(); expect(api.calls.filter((call) => call.method === "UpdateCluster")).toHaveLength(0);
  await required(d, "Issuer URL").fill("https://issuer.example.com"); await required(d, "Client ID").fill("cluster-users");
  await d.getByRole("button", { name: "Next", exact: true }).click();
  await d.getByRole("checkbox", { name: "Audit", exact: true }).check(); await input(d, "Control plane log retention (days)").fill("90");
  await d.getByRole("button", { name: "Add AWS add-on", exact: true }).click(); await required(d, "AWS add-on 1 name").fill("metrics-server");
  await d.getByRole("button", { name: "Preview update plan", exact: true }).click(); await expect(d.getByRole("heading", { name: "Review", exact: true })).toBeVisible();
  const preview = api.calls.find((call) => call.method === "UpdateCluster")!.body; expect(String(preview.updateMask).split(",").sort()).toEqual(["spec.access.oidc", "spec.aws.addons", "spec.aws.controlPlaneLogGroup", "spec.aws.enabledClusterLogTypes"].sort()); expect(api.unexpected).toEqual([]);
});

test("an untouched edit walks every step; the YAML view changes one field and only that field is masked", async ({ page }) => {
  const api = await mock(page), d = await edit(page);
  for (const heading of ["Access"]) { await d.getByRole("button", { name: "Next", exact: true }).click(); await expect(d.getByRole("heading", { name: heading, exact: true })).toBeVisible(); }
  await d.getByRole("button", { name: "Next", exact: true }).click();
  await d.getByRole("button", { name: "Preview update plan", exact: true }).click();
  await expect(d).toContainText("No changes to preview");
  expect(api.calls.filter((call) => call.method === "UpdateCluster")).toHaveLength(0);
  await d.getByRole("radio", { name: "YAML", exact: true }).click();
  const editor = d.getByRole("textbox", { name: "Cluster YAML", exact: true });
  await expect(editor).toContainText("name: production");
  // The stored spec with one change: the mask is the diff, not the document.
  await editor.fill(stringify({ apiVersion: "cluster-vending.io/v1", kind: "Cluster", metadata: { name: "production", labels: { environment: "production" } }, spec: { ...baselineSpec, maintenanceWindow: { ...baselineSpec.maintenanceWindow, start: "05:00" } } }));
  await d.getByRole("button", { name: "Preview update plan", exact: true }).click(); await expect(d.getByRole("heading", { name: "Review", exact: true })).toBeVisible();
  const preview = api.calls.find((call) => call.method === "UpdateCluster")!.body;
  expect(preview.updateMask).toBe("spec.maintenanceWindow");
  expect(preview.cluster).toEqual({ id: clusterId, etag: "cluster-etag", spec: { maintenanceWindow: { days: ["WEEKDAY_MONDAY"], start: "05:00", duration: "4h" } } });
  await d.getByRole("button", { name: "Back", exact: true }).click(); await d.getByRole("radio", { name: "Form", exact: true }).click();
  await expect(input(d, "Maintenance start (UTC)")).toHaveCount(0);
  expect(api.unexpected).toEqual([]);
});

test("mobile map rows keep stable input identities through key edits, removal, mode switches and validation", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 812 }); const api = await mock(page), d = await create(page);
  await d.getByRole("button", { name: "Add cloud tag", exact: true }).click(); await input(d, "Cloud tag 1 key").fill("team"); await input(d, "Cloud tag 1 value").fill("first");
  await d.getByRole("button", { name: "Add cloud tag", exact: true }).click(); await input(d, "Cloud tag 2 key").fill("team"); await input(d, "Cloud tag 2 value").fill("second"); const id = await input(d, "Cloud tag 2 key").getAttribute("id");
  await d.getByRole("button", { name: "Next", exact: true }).click(); await expect(d).toContainText("Cloud tag 2: This key already appears in row 1.");
  await expect(input(d, "Cloud tag 2 key")).toHaveAttribute("aria-invalid", "true");
  await d.getByRole("button", { name: "Remove cloud tag 1", exact: true }).click(); await expect(input(d, "Cloud tag 1 key")).toHaveAttribute("id", id!); await expect(input(d, "Cloud tag 1 value")).toHaveValue("second");
  await input(d, "Cloud tag 1 key").fill("owner"); await expect(input(d, "Cloud tag 1 key")).toBeFocused(); await d.getByRole("radio", { name: "YAML", exact: true }).click(); await expect(d.getByRole("textbox", { name: "Cluster YAML", exact: true })).toContainText("owner: second"); await d.getByRole("radio", { name: "Form", exact: true }).click(); await expect(input(d, "Cloud tag 1 key")).toHaveValue("owner"); await expect(input(d, "Cloud tag 1 value")).toHaveValue("second");
  await d.getByRole("button", { name: "Next", exact: true }).click(); await expect(d.getByRole("heading", { name: "Access", exact: true })).toBeFocused(); expect(api.calls.filter((call) => call.method === "CreateCluster")).toHaveLength(0); expect(api.unexpected).toEqual([]);
});


test("edit displays the current placement and can remove every cloud tag", async ({ page }) => {
  const api = await mock(page), d = await edit(page);
  await expect(d.locator("[data-workflow-rail]")).toContainText("Production network");
  await expect(d.getByRole("radio", { name: "Form", exact: true })).toBeChecked();
  await d.getByRole("button", { name: "Remove cloud tag 1", exact: true }).click();
  await expect(d.getByRole("button", { name: "Add cloud tag", exact: true })).toBeFocused();
  for (let next = 0; next < 2; next++) await d.getByRole("button", { name: "Next", exact: true }).click();
  await d.getByRole("button", { name: "Preview update plan", exact: true }).click();
  await expect(d).toContainText("Remove all");
  const preview = api.calls.filter(call => call.method === "UpdateCluster");
  expect(preview).toHaveLength(1);
  expect(preview[0].body).toMatchObject({ validateOnly: true, updateMask: "labels" });
});

for (const mode of ["create", "edit"] as const) test(`no step of the ${mode} wizard offers node group settings`, async ({ page }) => {
  const api = await mock(page), d = mode === "create" ? await create(page) : await edit(page);
  await expect(d.getByRole("region", { name: "Node groups", exact: true })).toContainText("Managed by the platform");
  for (const heading of ["Access", "Add-ons and logging"]) {
    for (const name of ["Add node group", "Add node taint"]) await expect(d.getByRole("button", { name, exact: true })).toHaveCount(0);
    for (const name of ["Instance types", "AWS AMI type", "Root disk (GiB)", "Minimum nodes"]) await expect(d.getByRole("textbox", { name, exact: true }).or(d.getByRole("combobox", { name, exact: true }))).toHaveCount(0);
    await expect(d.getByRole("checkbox", { name: "Use a launch template", exact: true })).toHaveCount(0);
    // Leaving Access needs a reachable endpoint: a new cluster's public endpoint has no allowed CIDR yet.
    if (mode === "create" && heading === "Add-ons and logging") await d.getByRole("checkbox", { name: "Public endpoint", exact: true }).uncheck();
    await d.getByRole("button", { name: "Next", exact: true }).click(); await expect(d.getByRole("heading", { name: heading, exact: true })).toBeVisible();
  }
  await expect(d.getByRole("heading", { name: "Compute", exact: true })).toHaveCount(0);
  expect(api.calls.some(call => call.method === "CreateCluster" || call.method === "UpdateCluster")).toBe(false);
});
