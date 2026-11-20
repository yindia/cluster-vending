import { expect, test, type Locator, type Page } from "@playwright/test";

const networkId = "00000000-0000-4000-8000-000000000002", clusterId = "00000000-0000-4000-8000-000000000003";
type Group = Record<string, unknown> & { name: string };
type Body = { validateOnly?: boolean; planId?: string; updateMask?: string; cluster: { etag: string; spec: { nodeGroups: Group[] } } };
type Call = { method: string; body: Body };
type Options = { viewer?: boolean; failPreview?: string; emptyStatus?: boolean; allow?: { instanceFamilies: string[]; instanceTypes?: string[] }; imported?: boolean };

const initialGroups = (): Group[] => [
  { name: "batch", instanceFamilies: ["m7i"], architectures: ["ARCHITECTURE_AMD64"], capacityTypes: ["CAPACITY_TYPE_ON_DEMAND"], labels: { team: "data" }, taints: [{ key: "dedicated", value: "batch", effect: "TAINT_EFFECT_NO_SCHEDULE" }], limits: { cpu: 64 } },
  { name: "gpu", instanceTypes: ["g5.xlarge"], architectures: ["ARCHITECTURE_AMD64"], capacityTypes: ["CAPACITY_TYPE_SPOT"] },
  { name: "fresh", instanceFamilies: ["c7g"], architectures: ["ARCHITECTURE_ARM64"], capacityTypes: ["CAPACITY_TYPE_ON_DEMAND"] },
];
const nodeGroupStatus = [
  { name: "system", platformManaged: true, phase: "NODE_GROUP_PHASE_READY", message: "", nodes: 2 },
  { name: "batch", phase: "NODE_GROUP_PHASE_READY", message: "", nodes: 4 },
  { name: "gpu", phase: "NODE_GROUP_PHASE_FAILED", message: "No GPU capacity in the selected zones", nodes: 0 },
];

test.beforeEach(async ({ page }) => {
  await page.route("**/api/auth/session", route => route.fulfill({ contentType: "application/json", body: JSON.stringify({ enabled: true, authenticated: true, csrfToken: "browser-test-csrf", username: "test-user" }) }));
});

async function mock(page: Page, options: Options = {}) {
  const calls: Call[] = [], unexpected: string[] = [];
  let groups = initialGroups(), revision = 1;
  const deleting: string[] = [];
  const network = { id: networkId, tenantId: "acme", name: "production", spec: { region: "us-west-2", tier: "TIER_1" } };
  const cluster = () => ({
    id: clusterId, tenantId: "acme", name: "production", etag: `etag-${revision}`, generation: String(revision),
    spec: { region: "us-west-2", network: { networkId }, defaultNodePool: { instanceTypes: ["t3.large"], capacityType: "CAPACITY_TYPE_ON_DEMAND", scalingConfig: { minSize: 2, maxSize: 2 } }, nodeGroups: groups, aws: {} },
    status: { state: "CLUSTER_STATE_RUNNING", observedGeneration: String(revision), nodeGroups: options.emptyStatus ? [] : [...nodeGroupStatus.filter(item => item.platformManaged || groups.some(group => group.name === item.name)), ...deleting.map(name => ({ name, phase: "NODE_GROUP_PHASE_DELETING", message: "Karpenter is draining the nodes", nodes: 1 }))] },
  });
  // What the server renders: the system group first (platform managed, an EKS node group: no YAML), then each user group; YAML text is opaque to the UI.
  const manifests = () => options.imported ? [] : [{ name: "system", platformManaged: true }, ...groups.map(group => ({ name: group.name, platformManaged: false }))].map(item => item.platformManaged ? item : ({
    ...item,
    ec2NodeClassYaml: `apiVersion: karpenter.k8s.aws/v1\nkind: EC2NodeClass\nmetadata:\n  name: ${item.name}\nspec:\n  role: production-karpenter-node\n`,
    nodePoolYaml: `apiVersion: karpenter.sh/v1\nkind: NodePool\nmetadata:\n  name: ${item.name}\nspec:\n  limits:\n    cpu: "64"\n`,
  }));
  await page.route("**/api/rpc/**", async route => {
    const method = new URL(route.request().url()).pathname.split("/").at(-1)!;
    const body = route.request().postDataJSON() as Body;
    calls.push({ method, body });
    let response: object;
    switch (method) {
      case "WhoAmI": response = options.viewer ? { subject: "viewer", grants: [{ role: "ACCESS_ROLE_TENANT_VIEWER", tenants: ["acme"] }] } : { anonymous: true }; break;
      case "ListTenants": response = { tenants: [{ id: "acme" }] }; break;
      case "GetCluster": response = { cluster: cluster(), asRequested: true }; break;
      case "GetNetwork": response = { network }; break;
      case "ListClusterEvents": response = { events: [] }; break;
      case "ListClusterNodeGroupManifests": response = { nodeGroups: manifests() }; break;
      case "GetNodeGroupOptions": response = { ...(options.allow ?? {}), architectures: ["ARCHITECTURE_AMD64", "ARCHITECTURE_ARM64"], capacityTypes: ["CAPACITY_TYPE_ON_DEMAND", "CAPACITY_TYPE_SPOT"] }; break;
      case "UpdateCluster": {
        if (body.validateOnly && options.failPreview) { await route.fulfill({ status: 400, contentType: "application/json", body: JSON.stringify({ code: "failed_precondition", message: options.failPreview }) }); return; }
        const next: Group[] = body.cluster?.spec?.nodeGroups ?? [];
        if (body.validateOnly) {
          const before = new Map(groups.map(group => [group.name, JSON.stringify(group)]));
          const changes = [
            ...next.map(group => ({ action: before.has(group.name) ? (before.get(group.name) === JSON.stringify(group) ? "PLANNED_ACTION_NO_CHANGE" : "PLANNED_ACTION_UPDATE_IN_PLACE") : "PLANNED_ACTION_CREATE", kind: "NodePool", name: group.name, detail: "Karpenter NodePool" })),
            ...groups.filter(group => !next.some(item => item.name === group.name)).map(group => ({ action: "PLANNED_ACTION_DELETE", kind: "NodePool", name: group.name, detail: "Karpenter drains the nodes", retainsCloudResource: false })),
          ].filter(change => change.action !== "PLANNED_ACTION_NO_CHANGE");
          response = { plan: { planId: "node-groups-plan", applicable: true, changes } };
        } else { deleting.push(...groups.filter(group => !next.some(item => item.name === group.name)).map(group => group.name)); groups = next; revision++; response = { cluster: cluster(), updateRecorded: true }; }
        break;
      }
      default: unexpected.push(method); await route.fulfill({ status: 501, contentType: "application/json", body: JSON.stringify({ code: "unimplemented", message: `Unexpected RPC ${method}` }) }); return;
    }
    await route.fulfill({ contentType: "application/json", body: JSON.stringify(response) });
  });
  return { calls, unexpected, updates: () => calls.filter(call => call.method === "UpdateCluster") };
}

async function open(page: Page) {
  await page.goto(`/clusters/${clusterId}?tab=node-groups`);
  return page.getByRole("tabpanel", { name: "Node groups", exact: true });
}
const row = (panel: Locator, name: string) => panel.getByRole("row").filter({ has: panel.page().getByText(name, { exact: true }) });
async function openAction(page: Page, panel: Locator, group: string, action: "Edit" | "Remove") {
  await panel.getByRole("button", { name: `Actions for node group ${group}`, exact: true }).click();
  await page.getByRole("menuitem", { name: action, exact: true }).click();
}
async function addToken(dialog: Locator, label: string, value: string) {
  const field = dialog.getByRole("combobox", { name: label, exact: true });
  await field.fill(value);
  await dialog.page().getByRole("option", { name: new RegExp(value.replace(".", "\\.")) }).first().click();
}
// Required fields append "Required" to their accessible name.
// The dialog's own error region (the design system adds a separate live-region copy).
const alert = (dialog: Locator) => dialog.locator('[role="alert"][tabindex="-1"]');
const text = (dialog: Locator, name: string) => dialog.getByRole("textbox", { name: new RegExp(`^${name.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}( Required)?$`) });

test("the system group is first and read-only, there is no platform row, and user groups show their spec and observed status", async ({ page }) => {
  const api = await mock(page), panel = await open(page);
  const table = panel.getByRole("table", { name: "Cluster node groups", exact: true });
  await expect(table).toContainText("system");
  await expect(row(panel, "system")).toContainText("Managed by platform");
  await expect(row(panel, "system")).toContainText("Ready");
  await expect(row(panel, "system")).toContainText("Locked");
  await expect(row(panel, "system")).toContainText("t3.large");
  await expect(row(panel, "system")).toContainText("2–2 (autoscaled)");
  await expect(row(panel, "system")).toContainText("On-demand");
  await expect(row(panel, "system").getByRole("link", { name: "Open node group system", exact: true })).toHaveAttribute("href", `/clusters/${clusterId}/node-groups/system`);
  await expect(panel.getByRole("button", { name: "Actions for node group system", exact: true })).toHaveCount(0);
  // The system group is an EKS node group, not Karpenter objects: no YAML to show for it.
  await expect(panel.getByRole("button", { name: "View YAML for node group system", exact: true })).toHaveCount(0);
  await expect(row(panel, "platform")).toHaveCount(0);
  await expect(table.getByRole("row")).toHaveCount(5); // header, system, batch, gpu, fresh
  await expect(row(panel, "batch")).toContainText("On-demand");
  await expect(row(panel, "batch")).toContainText("m7i family");
  await expect(row(panel, "batch")).toContainText("dedicated=batch:NoSchedule");
  await expect(row(panel, "batch")).toContainText("64 vCPU");
  await expect(row(panel, "gpu")).toContainText("Spot");
  await expect(row(panel, "gpu")).toContainText("Failed");
  await expect(row(panel, "gpu")).toContainText("No GPU capacity in the selected zones");
  await expect(row(panel, "fresh")).toContainText("Pending");
  await expect(panel.getByRole("button", { name: "Add node group", exact: true })).toBeEnabled();
  expect(api.unexpected).toEqual([]);
});

test("a viewer sees node groups without add, edit or remove controls", async ({ page }) => {
  await mock(page, { viewer: true });
  const panel = await open(page);
  await expect(row(panel, "batch")).toContainText("m7i family");
  await expect(panel.getByRole("button", { name: "Add node group", exact: true })).toHaveCount(0);
  await expect(panel.getByRole("button", { name: /^Actions for node group/ })).toHaveCount(0);
  await expect(panel).toContainText("Read-only access");
});

test("adding a Spot group reviews the plan and sends the full list under the node groups mask with the etag", async ({ page }) => {
  const api = await mock(page), panel = await open(page);
  await panel.getByRole("button", { name: "Add node group", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Add node group", exact: true });
  await text(dialog, "Name").fill("spot-workers");
  await addToken(dialog, "Instance families", "c7g");
  await dialog.getByRole("checkbox", { name: "ARM64", exact: true }).check();
  await dialog.getByRole("checkbox", { name: "AMD64", exact: true }).uncheck();
  await expect(dialog.locator("[data-spot-note]")).toHaveCount(0);
  await dialog.getByRole("checkbox", { name: "Spot", exact: true }).check();
  await dialog.getByRole("checkbox", { name: "On-demand", exact: true }).uncheck();
  await expect(dialog.locator("[data-spot-note]")).toContainText("interruption queue");
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await expect(dialog.getByRole("heading", { name: "Scheduling", exact: true })).toBeFocused();
  await dialog.getByRole("button", { name: "Add node label", exact: true }).click();
  await text(dialog, "Node label 1 key").fill("workload"); await text(dialog, "Node label 1 value").fill("batch");
  await dialog.getByRole("button", { name: "Add node taint", exact: true }).click();
  await text(dialog, "Node taint 1 key").fill("spot"); await text(dialog, "Node taint 1 value").fill("true");
  await text(dialog, "CPU limit (vCPU)").fill("128"); await text(dialog, "Memory limit (GiB)").fill("512");
  expect(api.updates()).toHaveLength(0);
  await dialog.getByRole("button", { name: "Preview plan", exact: true }).click();
  await expect(dialog.getByRole("heading", { name: "Review", exact: true })).toBeVisible();
  await expect(dialog).toContainText("NodePool / spot-workers");
  await expect(dialog).toContainText("Create");
  await expect(dialog).toContainText("workload=batch");
  await expect(dialog).toContainText("spot:true:NoSchedule".replace(":true:", "=true:"));
  await expect(dialog).toContainText("etag-1");
  const preview = api.updates()[0].body;
  expect(preview.updateMask).toBe("spec.nodeGroups");
  expect(preview.validateOnly).toBe(true);
  expect(preview.cluster.etag).toBe("etag-1");
  expect(Object.keys(preview.cluster.spec)).toEqual(["nodeGroups"]);
  expect(preview.cluster.spec.nodeGroups.map((group: Group) => group.name)).toEqual(["batch", "gpu", "fresh", "spot-workers"]);
  expect(preview.cluster.spec.nodeGroups[3]).toMatchObject({
    name: "spot-workers", instanceFamilies: ["c7g"], architectures: ["ARCHITECTURE_ARM64"], capacityTypes: ["CAPACITY_TYPE_SPOT"],
    labels: { workload: "batch" }, taints: [{ key: "spot", value: "true", effect: "TAINT_EFFECT_NO_SCHEDULE" }], limits: { cpu: 128, memoryGib: 512 },
  });
  await dialog.getByRole("button", { name: "Add node group", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  const applied = api.updates()[1].body;
  expect(applied.planId).toBe("node-groups-plan");
  expect(applied.validateOnly).toBeFalsy();
  expect(applied.updateMask).toBe("spec.nodeGroups");
  expect(applied.cluster).toEqual(preview.cluster);
  await expect(page.getByRole("status").filter({ hasText: "spot-workers" })).toBeVisible();
  await page.getByRole("tab", { name: "Node groups", exact: true }).click();
  await expect(row(panel, "spot-workers")).toContainText("Spot");
  await expect(row(panel, "spot-workers")).toContainText("Pending");
  expect(api.unexpected).toEqual([]);
});

test("editing a group keeps its name, sends the whole list and lets the user review the change", async ({ page }) => {
  const api = await mock(page), panel = await open(page);
  await openAction(page, panel, "batch", "Edit");
  const dialog = page.getByRole("dialog", { name: "Edit node group batch", exact: true });
  await expect(text(dialog, "Name")).toHaveAttribute("readonly", "");
  await expect(text(dialog, "Name")).toHaveValue("batch");
  await expect(dialog.getByRole("checkbox", { name: "On-demand", exact: true })).toBeChecked();
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await expect(text(dialog, "Node label 1 key")).toHaveValue("team");
  await expect(text(dialog, "CPU limit (vCPU)")).toHaveValue("64");
  await text(dialog, "CPU limit (vCPU)").fill("96");
  await dialog.getByRole("button", { name: "Preview plan", exact: true }).click();
  await expect(dialog).toContainText("NodePool / batch");
  await expect(dialog).toContainText("Update in place");
  await dialog.getByRole("button", { name: "Apply node group changes", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  const [preview, applied] = api.updates().map(call => call.body);
  expect(preview.updateMask).toBe("spec.nodeGroups");
  expect(preview.cluster.spec.nodeGroups.map((group: Group) => group.name)).toEqual(["batch", "gpu", "fresh"]);
  expect(preview.cluster.spec.nodeGroups[0].limits).toEqual({ cpu: 96 });
  expect(preview.cluster.spec.nodeGroups[1]).toEqual(initialGroups()[1]);
  expect(applied.planId).toBe("node-groups-plan");
  await page.getByRole("tab", { name: "Node groups", exact: true }).click();
  await expect(row(panel, "batch")).toContainText("96 vCPU");
});

test("removing a group shows the deletion in the plan before it is applied", async ({ page }) => {
  const api = await mock(page), panel = await open(page);
  await openAction(page, panel, "gpu", "Remove");
  const dialog = page.getByRole("dialog", { name: "Remove node group gpu", exact: true });
  await expect(dialog).toContainText("drains and deletes every node");
  await dialog.getByRole("button", { name: "Preview plan", exact: true }).click();
  await expect(dialog.getByRole("heading", { name: "Review", exact: true })).toBeVisible();
  await expect(dialog).toContainText("NodePool / gpu");
  await expect(dialog).toContainText("Delete");
  expect(api.updates()).toHaveLength(1);
  await dialog.getByRole("button", { name: "Remove node group", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  const [preview, applied] = api.updates().map(call => call.body);
  expect(preview.cluster.spec.nodeGroups.map((group: Group) => group.name)).toEqual(["batch", "fresh"]);
  expect(preview.updateMask).toBe("spec.nodeGroups");
  expect(preview.cluster.etag).toBe("etag-1");
  expect(applied.planId).toBe("node-groups-plan");
  await page.getByRole("tab", { name: "Node groups", exact: true }).click();
  await expect(row(panel, "gpu")).toContainText("Deleting");
  await expect(panel.getByRole("button", { name: "Actions for node group gpu", exact: true })).toHaveCount(0);
  await expect(row(panel, "batch")).toContainText("Ready");
});

test("client validation mirrors the server and blocks the plan request", async ({ page }) => {
  const api = await mock(page), panel = await open(page);
  await panel.getByRole("button", { name: "Add node group", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Add node group", exact: true });
  const next = dialog.getByRole("button", { name: "Next", exact: true });
  await next.click();
  await expect(alert(dialog)).toContainText("Name must be a lowercase DNS label");
  await text(dialog, "Name").fill("platform");
  await next.click();
  await expect(alert(dialog)).toContainText("reserved");
  await text(dialog, "Name").fill("batch");
  await next.click();
  await expect(alert(dialog)).toContainText("already exists");
  await text(dialog, "Name").fill("ok-name");
  await next.click();
  await expect(alert(dialog)).toContainText("at least one instance type or family");
  await addToken(dialog, "Instance families", "m7i");
  await dialog.getByRole("checkbox", { name: "On-demand", exact: true }).uncheck();
  await next.click();
  await expect(alert(dialog)).toContainText("capacity type");
  await dialog.getByRole("checkbox", { name: "On-demand", exact: true }).check();
  await next.click();
  await expect(dialog.getByRole("heading", { name: "Scheduling", exact: true })).toBeVisible();
  await dialog.getByRole("button", { name: "Add node taint", exact: true }).click();
  await text(dialog, "Node taint 1 key").fill("bad key");
  await text(dialog, "CPU limit (vCPU)").fill("0");
  await dialog.getByRole("button", { name: "Preview plan", exact: true }).click();
  await expect(alert(dialog)).toContainText("Taint 1");
  await expect(text(dialog, "Node taint 1 key")).toHaveAttribute("aria-invalid", "true");
  await dialog.getByRole("button", { name: "Remove node taint 1", exact: true }).click();
  await dialog.getByRole("button", { name: "Preview plan", exact: true }).click();
  await expect(alert(dialog)).toContainText("CPU limit");
  expect(api.updates()).toHaveLength(0);
  expect(api.unexpected).toEqual([]);
});

test("a server rejection of the plan is shown and nothing is applied", async ({ page }) => {
  const api = await mock(page, { failPreview: "Karpenter is not Ready yet" }), panel = await open(page);
  await openAction(page, panel, "fresh", "Remove");
  const dialog = page.getByRole("dialog", { name: "Remove node group fresh", exact: true });
  await dialog.getByRole("button", { name: "Preview plan", exact: true }).click();
  await expect(alert(dialog)).toContainText("Karpenter is not Ready yet");
  await expect(dialog.getByRole("heading", { name: "Remove", exact: true })).toBeVisible();
  expect(api.updates().every(call => call.body.validateOnly === true)).toBe(true);
});

test("the default node pool edit shows platform-managed fields read-only and sends no scaling, label or taint paths", async ({ page }) => {
  const api = await mock(page);
  await page.goto(`/clusters/${clusterId}`);
  await page.getByRole("button", { name: "Edit cluster", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Edit cluster", exact: true });
  await text(dialog, "Display name").fill("Production");
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await expect(text(dialog, "Minimum nodes")).toHaveAttribute("readonly", "");
  await expect(text(dialog, "Maximum nodes")).toHaveAttribute("readonly", "");
  await expect(dialog).toContainText("Managed by the platform");
  await dialog.getByRole("button", { name: "Node labels and taints", exact: true }).click();
  await expect(dialog).toContainText("runs Karpenter and the platform");
  await expect(dialog.getByRole("button", { name: "Add node label", exact: true })).toHaveCount(0);
  await expect(dialog.getByRole("button", { name: "Add node taint", exact: true })).toHaveCount(0);
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await dialog.getByRole("button", { name: "Preview update plan", exact: true }).click();
  await expect(dialog.getByRole("heading", { name: "Review", exact: true })).toBeVisible();
  expect(api.updates()[0].body.updateMask).toBe("displayName");
});

test("a narrow viewport keeps the node groups table and dialog free of horizontal overflow", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 812 });
  await mock(page);
  const panel = await open(page);
  const overflow = () => page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  await expect(row(panel, "batch")).toBeVisible();
  expect(await overflow()).toBeLessThanOrEqual(0);
  await panel.getByRole("button", { name: "Add node group", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Add node group", exact: true });
  await expect(text(dialog, "Name")).toBeVisible();
  expect(await overflow()).toBeLessThanOrEqual(0);
  const box = await dialog.boundingBox();
  expect(box!.x).toBeGreaterThanOrEqual(0);
  expect(box!.x + box!.width).toBeLessThanOrEqual(375);
  await page.screenshot({ path: "test-results/node-groups-narrow-dialog.png" });
});

test("the system row is always shown, Pending until the controller reports it, even with an empty node group status", async ({ page }) => {
  const api = await mock(page, { emptyStatus: true }), panel = await open(page);
  await expect(row(panel, "system")).toContainText("Managed by platform");
  await expect(row(panel, "system")).toContainText("Pending — waiting for the controller");
  await expect(panel.getByRole("button", { name: "Actions for node group system", exact: true })).toHaveCount(0);
  await expect(row(panel, "platform")).toHaveCount(0);
  await expect(row(panel, "batch")).toContainText("Pending");
  expect(api.calls.some(call => call.method === "ListClusterNodeGroupManifests" && (call.body as unknown as { clusterId: string }).clusterId === clusterId)).toBe(true);
  expect(api.unexpected).toEqual([]);
});

test("an imported cluster has no system node group row", async ({ page }) => {
  await mock(page, { emptyStatus: true, imported: true });
  const panel = await open(page);
  await expect(row(panel, "batch")).toBeVisible();
  await expect(row(panel, "system")).toHaveCount(0);
  await expect(panel.getByRole("button", { name: "View YAML for node group system", exact: true })).toHaveCount(0);
});

test("View YAML shows the EC2NodeClass and the NodePool of a user group, with copy", async ({ page }) => {
  const api = await mock(page), panel = await open(page);
  await panel.getByRole("button", { name: "View YAML for node group batch", exact: true }).click();
  let dialog = page.getByRole("dialog", { name: "Node group batch YAML", exact: true });
  await expect(dialog.getByRole("textbox", { name: "EC2NodeClass YAML", exact: true })).toBeVisible();
  await expect(dialog.getByRole("textbox", { name: "NodePool YAML", exact: true })).toBeVisible();
  await expect(dialog).toContainText("kind: EC2NodeClass");
  await expect(dialog).toContainText("kind: NodePool");
  await expect(dialog).toContainText("name: batch");
  await expect(dialog.getByRole("button", { name: "Copy EC2NodeClass YAML", exact: true })).toBeVisible();
  await expect(dialog.getByRole("button", { name: "Copy NodePool YAML", exact: true })).toBeVisible();
  await dialog.getByRole("button", { name: "Close YAML", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  await panel.getByRole("button", { name: "View YAML for node group gpu", exact: true }).click();
  dialog = page.getByRole("dialog", { name: "Node group gpu YAML", exact: true });
  await expect(dialog).toContainText("name: gpu");
  expect(api.unexpected).toEqual([]);
});

test("the platform allow-list suggests families and types and is checked before the plan; the server stays the authority", async ({ page }) => {
  const api = await mock(page, { allow: { instanceFamilies: ["t3", "t3a"], instanceTypes: ["t4g.large"] } }), panel = await open(page);
  await panel.getByRole("button", { name: "Add node group", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Add node group", exact: true });
  await expect(dialog).toContainText("Allowed: t3, t3a");
  await text(dialog, "Name").fill("web");
  // Suggestions come from the allow-list.
  await dialog.getByRole("combobox", { name: "Instance families", exact: true }).click();
  await expect(page.getByRole("option", { name: "t3a", exact: true })).toBeVisible();
  await expect(page.getByRole("option", { name: "t3", exact: true })).toBeVisible();
  await page.keyboard.press("Escape");
  await dialog.getByRole("combobox", { name: "Instance types", exact: true }).click();
  await expect(page.getByRole("option", { name: "t4g.large", exact: true })).toBeVisible();
  await page.keyboard.press("Escape");
  // A family outside it is refused with the allowed list, and nothing is sent.
  await addToken(dialog, "Instance families", "p5");
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await expect(alert(dialog)).toContainText('Instance family "p5" is not allowed');
  await expect(alert(dialog)).toContainText("t3, t3a");
  expect(api.updates()).toHaveLength(0);
  // A type outside both lists is refused too.
  await dialog.getByRole("button", { name: /^Remove p5/ }).click();
  await addToken(dialog, "Instance families", "t3");
  await addToken(dialog, "Instance types", "x2idn.16xlarge");
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await expect(alert(dialog)).toContainText('Instance type "x2idn.16xlarge" is not allowed');
  await dialog.getByRole("button", { name: /^Remove x2idn/ }).click();
  await addToken(dialog, "Instance types", "t4g.large");
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await expect(dialog.getByRole("heading", { name: "Scheduling", exact: true })).toBeVisible();
  expect(api.unexpected).toEqual([]);
});
