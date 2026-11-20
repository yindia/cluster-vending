import { expect, test, type Page } from "@playwright/test";
import { stringify } from "yaml";

test.beforeEach(async ({ page }) => {
  await page.route("**/api/auth/session", (route) => route.fulfill({
    contentType: "application/json",
    body: JSON.stringify({ enabled: true, authenticated: true, csrfToken: "browser-test-csrf", username: "test-user" }),
  }));
});

const clusterId = "00000000-0000-4000-8000-000000000003";
const id = "00000000-0000-4000-8000-000000000004";
const spec = { name: "payments", displayName: "Payments", namespaceKeys: ["payments"], quota: { namespaceCount: 1, hard: { pods: "10" } }, policyProfile: "restricted-ambient-v1" };
async function mock(page: Page, options: { existing?: boolean; fail?: boolean; naas?: boolean; viewer?: boolean; foreign?: boolean; deleting?: boolean; protection?: boolean; legacy?: boolean; failProtection?: boolean; duplicate?: boolean; longValues?: boolean } = {}) {
  const calls: { method: string; body: Record<string, unknown> }[] = [], unexpected: string[] = [];
  let current: Record<string, unknown> | undefined = options.existing ? { tenantId: "acme", clusterId, id, spec: { ...spec, ...(options.longValues ? { displayName: "Payments platform with a very long team allocation name for production workloads" } : {}), ...(options.legacy ? { name: "" } : {}), ...(options.protection !== undefined ? { deletionProtection: options.protection } : {}) }, etag: "fresh-etag", generation: "1", status: { state: options.deleting ? "SUB_TENANT_STATE_DELETING" : "SUB_TENANT_STATE_READY", observedGeneration: "1", ...(options.longValues ? { message: "Waiting for Capsule to reconcile namespace policy and resource quota across all workload namespaces. ".repeat(8) } : {}), remoteCapsuleTenant: "remote-team", namespaces: [{ key: "payments", name: "physical-payments", uid: "namespace-uid", templateRevision: "policy-1" }] } } : undefined;
  await page.route("**/api/rpc/**", async (route) => {
    const method = new URL(route.request().url()).pathname.split("/").at(-1)!;
    const body = route.request().postDataJSON() as Record<string, unknown>;
    calls.push({ method, body });
    const error = async (status: number, code: string, message: string) => route.fulfill({ status, contentType: "application/json", body: JSON.stringify({ code, message }) });
    let response: object;
    switch (method) {
      case "WhoAmI": response = options.viewer ? { subject: "viewer", grants: [{ role: "ACCESS_ROLE_TENANT_VIEWER", tenants: ["acme"] }] } : { anonymous: true }; break;
      case "ListTenants": response = { tenants: [{ id: "acme" }] }; break;
      case "GetCluster": response = { cluster: { id: clusterId, tenantId: "acme", name: "production", etag: "cluster-etag", spec: { aws: {}, naas: options.naas !== false, region: "us-west-1", defaultNodePool: { instanceTypes: ["m6i.large"], scalingConfig: { minSize: 1, maxSize: 3 } } }, status: { state: "CLUSTER_STATE_RUNNING" } }, asRequested: true }; break;
      case "ListClusterEvents": response = { events: [] }; break;
      case "ListSubTenants": response = { subtenants: current ? [{ ...current, ...(options.foreign ? { tenantId: "foreign" } : {}) }] : [] }; break;
      case "GetSubTenant": if (!current) { await error(404, "not_found", "Sub Tenant no longer exists"); return; } response = { subtenant: current }; break;
      case "CreateSubTenant":
        if (options.duplicate && (body.subtenant as { spec: { name: string } }).spec.name === "payments") { await error(409, "already_exists", "A Sub Tenant named payments already exists in this cluster"); return; }
        if (options.fail && calls.filter((call) => call.method === method).length === 1) { await error(503, "unavailable", "Retry this request"); return; }
        current = { tenantId: "acme", clusterId, id, spec: (body.subtenant as { spec: object }).spec, etag: "created-etag", generation: "1", status: { state: "SUB_TENANT_STATE_PENDING" } }; response = { subtenant: current }; break;
      case "UpdateSubTenant":
        if (options.failProtection && body.updateMask === "spec.deletionProtection") { await error(409, "aborted", "etag changed; retry with latest allocation"); return; }
        current = { ...current, spec: { ...(current?.spec as object), ...(body.subtenant as { spec: object }).spec }, etag: "updated-etag", generation: "2", status: { state: options.deleting ? "SUB_TENANT_STATE_DELETING" : "SUB_TENANT_STATE_READY", observedGeneration: "1" } }; response = { subtenant: current }; break;
      case "DeleteSubTenant": response = { subtenant: { ...current, status: { state: "SUB_TENANT_STATE_DELETING" } } }; current = undefined; break;
      default: unexpected.push(method); await error(501, "unimplemented", `Unmocked RPC ${method}`); return;
    }
    await route.fulfill({ contentType: "application/json", body: JSON.stringify(response) });
  });
  return { calls, unexpected };
}
async function open(page: Page) { await page.goto(`/clusters/${clusterId}`); await page.getByRole("tab", { name: "Sub Tenants", exact: true }).click(); await expect(page.getByRole("searchbox", { name: "Search Sub Tenants", exact: true })).toBeVisible(); }

test("guided drafts and explicit review, unavailable team access, identical request retry", async ({ page }) => {
  const api = await mock(page, { fail: true }); await open(page);
  await page.getByRole("button", { name: "Create Sub Tenant", exact: true }).click();
  const d = page.getByRole("dialog", { name: "Create Sub Tenant", exact: true });
  await expect(d.getByRole("checkbox", { name: "Deletion protection", exact: true })).toBeChecked();
  await d.getByRole("textbox", { name: /^Name\b/ }).fill("payments");
  await d.getByRole("textbox", { name: /^Display name\b/ }).fill("Payments");
  await d.getByRole("textbox", { name: /^Namespace keys\b/ }).fill("app");
  await d.getByRole("button", { name: "Next", exact: true }).click();
  await expect(d.getByRole("heading", { name: "Quota and access", exact: true })).toBeFocused();
  await expect(d.getByRole("textbox", { name: /^Approved identity references\b/ })).toBeDisabled();
  await expect(d).toContainText("Legacy identity references require a configured resolver");
  await d.getByRole("textbox", { name: /^Resource quotas\b/ }).fill("pods=10");
  await d.getByRole("textbox", { name: "Admin groups", exact: true }).fill("payments-admins,engineering:payments");
  await d.getByRole("textbox", { name: "Viewer groups", exact: true }).fill("payments-viewers");
  await d.getByRole("radio", { name: "Paste YAML", exact: true }).click();
  await d.getByRole("textbox", { name: "Sub Tenant manifest", exact: true }).fill("retained YAML draft");
  await d.getByRole("radio", { name: "Guided form", exact: true }).click();
  await expect(d.getByRole("textbox", { name: /^Display name\b/ })).toHaveValue("Payments");
  await d.getByRole("button", { name: "Next", exact: true }).click();
  await expect(d.getByRole("textbox", { name: /^Resource quotas\b/ })).toHaveValue("pods=10");
  await expect(d.getByRole("textbox", { name: "Admin groups", exact: true })).toHaveValue("payments-admins,engineering:payments");
  await expect(d.getByRole("textbox", { name: "Viewer groups", exact: true })).toHaveValue("payments-viewers");
  await d.getByRole("button", { name: "Review Sub Tenant", exact: true }).click();
  await expect(d).toContainText("payments-app");
  await expect(d).toContainText("Changes are saved first."); await expect(d).not.toContainText("etag"); await expect(d).not.toContainText("Exact reviewed Sub Tenant request");
  expect(api.calls.filter((call) => call.method === "CreateSubTenant")).toHaveLength(0);
  await d.getByRole("button", { name: "Confirm create Sub Tenant", exact: true }).click(); await expect(d).toContainText("Retry this request");
  await d.getByRole("button", { name: "Confirm create Sub Tenant", exact: true }).click(); await expect(d).toHaveCount(0);
  await expect(page.getByRole("table", { name: "Cluster Sub Tenants", exact: true })).toContainText("Payments");
  const requests = api.calls.filter((call) => call.method === "CreateSubTenant"); expect(requests[0].body).toEqual(requests[1].body); expect(requests[0].body.requestId).toMatch(/^[0-9a-f-]{36}$/); expect(requests[0].body.subtenant).toMatchObject({ spec: { access: { adminGroups: ["payments-admins", "engineering:payments"], viewerGroups: ["payments-viewers"] } } }); expect(api.unexpected).toEqual([]);
});

test("view identifiers and YAML, etag update, observed generation, immediate deletion", async ({ page }) => {
  const api = await mock(page, { existing: true }); await open(page);
  const table = page.getByRole("table", { name: "Cluster Sub Tenants", exact: true });
  await table.getByRole("button", { name: `Actions for Sub Tenant Payments (${id})`, exact: true }).click();
  await page.getByRole("menuitem", { name: "View", exact: true }).click();
  const detail = page.getByRole("region", { name: "Sub Tenant details", exact: true });
  await expect(detail).toContainText("remote-team"); await expect(detail.getByRole("table", { name: "Sub Tenant namespaces", exact: true })).toContainText("namespace-uid");
  await detail.getByRole("button", { name: "View Sub Tenant YAML", exact: true }).click(); await expect(detail.getByRole("textbox", { name: "Sub Tenant YAML", exact: true })).toContainText("namespaceKeys:");
  await detail.getByRole("link", { name: "Cluster Sub Tenants", exact: true }).click();
  await table.getByRole("button", { name: `Actions for Sub Tenant Payments (${id})`, exact: true }).click();
  await page.getByRole("menuitem", { name: "Edit", exact: true }).click();
  const d = page.getByRole("dialog", { name: "Edit Sub Tenant", exact: true });
  await expect(d.getByRole("textbox", { name: /^Name\b/ })).toHaveAttribute("readonly", "");
  await expect(d.getByRole("checkbox", { name: "Deletion protection", exact: true })).toBeChecked();
  await d.getByRole("checkbox", { name: "Deletion protection", exact: true }).uncheck();
  await d.getByRole("textbox", { name: /^Namespace keys\b/ }).fill("renamed"); await d.getByRole("button", { name: "Next", exact: true }).click(); await expect(d).toContainText("must be retained");
  await d.getByRole("textbox", { name: /^Namespace keys\b/ }).fill("payments"); await d.getByRole("textbox", { name: /^Display name\b/ }).fill("Renamed payments"); await d.getByRole("button", { name: "Next", exact: true }).click(); await d.getByRole("button", { name: "Review Sub Tenant", exact: true }).click(); await d.getByRole("button", { name: "Apply Sub Tenant update", exact: true }).click(); await expect(d).toHaveCount(0);
  await expect(table).toContainText("Awaiting observation"); const update = api.calls.find((call) => call.method === "UpdateSubTenant")!.body; expect(update.updateMask).toBe("spec.deletionProtection,spec.displayName"); expect(update.subtenant).toMatchObject({ id, etag: "fresh-etag" });
  await table.getByRole("button", { name: `Actions for Sub Tenant Renamed payments (${id})`, exact: true }).click();
  await page.getByRole("menuitem", { name: "Delete", exact: true }).click(); const deletion = page.getByRole("dialog", { name: "Delete Sub Tenant", exact: true });
  await expect(deletion.getByRole("button", { name: "Confirm delete Sub Tenant", exact: true })).toBeDisabled(); await deletion.getByRole("textbox", { name: "Confirm Sub Tenant ID", exact: true }).fill(id); await deletion.getByRole("button", { name: "Confirm delete Sub Tenant", exact: true }).click(); await expect(deletion).toHaveCount(0); await expect(page.getByText("Sub Tenant deleted", { exact: true })).toBeVisible();
  await expect(page.getByText("No Sub Tenants have been created in this cluster.", { exact: true })).toBeVisible(); expect(api.calls.find((call) => call.method === "DeleteSubTenant")!.body).toEqual({ tenantId: "acme", clusterId, subtenantId: id, etag: "updated-etag" }); expect(api.unexpected).toEqual([]);
});

test("YAML tenant guard and edited failed review creates a fresh reviewed request", async ({ page }) => {
  const api = await mock(page, { fail: true }); await open(page); await page.getByRole("button", { name: "Create Sub Tenant", exact: true }).click(); const d = page.getByRole("dialog", { name: "Create Sub Tenant", exact: true }); await d.getByRole("radio", { name: "Paste YAML", exact: true }).click(); const yaml = d.getByRole("textbox", { name: "Sub Tenant manifest", exact: true });
  const manifest = (tenant = "acme", name = "Payments") => stringify({ apiVersion: "cluster-vending.io/v1", kind: "SubTenant", metadata: { tenant, clusterId }, spec: { ...spec, displayName: name } });
  await yaml.fill(manifest("foreign")); await d.getByRole("button", { name: "Review Sub Tenant", exact: true }).click(); await expect(d).toContainText("must match the active tenant"); expect(api.calls.filter((call) => call.method === "CreateSubTenant")).toHaveLength(0);
  await yaml.fill(manifest()); await d.getByRole("button", { name: "Review Sub Tenant", exact: true }).click(); await d.getByRole("button", { name: "Confirm create Sub Tenant", exact: true }).click(); await expect(d).toContainText("Retry this request"); await d.getByRole("button", { name: "Back", exact: true }).click(); await yaml.fill(manifest("acme", "Changed draft")); await expect(d.getByRole("button", { name: "Confirm create Sub Tenant", exact: true })).toHaveCount(0); await d.getByRole("button", { name: "Review Sub Tenant", exact: true }).click(); await d.getByRole("button", { name: "Confirm create Sub Tenant", exact: true }).click(); await expect(d).toHaveCount(0);
  const creates = api.calls.filter((call) => call.method === "CreateSubTenant"); expect(creates[0].body.requestId).not.toBe(creates[1].body.requestId); expect(api.unexpected).toEqual([]);
});

test("existing allocations prevent disabling NaaS", async ({ page }) => {
  const api = await mock(page, { existing: true }); await page.goto(`/clusters/${clusterId}`); await page.getByRole("button", { name: "Edit cluster", exact: true }).click(); const d = page.getByRole("dialog", { name: "Edit cluster", exact: true }); await expect(d.getByRole("checkbox", { name: "Namespace as a Service (NaaS)", exact: true })).toBeDisabled(); await expect(d).toContainText("Delete all Sub Tenants, including those still deleting"); expect(api.unexpected).toEqual([]);
});

test("foreign results fail closed for viewers", async ({ page }) => {
  const api = await mock(page, { existing: true, foreign: true, viewer: true }); await open(page); await expect(page.getByText("Sub Tenants unavailable", { exact: true })).toBeVisible(); await expect(page.getByRole("button", { name: "Create Sub Tenant", exact: true })).toHaveCount(0); await expect(page.getByRole("table", { name: "Cluster Sub Tenants", exact: true })).toHaveCount(0); expect(api.unexpected).toEqual([]);
});

test("disabled NaaS exposes no tab and makes no allocation calls", async ({ page }) => {
  const api = await mock(page, { naas: false }); await page.goto(`/clusters/${clusterId}`); await expect(page.getByRole("heading", { name: "production", exact: true })).toBeVisible(); await expect(page.getByRole("tab", { name: "Sub Tenants", exact: true })).toHaveCount(0); expect(api.calls.filter((call) => /SubTenant/.test(call.method))).toHaveLength(0); expect(api.unexpected).toEqual([]);
});


test("row actions support keyboard opening, navigation, dismissal and selection", async ({ page }) => {
  const api = await mock(page, { existing: true }); await open(page);
  const table = page.getByRole("table", { name: "Cluster Sub Tenants", exact: true });
  await expect(table.getByRole("button", { name: /Actions for Sub Tenant/ })).toHaveCount(1);
  await expect(table.getByRole("button", { name: /^(View|Edit|Delete)$/ })).toHaveCount(0);
  const trigger = table.getByRole("button", { name: `Actions for Sub Tenant Payments (${id})`, exact: true });
  await trigger.focus(); await expect(page.getByRole("menu")).toHaveCount(0);
  await trigger.press("ArrowDown");
  const menu = page.getByRole("menu", { name: "Actions", exact: true });
  await expect(menu.getByRole("menuitem", { name: "View", exact: true })).toBeFocused();
  await page.keyboard.press("ArrowDown"); await expect(menu.getByRole("menuitem", { name: "Edit", exact: true })).toBeFocused();
  await page.keyboard.press("Escape"); await expect(menu).toHaveCount(0); await expect(trigger).toBeFocused();
  await trigger.press("ArrowDown"); await expect(menu.getByRole("menuitem", { name: "View", exact: true })).toBeFocused(); await page.keyboard.press("Enter");
  await expect(page.getByRole("region", { name: "Sub Tenant details", exact: true })).toBeVisible();
  expect(api.unexpected).toEqual([]);
});

test("viewer action menus offer View only and never expose mutations", async ({ page }) => {
  const api = await mock(page, { existing: true, viewer: true }); await open(page);
  await page.getByRole("button", { name: `Actions for Sub Tenant Payments (${id})`, exact: true }).click();
  const menu = page.getByRole("menu", { name: "Actions", exact: true });
  await expect(menu.getByRole("menuitem")).toHaveCount(1);
  await expect(menu.getByRole("menuitem", { name: "View", exact: true })).toBeVisible();
  await expect(menu.getByRole("menuitem", { name: /^(Edit|Delete)$/ })).toHaveCount(0);
  await menu.getByRole("menuitem", { name: "View", exact: true }).click();
  await expect(page.getByRole("region", { name: "Sub Tenant details", exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Create cloud resource" })).toHaveCount(0);
  expect(api.calls.filter((call) => /^(Create|Update|Delete)SubTenant$/.test(call.method))).toHaveLength(0);
  expect(api.unexpected).toEqual([]);
});


test("required immutable names validate locally and duplicate-name server errors preserve drafts", async ({ page }) => {
  const api = await mock(page, { duplicate: true }); await open(page);
  await page.getByRole("button", { name: "Create Sub Tenant", exact: true }).click();
  const d = page.getByRole("dialog", { name: "Create Sub Tenant", exact: true });
  const name = d.getByRole("textbox", { name: /^Name\b/ });
  await name.fill("Uppercase"); await d.getByRole("textbox", { name: /^Namespace keys\b/ }).fill("app");
  await d.getByRole("button", { name: "Next", exact: true }).click(); await expect(d).toContainText("1–35 lowercase DNS characters");
  expect(api.calls.filter((call) => call.method === "CreateSubTenant")).toHaveLength(0);
  await name.fill("payments"); await d.getByRole("button", { name: "Next", exact: true }).click();
  await d.getByRole("textbox", { name: /^Resource quotas\b/ }).fill("pods=10");
  await d.getByRole("button", { name: "Review Sub Tenant", exact: true }).click(); await expect(d).toContainText("payments-app");
  await d.getByRole("button", { name: "Confirm create Sub Tenant", exact: true }).click();
  await expect(d).toContainText("already exists in this cluster");
  await d.getByRole("button", { name: "Back", exact: true }).click(); await d.getByRole("button", { name: "Back", exact: true }).click();
  await expect(name).toHaveValue("payments"); await name.fill("payments-team");
  await d.getByRole("button", { name: "Next", exact: true }).click(); await expect(d.getByRole("textbox", { name: /^Resource quotas\b/ })).toHaveValue("pods=10");
  await d.getByRole("button", { name: "Review Sub Tenant", exact: true }).click(); await d.getByRole("button", { name: "Confirm create Sub Tenant", exact: true }).click(); await expect(d).toHaveCount(0);
  const creates = api.calls.filter((call) => call.method === "CreateSubTenant");
  expect(creates[0].body.requestId).not.toBe(creates[1].body.requestId);
  expect(creates[1].body.subtenant).toMatchObject({ spec: { name: "payments-team", deletionProtection: true } });
  expect(api.unexpected).toEqual([]);
});

test("protected deleting allocations allow only a reviewed protection-only update using a fresh etag", async ({ page }) => {
  const api = await mock(page, { existing: true, deleting: true }); await open(page);
  const table = page.getByRole("table", { name: "Cluster Sub Tenants", exact: true });
  await expect(table).toContainText("Protected"); await expect(table).toContainText("Deleting");
  await table.getByRole("button", { name: `Actions for Sub Tenant Payments (${id})`, exact: true }).click();
  const menu = page.getByRole("menu", { name: "Actions", exact: true });
  await expect(menu.getByRole("menuitem", { name: "Edit", exact: true })).toHaveAttribute("aria-disabled", "true");
  await expect(menu.getByRole("menuitem", { name: /^Delete\b/ })).toHaveAttribute("aria-disabled", "true");
  await expect(menu).toContainText("Disable deletion protection before deleting");
  await menu.getByRole("menuitem", { name: "Disable deletion protection", exact: true }).click();
  const d = page.getByRole("dialog", { name: "Disable deletion protection", exact: true });
  await expect(d).toContainText("Deletion is already pending");
  expect(api.calls.filter((call) => call.method === "UpdateSubTenant")).toHaveLength(0);
  await d.getByRole("button", { name: "Disable deletion protection", exact: true }).click(); await expect(d).toHaveCount(0);
  await expect(table).toContainText("Off"); await expect(table).toContainText("Deleting");
  const updates = api.calls.filter((call) => call.method === "UpdateSubTenant");
  expect(updates).toHaveLength(1);
  expect(updates[0].body).toEqual({ tenantId: "acme", clusterId, subtenant: { id, etag: "fresh-etag", spec: { deletionProtection: false } }, updateMask: "spec.deletionProtection" });
  const index = api.calls.indexOf(updates[0]); expect(api.calls[index - 1]).toMatchObject({ method: "GetSubTenant", body: { tenantId: "acme", clusterId, subtenantId: id } });
  expect(api.calls.filter((call) => call.method === "DeleteSubTenant")).toHaveLength(0);
  expect(api.unexpected).toEqual([]);
});

test("protection update conflicts remain visible without enabling deletion", async ({ page }) => {
  const api = await mock(page, { existing: true, deleting: true, failProtection: true }); await open(page);
  await page.getByRole("button", { name: `Actions for Sub Tenant Payments (${id})`, exact: true }).click(); await page.getByRole("menuitem", { name: "Disable deletion protection", exact: true }).click();
  const d = page.getByRole("dialog", { name: "Disable deletion protection", exact: true });
  await d.getByRole("button", { name: "Disable deletion protection", exact: true }).click(); await expect(d).toContainText("etag changed");
  await expect(d.getByRole("button", { name: "Disable deletion protection", exact: true })).toBeEnabled();
  expect(api.calls.filter((call) => call.method === "DeleteSubTenant")).toHaveLength(0);
  expect(api.unexpected).toEqual([]);
});

test("legacy allocations retain blank names and existing observed namespace names", async ({ page }) => {
  const api = await mock(page, { existing: true, legacy: true }); await open(page);
  await page.getByRole("button", { name: `Actions for Sub Tenant Payments (${id})`, exact: true }).click(); await page.getByRole("menuitem", { name: "Edit", exact: true }).click();
  const d = page.getByRole("dialog", { name: "Edit Sub Tenant", exact: true });
  await expect(d.getByRole("textbox", { name: /^Name\b/ })).toHaveValue(""); await expect(d.getByRole("textbox", { name: /^Name\b/ })).toHaveAttribute("readonly", "");
  await d.getByRole("textbox", { name: /^Display name\b/ }).fill("Legacy display"); await d.getByRole("button", { name: "Next", exact: true }).click(); await d.getByRole("button", { name: "Review Sub Tenant", exact: true }).click();
  await expect(d).not.toContainText("Planned namespaces:"); await d.getByRole("button", { name: "Apply Sub Tenant update", exact: true }).click(); await expect(d).toHaveCount(0);
  expect(api.calls.find((call) => call.method === "UpdateSubTenant")!.body.updateMask).toBe("spec.displayName");
  expect(api.unexpected).toEqual([]);
});


test("YAML group arrays display in detail and guided edits clear groups through the access mask", async ({ page }) => {
  const api = await mock(page); await open(page); await page.getByRole("button", { name: "Create Sub Tenant", exact: true }).click();
  const d = page.getByRole("dialog", { name: "Create Sub Tenant", exact: true }); await d.getByRole("radio", { name: "Paste YAML", exact: true }).click();
  const yaml = d.getByRole("textbox", { name: "Sub Tenant manifest", exact: true });
  const manifest = (viewerGroups: string[]) => stringify({ apiVersion: "cluster-vending.io/v1", kind: "SubTenant", metadata: { tenant: "acme", clusterId }, spec: { ...spec, access: { adminGroups: ["payments-admins"], viewerGroups } } });
  await yaml.fill(manifest(["payments-admins"])); await d.getByRole("button", { name: "Review Sub Tenant", exact: true }).click(); await expect(d).toContainText("cannot be both administrator and viewer");
  expect(api.calls.filter((call) => call.method === "CreateSubTenant")).toHaveLength(0);
  await yaml.fill(manifest(["payments-viewers"])); await d.getByRole("button", { name: "Review Sub Tenant", exact: true }).click();
  await expect(d).toContainText("Admin groups: payments-admins"); await expect(d).toContainText("Viewer groups: payments-viewers");
  await d.getByRole("button", { name: "Confirm create Sub Tenant", exact: true }).click(); await expect(d).toHaveCount(0);
  await page.getByRole("button", { name: `Actions for Sub Tenant Payments (${id})`, exact: true }).click(); await page.getByRole("menuitem", { name: "View", exact: true }).click();
  const detail = page.getByRole("region", { name: "Sub Tenant details", exact: true }); await expect(detail).toContainText("payments-admins"); await expect(detail).toContainText("payments-viewers");
  await detail.getByRole("link", { name: "Cluster Sub Tenants", exact: true }).click();
  await page.getByRole("button", { name: `Actions for Sub Tenant Payments (${id})`, exact: true }).click(); await page.getByRole("menuitem", { name: "Edit", exact: true }).click();
  const edit = page.getByRole("dialog", { name: "Edit Sub Tenant", exact: true }); await edit.getByRole("button", { name: "Next", exact: true }).click();
  await expect(edit.getByRole("textbox", { name: "Admin groups", exact: true })).toHaveValue("payments-admins"); await edit.getByRole("textbox", { name: "Admin groups", exact: true }).fill(""); await edit.getByRole("textbox", { name: "Viewer groups", exact: true }).fill("");
  await edit.getByRole("button", { name: "Review Sub Tenant", exact: true }).click(); await expect(edit).toContainText("Group access will be removed or changed"); await edit.getByRole("button", { name: "Apply Sub Tenant update", exact: true }).click(); await expect(edit).toHaveCount(0);
  const update = api.calls.find((call) => call.method === "UpdateSubTenant")!.body; expect(update.updateMask).toBe("spec.access"); expect(update.subtenant).toMatchObject({ etag: "created-etag", spec: { access: {} } });
  expect(api.unexpected).toEqual([]);
});


test("Sub Tenant table fits its container at desktop, tablet and phone widths", async ({ page }) => {
  test.setTimeout(60000);
  const api = await mock(page, { existing: true, longValues: true });
  await open(page);
  const section = page.getByRole("region", { name: "Cluster Sub Tenants", exact: true });
  const table = section.getByRole("table", { name: "Cluster Sub Tenants", exact: true });
  for (const width of [1440, 1280, 1024, 768, 390]) {
    await page.setViewportSize({ width, height: 1000 });
    await expect(table).toBeVisible();
    await expect.poll(async () => table.evaluate((element) => {
      const box = element.getBoundingClientRect();
      let parent: HTMLElement | null = element as HTMLElement;
      const overflowing: string[] = [];
      while (parent && parent.tagName !== "MAIN") {
        if (parent.scrollWidth > parent.clientWidth + 2) overflowing.push(`${parent.tagName}:${parent.scrollWidth}/${parent.clientWidth}`);
        parent = parent.parentElement;
      }
      return { overflowing, right: Math.round(box.right) };
    })).toEqual({ overflowing: [], right: expect.any(Number) });
    const action = table.getByRole("button", { name: /^Actions for Sub Tenant/ });
    // ResizeObserver can replace the responsive table between measuring it and scrolling.
    await expect(async () => {
      await action.scrollIntoViewIfNeeded();
      await expect(action).toBeInViewport();
    }).toPass({ timeout: 5000 });
    await expect(table.getByRole("columnheader", { name: "Status", exact: true })).toBeVisible();
    const actionBox = await action.boundingBox();
    expect(actionBox!.x + actionBox!.width).toBeLessThanOrEqual(width);
  }
  await table.getByRole("button", { name: /^Actions for Sub Tenant/ }).click();
  await page.getByRole("menuitem", { name: "View", exact: true }).click();
  const detail = page.getByRole("region", { name: "Sub Tenant details", exact: true });
  await expect(detail).toContainText(id);
  await expect(detail).toContainText("remote-team");
  expect(api.unexpected).toEqual([]);
});

test("Sub Tenant detail saves three services locally and keeps its resources out of the cluster list", async ({ page }, info) => {
  const api = await mock(page, { existing: true });
  await open(page);
  await page.getByRole("link", { name: "Payments", exact: true }).click();
  await expect(page).toHaveURL(new RegExp(`/clusters/${clusterId}/subtenants/${id}$`));
  const detail = page.getByRole("region", { name: "Sub Tenant cloud resources" });
  const table = detail.getByRole("table", { name: "Managed resource previews" });
  test.setTimeout(90000);
  for (const [service, name, field, example] of [
    ["S3 bucket", "payments-archive", "kind: Bucket", "bucketNamespace: global"],
    ["RDS database", "payments-db", "engine: mysql", "copyTagsToSnapshot: true"],
    ["ElastiCache cache", "payments-cache", "numCacheClusters: 2", "applyImmediately: false"],
  ]) {
    await detail.getByRole("button", { name: "Create cloud resource" }).click();
    const dialog = page.getByRole("dialog", { name: "Create cloud resource" });
    await dialog.getByRole("radio", { name: service, exact: true }).check();
    await dialog.getByRole("textbox", { name: /^Resource name/ }).fill(name);
    await dialog.getByRole("button", { name: "Configure resource" }).click();
    if (service === "S3 bucket") {
      await dialog.getByRole("checkbox", { name: "Object versioning" }).uncheck();
      await dialog.getByRole("radio", { name: "AWS KMS" }).click();
      await dialog.getByRole("textbox", { name: "KMS key ARN" }).fill("alias/example");
    } else if (service === "RDS database") {
      await dialog.getByRole("radio", { name: "MySQL", exact: true }).click();
      await dialog.getByRole("textbox", { name: "DB instance class", exact: false }).fill("db.m6g.large");
    } else {
      await dialog.getByRole("checkbox", { name: "Multi-AZ with automatic failover" }).check();
      await dialog.getByRole("button", { name: "Network settings" }).click();
      await dialog.getByRole("textbox", { name: "Cache subnet group", exact: true }).fill("private-cache");
    }
    await dialog.getByRole("button", { name: "Additional spec (YAML)" }).click();
    await dialog.getByRole("button", { name: "Use example spec" }).click();
    await expect(dialog.getByRole("textbox", { name: "Additional provider spec" })).toContainText(example);
    await dialog.getByRole("button", { name: "Review preview", exact: true }).click();
    await expect(dialog.getByRole("heading", { name: "Review preview" })).toBeFocused();
    await expect(dialog).toContainText("Sub Tenant · Payments");
    await expect(dialog.getByRole("textbox", { name: "Crossplane preview", exact: true })).toContainText(field);
    await dialog.getByRole("button", { name: "Back", exact: true }).click();
    await expect(dialog.getByRole("heading", { name: service + " settings" })).toBeFocused();
    await dialog.getByRole("button", { name: "Additional spec (YAML)" }).click();
    await expect(dialog.getByRole("textbox", { name: "Additional provider spec" })).toContainText(example);
    await dialog.getByRole("button", { name: "Review preview", exact: true }).click();
    await dialog.getByRole("button", { name: "Create preview", exact: true }).click();
    await expect(table.getByRole("row").filter({ hasText: name })).toContainText(service);
  }
  await page.reload();
  await expect(table.getByRole("row")).toHaveCount(4);
  await page.screenshot({ path: info.outputPath("subtenant-managed-resources.png") });
  await page.setViewportSize({ width: 390, height: 844 });
  await expect.poll(() => table.evaluate(node => node.getBoundingClientRect().right)).toBeLessThanOrEqual(390);
  await page.getByRole("link", { name: "Cluster Sub Tenants", exact: true }).click();
  await page.getByRole("tab", { name: "Cloud Resource", exact: true }).click();
  await expect(page.getByRole("region", { name: "Platform-owned cloud resources" })).toContainText("No managed resource previews yet");
  expect(api.calls.filter(({ method }) => /^(Create|Update|Delete|Apply)/.test(method))).toEqual([]);
  expect(api.unexpected).toEqual([]);
});

test("Sub Tenant name opens details and previews stay within their owner", async ({ page }) => {
  await mock(page, { existing: true }); await open(page);
  await page.getByRole("link", { name: "Payments", exact: true }).click();
  const detail = page.getByRole("region", { name: "Sub Tenant cloud resources" });
  await detail.getByRole("button", { name: "Create cloud resource" }).click();
  const dialog = page.getByRole("dialog", { name: "Create cloud resource" });
  await dialog.getByRole("textbox", { name: /^Resource name/ }).fill("private-preview");
  await dialog.getByRole("button", { name: "Configure resource" }).click();
  await dialog.getByRole("button", { name: "Review preview", exact: true }).click();
  await dialog.getByRole("button", { name: "Create preview", exact: true }).click();
  const otherId = "00000000-0000-4000-8000-000000000005";
  await page.route("**/GetSubTenant", route => route.fulfill({ json: { subtenant: { tenantId: "acme", clusterId, id: otherId, spec: { ...spec, displayName: "Analytics" } } } }));
  await page.goto(`/clusters/${clusterId}/subtenants/${otherId}`);
  await expect(page.getByRole("heading", { name: "Analytics" })).toBeVisible();
  await expect(detail).toContainText("No managed resource previews yet");
  await expect(detail).not.toContainText("private-preview");
});

test("cluster resources are available without NaaS but Sub Tenant creation is gated", async ({ page }) => {
  const api = await mock(page, { naas: false });
  await page.goto(`/clusters/${clusterId}?tab=cloud-resources`);
  await expect(page.getByRole("region", { name: "Platform-owned cloud resources" }).getByRole("button", { name: "Create cloud resource" })).toBeVisible();
  await page.goto(`/clusters/${clusterId}/subtenants/${id}`);
  await expect(page.getByText("Namespace as a Service is disabled on this cluster.")).toBeVisible();
  expect(api.calls.filter(({ method }) => method.endsWith("SubTenant") || method === "ListSubTenants")).toEqual([]);
});

test("Platform-owned previews validate additional fields against the pinned RDS CRD", async ({ page }) => {
  const api = await mock(page);
  await page.goto(`/clusters/${clusterId}?tab=cloud-resources`);
  await page.getByRole("button", { name: "Create cloud resource" }).click();
  const dialog = page.getByRole("dialog", { name: "Create cloud resource" });
  await dialog.getByRole("radio", { name: "RDS database" }).check();
  await dialog.getByRole("textbox", { name: /^Resource name/ }).fill("team-db");
  await dialog.getByRole("button", { name: "Configure resource" }).click();
  await dialog.getByRole("button", { name: "Additional spec (YAML)" }).click();
  await dialog.getByRole("button", { name: /Field reference/ }).click();
  await dialog.getByRole("searchbox", { name: "Find fields in Instance" }).fill("backupRetentionPeriod");
  await expect(dialog).toContainText("1 of 90 provider fields");
  const editor = dialog.getByRole("textbox", { name: "Additional provider spec" });
  await editor.fill("nonexistentField: true");
  await dialog.getByRole("button", { name: "Review preview" }).click();
  await expect(dialog.getByRole("alert").filter({ hasText: "Check the preview" })).toContainText("not defined by this CRD");
  await editor.fill("spec:\n  forProvider:\n    backupRetentionPeriod: 14\n    multiAz: true");
  await dialog.getByRole("button", { name: "Review preview" }).click();
  await expect(dialog).toContainText("Platform · production");
  await expect(dialog.getByRole("textbox", { name: "Crossplane preview" })).toContainText("backupRetentionPeriod: 14");
  await dialog.getByRole("button", { name: "Create preview" }).click();
  await page.reload();
  await page.getByRole("region", { name: "Platform-owned cloud resources" }).getByRole("button", { name: "View YAML" }).click();
  await expect(page.getByRole("textbox", { name: "Saved Crossplane preview" })).toContainText("multiAz: true");
  expect(api.calls.filter(({ method }) => /^(Create|Update|Delete)/.test(method))).toEqual([]);
});

test("the cluster creates each service for the platform, independently of a Sub Tenant with the same name", async ({ page }) => {
  const api = await mock(page, { existing: true });
  await page.goto(`/clusters/${clusterId}?tab=cloud-resources`);
  const platform = page.getByRole("region", { name: "Platform-owned cloud resources" });
  const panel = page.getByRole("tabpanel", { name: "Cloud Resource" });
  await expect(platform).toBeVisible();
  await expect(panel.getByRole("heading", { name: "Reported infrastructure" })).toHaveCount(0);
  await expect(panel.getByRole("button", { name: "Open Cluster snapshot" })).toHaveCount(0);
  await expect(panel.getByRole("table")).toHaveCount(1);
  for (const service of ["S3 bucket", "RDS database", "ElastiCache cache"]) {
    await platform.getByRole("button", { name: "Create cloud resource" }).click();
    const wizard = page.getByRole("dialog", { name: "Create cloud resource" });
    await wizard.getByRole("radio", { name: service, exact: true }).check();
    await wizard.getByRole("textbox", { name: /^Resource name/ }).fill("shared");
    await wizard.getByRole("button", { name: "Configure resource" }).click();
    await wizard.getByRole("button", { name: "Review preview" }).click();
    const yaml = wizard.getByRole("textbox", { name: "Crossplane preview" });
    await expect(yaml).toContainText(clusterId);
    await expect(yaml).not.toContainText("subtenant-id");
    if (service === "S3 bucket") await expect(yaml).toContainText("kind: BucketVersioning");
    await wizard.getByRole("button", { name: "Create preview" }).click();
  }
  await expect(platform.getByRole("row")).toHaveCount(4);
  await page.getByRole("tab", { name: "Sub Tenants", exact: true }).click();
  await page.getByRole("link", { name: "Payments", exact: true }).click();
  const allocation = page.getByRole("region", { name: "Sub Tenant cloud resources" });
  await expect(allocation).toContainText("No managed resource previews yet");
  await allocation.getByRole("button", { name: "Create cloud resource" }).click();
  const wizard = page.getByRole("dialog", { name: "Create cloud resource" });
  await wizard.getByRole("textbox", { name: /^Resource name/ }).fill("shared");
  await wizard.getByRole("button", { name: "Configure resource" }).click();
  await wizard.getByRole("button", { name: "Review preview" }).click();
  await expect(wizard.getByRole("textbox", { name: "Crossplane preview" })).toContainText("cloud.cluster-vending.io/subtenant-id: " + id);
  await wizard.getByRole("button", { name: "Create preview" }).click();
  await expect(allocation.getByRole("row")).toHaveCount(2);
  expect(api.calls.filter(({ method }) => /^(Create|Update|Delete)/.test(method))).toEqual([]);
});
