import { expect, test, type Page } from "@playwright/test";
const clusterId = "00000000-0000-4000-8000-000000000003";
const cloudResources = { iamRole: { access: "COMPONENT_IAM_ACCESS_POD_IDENTITY", serviceAccounts: [{ namespace: "external-dns", name: "external-dns" }], managedPolicyArns: ["arn:aws:iam::aws:policy/AmazonRoute53ReadOnlyAccess"], policy: '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"route53:ChangeResourceRecordSets","Resource":"*"}]}' } };
const definition = { name: "external-dns", description: "Synchronize DNS records.", management: "COMPONENT_MANAGEMENT_TENANT", provider: "CLOUD_PROVIDER_AWS", configurable: true, requires: ["policies"], versions: { "1.35": "0.16.0" }, platformValues: { policy: "sync" }, valuesSchema: { type: "object", properties: { replicaCount: { type: "integer" } } }, cloudResources };
async function mockCatalog(page: Page, allowCluster = false) {
  const calls: { method: string; body: Record<string, unknown> }[] = [], unexpected: string[] = [];
  await page.route("**/api/rpc/**", async (route) => {
    const method = new URL(route.request().url()).pathname.split("/").at(-1)!;
    const body = route.request().postDataJSON() as Record<string, unknown>;
    calls.push({ method, body });
    let response: object;
    switch (method) {
      case "WhoAmI": response = { anonymous: true }; break;
      case "ListTenants": response = { tenants: [{ id: "acme" }] }; break;
      case "ListComponentDefinitions": response = { definitions: [definition, { ...definition, name: "policies", requires: [], cloudResources: undefined }, { ...definition, name: "blocked", disabled: true, disabledReason: "Disabled by catalog policy" }] }; break;
      case "GetComponentValues": response = { userValues: "# Replica count\nreplicaCount: 2\n" }; break;
      case "GetCluster": if (allowCluster) { response = { cluster: { id: clusterId, tenantId: "acme", name: "production", spec: {}, status: { state: "CLUSTER_STATE_RUNNING" } } }; break; } return fail();
      case "ListClusterEvents": if (allowCluster) { response = { events: [] }; break; } return fail();
      case "ListComponents": if (allowCluster) { response = { components: [{ tenantId: "acme", clusterId, component: "policies", management: "COMPONENT_MANAGEMENT_TENANT", status: { phase: "COMPONENT_PHASE_READY" } }] }; break; } return fail();
      default: return fail();
    }
    async function fail() { unexpected.push(method); await route.fulfill({ status: 501, contentType: "application/json", body: JSON.stringify({ code: "unimplemented", message: `Unexpected RPC ${method}` }) }); }
    await route.fulfill({ contentType: "application/json", body: JSON.stringify(response) });
  });
  return { calls, unexpected };
}
async function expectPureCatalog(page: Page) {
  await expect(page.getByRole("combobox", { name: "Cluster", exact: true })).toHaveCount(0);
  await expect(page.getByRole("button", { name: /^(Refresh clusters|Enable component|Manage component|Install|Update values|Disable|Delete)$/ })).toHaveCount(0);
  await expect(page.getByRole("table", { name: "Installed components", exact: true })).toHaveCount(0);
  await expect(page.getByText("Select a cluster", { exact: true })).toHaveCount(0);
}

test("catalog ignores cluster query and keeps searchable definitions, filters and clean detail links", async ({ page }) => {
  const mock = await mockCatalog(page);
  await page.goto(`/components?cluster=${clusterId}`);
  const table = page.getByRole("table", { name: "Component catalog", exact: true });
  await expect(table).toContainText("external-dns"); await expectPureCatalog(page);
  await expect(table.getByRole("link", { name: "external-dns", exact: true })).toHaveAttribute("href", "/components/external-dns");
  await page.getByRole("searchbox", { name: "Search component catalog", exact: true }).fill("policies");
  await expect(table).toContainText("policies");
  await page.getByRole("button", { name: "Clear catalog filters", exact: true }).click();
  await page.getByRole("combobox", { name: "Catalog availability", exact: true }).click(); await page.getByRole("option", { name: "Unavailable", exact: true }).click();
  await expect(table).toContainText("Disabled by catalog policy"); await expect(table.getByRole("link", { name: "external-dns", exact: true })).toHaveCount(0);
  await page.getByRole("button", { name: "Refresh catalog", exact: true }).click();
  await expect.poll(() => mock.calls.filter((call) => call.method === "ListComponentDefinitions").length).toBeGreaterThan(1);
  expect(mock.calls.filter((call) => call.method === "ListComponentDefinitions").every((call) => call.body.tenantId === "acme" && !call.body.clusterId)).toBe(true);
  expect(mock.unexpected).toEqual([]);
});

test("component details remain read-only catalog with release versions, templates, schema and keyboard tabs", async ({ page }) => {
  const mock = await mockCatalog(page);
  await page.goto(`/components/external-dns?cluster=${clusterId}`);
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("external-dns"); await expectPureCatalog(page);
  const overview = page.getByRole("tabpanel", { name: "Overview", exact: true });
  await expect(overview.getByRole("table", { name: "Component release versions", exact: true })).toContainText("0.16.0");
  await expect(overview.getByRole("link", { name: "policies", exact: true })).toHaveAttribute("href", "/components/policies");
  const tab = page.getByRole("tab", { name: "Values", exact: true }); await tab.focus(); await tab.press("Enter");
  const values = page.getByRole("tabpanel", { name: "Values", exact: true });
  await expect(values.getByRole("textbox", { name: "Default config", exact: true })).toHaveAttribute("aria-readonly", "true");
  await expect(values.getByRole("textbox", { name: "Helm values template", exact: true })).toContainText("# Replica count");
  await expect(values.getByRole("textbox", { name: "Helm values template", exact: true })).toHaveAttribute("aria-readonly", "true");
  await expect(page.getByRole("textbox", { name: "Tenant values", exact: true })).toHaveCount(0);
  await page.getByRole("tab", { name: "Schema", exact: true }).click();
  await expect(page.getByRole("tabpanel", { name: "Values", exact: true })).toHaveCount(0);
  await expect(page.getByRole("tabpanel", { name: "Schema", exact: true }).getByRole("textbox", { name: "Values schema", exact: true })).toContainText("replicaCount");
  await page.getByRole("button", { name: "Refresh component", exact: true }).click();
  await expect.poll(() => mock.calls.filter((call) => call.method === "ListComponentDefinitions").length).toBeGreaterThan(1);
  expect(mock.calls.filter((call) => call.method === "ListComponentDefinitions").every((call) => !call.body.clusterId)).toBe(true);
  expect(mock.unexpected).toEqual([]);
});

test("cluster detail retains management and treats id-less READY tenant entries as Disabled", async ({ page }) => {
  const mock = await mockCatalog(page, true);
  await page.goto(`/clusters/${clusterId}?tab=components`);
  const table = page.getByRole("table", { name: "Cluster tenant component catalog", exact: true });
  const policies = table.getByRole("row").filter({ has: page.getByRole("link", { name: "policies", exact: true }) });
  await expect(policies).toContainText("Disabled"); await expect(policies).not.toContainText("Ready");
  await expect(policies.getByRole("button", { name: "Enable component", exact: true })).toBeEnabled();
  expect(mock.calls.some((call) => call.method === "ListComponents" && call.body.clusterId === clusterId)).toBe(true);
  expect(mock.unexpected).toEqual([]);
});

test("component details show the IAM role it creates in the tenant's account, read-only", async ({ page }) => {
  const mock = await mockCatalog(page);
  await page.goto("/components/external-dns");
  const section = page.getByRole("region", { name: "Cloud resources", exact: true });
  await expect(section).toContainText("Created in your tenant's AWS account");
  await expect(section).toContainText("EKS Pod Identity");
  await expect(section).toContainText("external-dns/external-dns");
  await expect(section).toContainText("arn:aws:iam::aws:policy/AmazonRoute53ReadOnlyAccess");
  const policy = section.getByRole("textbox", { name: "IAM policy document", exact: true });
  await expect(policy).toContainText("route53:ChangeResourceRecordSets");
  await expect(policy).toHaveAttribute("aria-readonly", "true");
  await page.goto("/components/policies");
  await expect(page.getByRole("region", { name: "Cloud resources", exact: true })).toContainText("creates no cloud resources");
  expect(mock.unexpected).toEqual([]);
});
