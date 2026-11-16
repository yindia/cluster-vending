import { expect, test, type Locator, type Page } from "@playwright/test";

const accountId = "00000000-0000-4000-8000-000000000001", networkId = "00000000-0000-4000-8000-000000000002", clusterId = "00000000-0000-4000-8000-000000000003";
const keyArn = "arn:aws:kms:us-west-1:111122223333:key/00000000-0000-4000-8000-000000000007";
const oidc = "name: workforce\nissuerUrl: https://issuer.example.com\nclientId: cluster-users\ngroupsClaim: groups\n";
const accessEntries = "- principalArn: arn:aws:iam::111122223333:role/developers\n  policies:\n    - policyArn: arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy\n";
const baselineSpec = {
  region: "us-west-1", network: { networkId, zoneIds: ["usw1-az1", "usw1-az2"], serviceCidr: "172.20.0.0/16" }, releaseChannel: "RELEASE_CHANNEL_STABLE", upgradeMode: "UPGRADE_MODE_MANUAL",
  maintenanceWindow: { days: ["WEEKDAY_MONDAY"], start: "02:00", duration: "4h" },
  controlPlane: { kubernetesVersion: "1.35", endpointAccess: { endpointPrivateAccess: true, endpointPublicAccess: false }, secretsEncryption: { enabled: true, kmsKeyArn: keyArn } },
  defaultNodePool: { instanceTypes: ["m6i.large"], labels: { role: "worker" }, taints: [{ key: "dedicated", value: "payments", effect: 1 }], scalingConfig: { minSize: 1, maxSize: 3, desiredSize: 2 } },
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

test("guided create reviews and sends tags, node labels, all taint effects and advanced typed fields", async ({ page }) => {
  test.setTimeout(60_000);
  const api = await mock(page), d = await create(page);
  await d.getByRole("button", { name: "Cloud tags", exact: true }).click(); await d.getByRole("button", { name: "Add cloud tag", exact: true }).click();
  await input(d, "Cloud tag 1 key").fill("team"); await expect(input(d, "Cloud tag 1 key")).toBeFocused(); await input(d, "Cloud tag 1 value").fill("platform");
  await d.getByRole("button", { name: "Maintenance window", exact: true }).click(); await d.getByRole("checkbox", { name: "Monday", exact: true }).check(); await input(d, "Maintenance start (UTC)").fill("02:00"); await input(d, "Maintenance duration").fill("90m");
  await d.getByRole("button", { name: "Next", exact: true }).click(); await expect(d.getByRole("heading", { name: "Compute", exact: true })).toBeFocused();
  await input(d, "Initial desired nodes").fill("2"); await d.getByRole("button", { name: "Node labels and taints", exact: true }).click(); await d.getByRole("button", { name: "Add node label", exact: true }).click(); await input(d, "Node label 1 key").fill("role"); await input(d, "Node label 1 value").fill("worker");
  for (const [index, effect] of ["No schedule", "Prefer no schedule", "No execute"].entries()) {
    await d.getByRole("button", { name: "Add node taint", exact: true }).click(); await input(d, `Node taint ${index + 1} key`).fill(`dedicated-${index}`); await input(d, `Node taint ${index + 1} value`).fill("payments");
    await choose(page, d, `Node taint ${index + 1} effect`, effect);
  }
  await d.getByRole("button", { name: "AWS node image and launch template", exact: true }).click(); await choose(page, d, "AWS AMI type", "Al2023 x86 64 standard"); await input(d, "AWS launch template (YAML)").fill('id: lt-0123456789abcdef0\nversion: "1"\n');
  await d.getByRole("button", { name: "Next", exact: true }).click(); await d.getByRole("checkbox", { name: "Public endpoint", exact: true }).uncheck();
  await d.getByRole("button", { name: "Cluster network settings", exact: true }).click(); await input(d, "Availability zone IDs").fill("usw1-az1,usw1-az2"); await input(d, "Kubernetes service CIDR").fill("172.20.0.0/16"); await input(d, "Additional cluster security groups").fill("sg-0123456789abcdef0");
  await d.getByRole("button", { name: "Secrets encryption", exact: true }).click(); await choose(page, d, "Encrypt Kubernetes Secrets", "Enabled"); await input(d, "Encryption KMS key ARN").fill(keyArn);
  await d.getByRole("button", { name: "OIDC and access entries", exact: true }).click(); await choose(page, d, "AWS authentication mode", "Api"); await input(d, "OIDC provider (YAML)").fill(oidc); await input(d, "Access entries (YAML)").fill(accessEntries);
  await d.getByRole("button", { name: "AWS support and control plane logging", exact: true }).click(); await choose(page, d, "AWS support policy", "Extended"); await choose(page, d, "Managed control plane log group", "Enabled"); await input(d, "Control plane log retention (days)").fill("30"); await d.getByRole("checkbox", { name: "Api", exact: true }).check(); await d.getByRole("checkbox", { name: "Audit", exact: true }).check();
  await d.getByRole("button", { name: "AWS add-ons", exact: true }).click(); await input(d, "AWS add-ons (YAML)").fill("- name: metrics-server\n"); await d.getByRole("button", { name: "Next", exact: true }).click();
  await expect(d.getByRole("heading", { name: "Review", exact: true })).toBeFocused(); await expect(d).toContainText("team=platform"); await expect(d).toContainText("dedicated-2=payments:NO_EXECUTE");
  expect(api.calls.filter((call) => call.method === "CreateCluster")).toHaveLength(0); await d.getByRole("button", { name: "Create cluster", exact: true }).click(); await expect(d).toHaveCount(0);
  const body = api.calls.find((call) => call.method === "CreateCluster")!.body;
  expect(body.cluster).toMatchObject({ labels: { team: "platform" }, spec: { maintenanceWindow: { days: ["WEEKDAY_MONDAY"], start: "02:00", duration: "90m" }, network: { zoneIds: ["usw1-az1", "usw1-az2"], serviceCidr: "172.20.0.0/16" }, defaultNodePool: { labels: { role: "worker" }, scalingConfig: { desiredSize: 2 }, taints: [{ key: "dedicated-0", value: "payments", effect: "TAINT_EFFECT_NO_SCHEDULE" }, { key: "dedicated-1", value: "payments", effect: "TAINT_EFFECT_PREFER_NO_SCHEDULE" }, { key: "dedicated-2", value: "payments", effect: "TAINT_EFFECT_NO_EXECUTE" }] }, controlPlane: { secretsEncryption: { enabled: true, kmsKeyArn: keyArn } }, access: { oidc: { name: "workforce", issuerUrl: "https://issuer.example.com" }, accessEntries: [{ principalArn: "arn:aws:iam::111122223333:role/developers" }] }, aws: { defaultNodePool: { amiType: "AWS_AMI_TYPE_AL2023_X86_64_STANDARD", launchTemplate: { id: "lt-0123456789abcdef0", version: "1" } }, additionalSecurityGroupIds: ["sg-0123456789abcdef0"], enabledClusterLogTypes: ["AWS_CLUSTER_LOG_TYPE_API", "AWS_CLUSTER_LOG_TYPE_AUDIT"], upgradePolicy: { supportType: "AWS_SUPPORT_TYPE_EXTENDED" }, controlPlaneLogGroup: { enabled: true, retentionDays: 30 }, addons: [{ name: "metrics-server" }] } } });
  expect(api.unexpected).toEqual([]);
});

test("edit preserves immutable values and encryption while sending sparse tag, label, taint and maintenance masks", async ({ page }) => {
  const api = await mock(page), d = await edit(page);
  await d.getByRole("button", { name: "Cloud tags", exact: true }).click(); await input(d, "Cloud tag 1 value").fill("staging"); await d.getByRole("button", { name: "Maintenance window", exact: true }).click(); await input(d, "Maintenance start (UTC)").fill("03:00"); await d.getByRole("button", { name: "Next", exact: true }).click();
  await expect(input(d, "Initial desired nodes")).toHaveAttribute("readonly", ""); await expect(input(d, "Initial desired nodes")).toHaveValue("2");
  await d.getByRole("button", { name: "Node labels and taints", exact: true }).click(); await input(d, "Node label 1 value").fill("compute"); await d.getByRole("button", { name: "Remove node taint 1", exact: true }).click(); await d.getByRole("button", { name: "Next", exact: true }).click();
  await d.getByRole("button", { name: "Cluster network settings", exact: true }).click(); for (const label of ["Availability zone IDs", "Kubernetes service CIDR", "Additional cluster security groups"]) await expect(input(d, label)).toHaveAttribute("readonly", "");
  await d.getByRole("button", { name: "Secrets encryption", exact: true }).click(); await expect(input(d, "Encryption KMS key ARN")).toHaveAttribute("readonly", ""); await expect(input(d, "Encryption KMS key ARN")).toHaveValue(keyArn); await expect(d.getByRole("combobox", { name: "Encrypt Kubernetes Secrets", exact: true })).toBeDisabled();
  await d.getByRole("button", { name: "OIDC and access entries", exact: true }).click(); await expect(d.getByRole("combobox", { name: "AWS authentication mode", exact: true })).toBeDisabled();
  await d.getByRole("button", { name: "Preview update plan", exact: true }).click(); await expect(d).toContainText("environment=staging"); await expect(d).toContainText("role=compute");
  const preview = api.calls.find((call) => call.method === "UpdateCluster")!.body;
  expect(String(preview.updateMask).split(",").sort()).toEqual(["labels", "spec.defaultNodePool.labels", "spec.defaultNodePool.taints", "spec.maintenanceWindow"].sort());
  expect(preview.cluster).toEqual({ id: clusterId, etag: "cluster-etag", labels: { environment: "staging" }, spec: { defaultNodePool: { labels: { role: "compute" } }, maintenanceWindow: { days: ["WEEKDAY_MONDAY"], start: "03:00", duration: "4h" } } });
  await d.getByRole("button", { name: "Apply reviewed plan", exact: true }).click(); await expect(d).toHaveCount(0); const applied = api.calls.filter((call) => call.method === "UpdateCluster")[1].body; expect(applied.cluster).toEqual(preview.cluster); expect(applied.updateMask).toBe(preview.updateMask); expect(applied.planId).toBe("guided-plan"); expect(api.unexpected).toEqual([]);
});

test("advanced edits use their own masks and keep malformed YAML on the Access step", async ({ page }) => {
  const api = await mock(page), d = await edit(page); await d.getByRole("button", { name: "Next", exact: true }).click(); await d.getByRole("button", { name: "Next", exact: true }).click();
  await d.getByRole("button", { name: "OIDC and access entries", exact: true }).click(); await input(d, "OIDC provider (YAML)").fill("unknownField: true"); await d.getByRole("button", { name: "Preview update plan", exact: true }).click(); await expect(d.getByRole("heading", { name: "Access", exact: true })).toBeVisible(); expect(api.calls.filter((call) => call.method === "UpdateCluster")).toHaveLength(0);
  await expect(input(d, "OIDC provider (YAML)")).toContainText("unknownField"); await input(d, "OIDC provider (YAML)").fill(oidc);
  await d.getByRole("button", { name: "AWS support and control plane logging", exact: true }).click(); await d.getByRole("checkbox", { name: "Audit", exact: true }).check(); await input(d, "Control plane log retention (days)").fill("90");
  await d.getByRole("button", { name: "AWS add-ons", exact: true }).click(); await input(d, "AWS add-ons (YAML)").fill("- name: metrics-server\n"); await d.getByRole("button", { name: "Preview update plan", exact: true }).click(); await expect(d.getByRole("heading", { name: "Review", exact: true })).toBeVisible();
  const preview = api.calls.find((call) => call.method === "UpdateCluster")!.body; expect(String(preview.updateMask).split(",").sort()).toEqual(["spec.access.oidc", "spec.aws.addons", "spec.aws.controlPlaneLogGroup", "spec.aws.enabledClusterLogTypes"].sort()); expect(api.unexpected).toEqual([]);
});

test("mobile map rows keep stable input identities through key edits, removal, mode switches and validation", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 812 }); const api = await mock(page), d = await create(page);
  await d.getByRole("button", { name: "Cloud tags", exact: true }).click(); await d.getByRole("button", { name: "Add cloud tag", exact: true }).click(); await input(d, "Cloud tag 1 key").fill("team"); await input(d, "Cloud tag 1 value").fill("first");
  await d.getByRole("button", { name: "Add cloud tag", exact: true }).click(); await input(d, "Cloud tag 2 key").fill("team"); await input(d, "Cloud tag 2 value").fill("second"); const id = await input(d, "Cloud tag 2 key").getAttribute("id");
  await d.getByRole("button", { name: "Next", exact: true }).click(); await expect(d).toContainText("Tag and label keys must be unique");
  await d.getByRole("button", { name: "Remove cloud tag 1", exact: true }).click(); await expect(input(d, "Cloud tag 1 key")).toHaveAttribute("id", id!); await expect(input(d, "Cloud tag 1 value")).toHaveValue("second");
  await input(d, "Cloud tag 1 key").fill("owner"); await expect(input(d, "Cloud tag 1 key")).toBeFocused(); await d.getByRole("radio", { name: "Paste YAML", exact: true }).click(); await d.getByRole("radio", { name: "Guided form", exact: true }).click(); await d.getByRole("button", { name: "Cloud tags", exact: true }).click(); await expect(input(d, "Cloud tag 1 value")).toHaveValue("second");
  await d.getByRole("button", { name: "Next", exact: true }).click(); await expect(d.getByRole("heading", { name: "Compute", exact: true })).toBeFocused(); expect(api.calls.filter((call) => call.method === "CreateCluster")).toHaveLength(0); expect(api.unexpected).toEqual([]);
});


test("existing numeric taint effects hydrate, retain their selection and review without changing the effect", async ({ page }) => {
  const api = await mock(page), d = await edit(page);
  await d.getByRole("button", { name: "Next", exact: true }).click();
  await d.getByRole("button", { name: "Node labels and taints", exact: true }).click();
  await expect(d.getByRole("combobox", { name: "Node taint 1 effect", exact: true })).toHaveText("No schedule");
  await expect(input(d, "Node taint 1 key")).toHaveValue("dedicated");
  await input(d, "Node taint 1 value").fill("batch");
  await d.getByRole("button", { name: "Next", exact: true }).click();
  await d.getByRole("button", { name: "Preview update plan", exact: true }).click();
  await expect(d.getByRole("heading", { name: "Review", exact: true })).toBeVisible();
  await expect(d).toContainText("dedicated=batch:NO_SCHEDULE");
  const preview = api.calls.find((call) => call.method === "UpdateCluster")!.body;
  expect(preview.updateMask).toBe("spec.defaultNodePool.taints");
  expect(preview.cluster).toEqual({ id: clusterId, etag: "cluster-etag", spec: { defaultNodePool: { taints: [{ key: "dedicated", value: "batch", effect: "TAINT_EFFECT_NO_SCHEDULE" }] } } });
  await d.getByRole("button", { name: "Apply reviewed plan", exact: true }).click(); await expect(d).toHaveCount(0);
  expect(api.calls.filter((call) => call.method === "UpdateCluster")[1].body.cluster).toEqual(preview.cluster);
  expect(api.unexpected).toEqual([]);
});
