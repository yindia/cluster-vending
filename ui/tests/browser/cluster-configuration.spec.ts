import { expect, test, type Page } from "@playwright/test";

const id = "00000000-0000-4000-8000-000000000003";
const networkId = "00000000-0000-4000-8000-000000000002";
const advancedSpec = {
  region: "us-east-1", releaseChannel: 3, upgradeMode: 2, naas: true,
  network: { networkId, zoneIds: ["use1-az1", "use1-az2"], serviceCidr: "172.20.0.0/16" },
  controlPlane: { kubernetesVersion: "1.35", endpointAccess: { endpointPrivateAccess: true, endpointPublicAccess: false, publicAccessCidrs: [] }, secretsEncryption: { enabled: true, kmsKeyArn: "arn:aws:kms:us-east-1:111122223333:key/config-key" } },
  defaultNodePool: { instanceTypes: ["m6i.large"], capacityType: 2, diskSizeGb: 80, scalingConfig: { minSize: 0, maxSize: 8, desiredSize: 0 }, labels: { workload: "batch" }, taints: [{ key: "dedicated", value: "batch", effect: 1 }] },
  maintenanceWindow: { days: [1, 5], start: "03:30", duration: "2h" },
  access: {
    adminPrincipals: ["arn:aws:iam::111122223333:role/config-admin"],
    accessEntries: [{ principalArn: "arn:aws:iam::111122223333:role/config-reader", type: 1, policies: [{ policyArn: "arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy", scope: 2, namespaces: ["payments"] }] }],
    oidc: { name: "corporate", issuerUrl: "https://login.example.test", clientId: "config-client", groupsClaim: "groups", usernameClaim: "sub", requiredClaims: { environment: "production" }, entra: { tenantId: "directory-id", clientId: "entra-client" } },
  },
  aws: {
    irsa: false, accessConfig: { authenticationMode: 2 }, enabledClusterLogTypes: [1, 2], upgradePolicy: { supportType: 1 },
    additionalSecurityGroupIds: ["sg-config-control"], controlPlaneLogGroup: { enabled: true, retentionDays: 30 },
    defaultNodePool: { amiType: 20, launchTemplate: { id: "lt-config-nodes", version: "7", securityGroupIds: ["sg-config-nodes"], imageId: "ami-config-image", userData: "opaque-user-data-not-for-metadata" } },
    addons: [{ name: "aws-mountpoint-s3-csi-driver", version: "v1.2.3-eksbuild.1", configurationValues: '{"node":{"logLevel":"debug"}}', s3BucketNames: ["config-bucket"] }],
  },
};

async function mockConfiguration(page: Page, spec: object) {
  const calls: { method: string; body: Record<string, unknown> }[] = [];
  const unexpected: string[] = [];
  const cluster = { tenantId: "acme", id, name: "config-cluster", displayName: "Configuration test", labels: { team: "platform" }, spec,
    status: { state: "CLUSTER_STATE_RUNNING", currentKubernetesVersion: "1.30-observed-only", message: "Observed status is separate", endpoint: { url: "https://observed-endpoint.example.test" } } };
  await page.route("**/api/rpc/**", async (route) => {
    const method = new URL(route.request().url()).pathname.split("/").at(-1)!;
    const body = route.request().postDataJSON() as Record<string, unknown>;
    calls.push({ method, body });
    let result: object;
    switch (method) {
      case "WhoAmI": result = { subject: "config-viewer", grants: [{ role: "ACCESS_ROLE_TENANT_VIEWER", tenants: ["acme"] }] }; break;
      case "ListTenants": result = { tenants: [{ id: "acme" }] }; break;
      case "GetCluster": result = { cluster, asRequested: true }; break;
      case "GetNetwork": result = { network: { tenantId: "acme", id: networkId, name: "config-network", spec: { region: "us-east-1" }, status: { state: "NETWORK_STATE_READY" } } }; break;
      case "ListClusterEvents": result = { events: [] }; break;
      default:
        unexpected.push(method);
        await route.fulfill({ status: 501, contentType: "application/json", body: JSON.stringify({ code: "unimplemented", message: `Unmocked ${method}` }) });
        return;
    }
    await route.fulfill({ contentType: "application/json", body: JSON.stringify(result) });
  });
  return { calls, unexpected };
}

async function openConfiguration(page: Page) {
  await page.goto(`/clusters/${id}`);
  await page.getByRole("tab", { name: "Configuration", exact: true }).click();
  return page.getByRole("tabpanel", { name: "Configuration", exact: true });
}

test("viewer can inspect full advanced desired configuration, without observed status leakage", async ({ page }) => {
  const api = await mockConfiguration(page, advancedSpec);
  const panel = await openConfiguration(page);
  for (const heading of ["Cluster settings", "Upgrades", "Network", "Control plane", "Default node pool", "Access", "Maintenance window", "Amazon Web Services configuration"]) {
    await expect(panel.getByRole("heading", { name: heading, exact: true })).toBeVisible();
  }
  for (const value of ["1.35", "172.20.0.0/16", "use1-az2", "m6i.large", "dedicated", "03:30 UTC", "Monday, Friday", "2h", "payments", "config-reader", "config-client", "entra-client", "lt-config-nodes", "sg-config-control", "sg-config-nodes", "ami-config-image"]) {
    await expect(panel).toContainText(value);
  }
  await expect(panel.getByRole("button", { name: "Copy Network ID", exact: true })).toBeVisible();
  await expect(panel.getByRole("button", { name: "Copy Secrets encryption / KMS key ARN", exact: true })).toBeVisible();
  await expect(panel).toContainText("Configured (user data payload)");
  await expect(panel).not.toContainText("opaque-user-data-not-for-metadata");
  await expect(panel).not.toContainText("1.30-observed-only");
  await expect(panel).not.toContainText("observed-endpoint.example.test");
  await expect(panel).not.toContainText("Observed status is separate");
  await expect(panel).not.toContainText("aws-mountpoint-s3-csi-driver");
  await expect(panel).not.toContainText("config-bucket");
  await expect(panel).not.toContainText("logLevel");
  await expect(panel.getByRole("button", { name: "View YAML", exact: true })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "View YAML", exact: true })).toHaveCount(1);
  await page.screenshot({ path: test.info().outputPath("configuration-desktop.png") });
  await page.setViewportSize({ width: 390, height: 844 });
  await panel.getByRole("heading", { name: "Maintenance window", exact: true }).scrollIntoViewIfNeeded();
  const bounds = await panel.boundingBox();
  expect(bounds).not.toBeNull();
  expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(391);
  await page.screenshot({ path: test.info().outputPath("configuration-mobile.png") });
  expect(api.unexpected).toEqual([]);
});

test("configuration keeps false and zero while omitting empty optional settings", async ({ page }) => {
  const api = await mockConfiguration(page, { region: "us-east-1", naas: false, defaultNodePool: { scalingConfig: { minSize: 0, maxSize: 0, desiredSize: 0 } }, controlPlane: { endpointAccess: { endpointPublicAccess: false } }, aws: { irsa: false } });
  const panel = await openConfiguration(page);
  async function value(label: string, expected: string) {
    const term = panel.locator("dt").filter({ hasText: new RegExp(`^${label.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}$`) });
    await expect(term).toHaveCount(1);
    await expect(term.locator("+ dd")).toHaveText(expected);
  }
  await value("Namespace as a Service", "Disabled");
  await value("Scaling / Initial desired nodes", "0");
  await value("Scaling / Minimum nodes", "0");
  await value("API endpoint / Endpoint public access", "Disabled");
  await value("IRSA", "Disabled");
  await expect(panel).toContainText("Any time (no maintenance window configured)");
  await expect(panel).not.toContainText("Not set");
  await expect(panel).not.toContainText("Empty string");
  await expect(panel).not.toContainText("None (empty list)");
  expect(api.unexpected).toEqual([]);
});

test("configuration reuses tenant-scoped read-only YAML viewer and copy", async ({ page }) => {
  const api = await mockConfiguration(page, advancedSpec);
  const panel = await openConfiguration(page);
  await expect(panel.getByRole("button", { name: "View YAML", exact: true })).toHaveCount(0);
  await page.getByRole("button", { name: "View YAML", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Cluster YAML", exact: true });
  await expect(dialog.getByRole("textbox", { name: "Cluster YAML configuration", exact: true })).toBeVisible();
  await expect(dialog.getByRole("button", { name: "Copy Cluster YAML configuration", exact: true })).toBeVisible();
  expect(api.calls.some(({ method, body }) => method === "GetCluster" && body.asRequested === true && body.tenantId === "acme" && body.clusterId === id)).toBe(true);
  expect(api.calls.every(({ method }) => ["WhoAmI", "ListTenants", "GetCluster", "GetNetwork", "ListClusterEvents"].includes(method))).toBe(true);
  expect(api.unexpected).toEqual([]);
});

test("maintenance window stays visible in manual mode and empty days means every day", async ({ page }) => {
  await mockConfiguration(page, { ...advancedSpec, upgradeMode: 1, maintenanceWindow: { days: [], start: "22:30", duration: "90m" } });
  const panel = await openConfiguration(page);
  const window = panel.getByRole("region", { name: "Maintenance window", exact: true });
  await expect(window).toContainText("Every day");
  await expect(window).toContainText("22:30 UTC");
  await expect(window).toContainText("90m");
  await expect(window).toContainText("Automatic upgrades");
  await expect(panel).toContainText("Manual");
});
