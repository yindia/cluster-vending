import { expect, test, type Page } from "@playwright/test";
import { stringify } from "yaml";
const clusterId = "00000000-0000-4000-8000-000000000003";
const id = "00000000-0000-4000-8000-000000000004";
const spec = { displayName: "Payments", namespaceKeys: ["payments"], quota: { namespaceCount: 1, hard: { pods: "10" } }, policyProfile: "restricted-ambient-v1" };
async function mock(page: Page, options: { existing?: boolean; fail?: boolean; naas?: boolean; viewer?: boolean; foreign?: boolean } = {}) {
  const calls: { method: string; body: Record<string, unknown> }[] = [], unexpected: string[] = [];
  let current: Record<string, unknown> | undefined = options.existing ? { tenantId: "acme", clusterId, id, spec, etag: "fresh-etag", generation: "1", status: { state: "SUB_TENANT_STATE_READY", observedGeneration: "1", remoteCapsuleTenant: "remote-team", namespaces: [{ key: "payments", name: "physical-payments", uid: "namespace-uid", templateRevision: "policy-1" }] } } : undefined;
  await page.route("**/api/rpc/**", async (route) => {
    const method = new URL(route.request().url()).pathname.split("/").at(-1)!;
    const body = route.request().postDataJSON() as Record<string, unknown>;
    calls.push({ method, body });
    const error = async (status: number, code: string, message: string) => route.fulfill({ status, contentType: "application/json", body: JSON.stringify({ code, message }) });
    let response: object;
    switch (method) {
      case "WhoAmI": response = options.viewer ? { subject: "viewer", grants: [{ role: "ACCESS_ROLE_TENANT_VIEWER", tenants: ["acme"] }] } : { anonymous: true }; break;
      case "ListTenants": response = { tenants: [{ id: "acme" }] }; break;
      case "GetCluster": response = { cluster: { id: clusterId, tenantId: "acme", name: "production", etag: "cluster-etag", spec: { naas: options.naas !== false, region: "us-west-1", defaultNodePool: { instanceTypes: ["m6i.large"], scalingConfig: { minSize: 1, maxSize: 3 } } }, status: { state: "CLUSTER_STATE_RUNNING" } }, asRequested: true }; break;
      case "ListClusterEvents": response = { events: [] }; break;
      case "ListSubTenants": response = { subtenants: current ? [{ ...current, ...(options.foreign ? { tenantId: "foreign" } : {}) }] : [] }; break;
      case "GetSubTenant": if (!current) { await error(404, "not_found", "SubTenant no longer exists"); return; } response = { subtenant: current }; break;
      case "CreateSubTenant":
        if (options.fail && calls.filter((call) => call.method === method).length === 1) { await error(503, "unavailable", "Retry this request"); return; }
        current = { tenantId: "acme", clusterId, id, spec: (body.subtenant as { spec: object }).spec, etag: "created-etag", generation: "1", status: { state: "SUB_TENANT_STATE_PENDING" } }; response = { subtenant: current }; break;
      case "UpdateSubTenant": current = { ...current, spec: (body.subtenant as { spec: object }).spec, etag: "updated-etag", generation: "2", status: { state: "SUB_TENANT_STATE_READY", observedGeneration: "1" } }; response = { subtenant: current }; break;
      case "DeleteSubTenant": response = { subtenant: { ...current, status: { state: "SUB_TENANT_STATE_DELETING" } } }; current = undefined; break;
      default: unexpected.push(method); await error(501, "unimplemented", `Unmocked RPC ${method}`); return;
    }
    await route.fulfill({ contentType: "application/json", body: JSON.stringify(response) });
  });
  return { calls, unexpected };
}
async function open(page: Page) { await page.goto(`/clusters/${clusterId}`); await page.getByRole("tab", { name: "SubTenants", exact: true }).click(); await expect(page.getByRole("searchbox", { name: "Search SubTenants", exact: true })).toBeVisible(); }

test("guided drafts and explicit review, unavailable team access, identical request retry", async ({ page }) => {
  const api = await mock(page, { fail: true }); await open(page);
  await page.getByRole("button", { name: "Create SubTenant", exact: true }).click();
  const d = page.getByRole("dialog", { name: "Create SubTenant", exact: true });
  await d.getByRole("textbox", { name: /^Display name\b/ }).fill("Payments");
  await d.getByRole("textbox", { name: /^Namespace keys\b/ }).fill("payments");
  await d.getByRole("button", { name: "Next", exact: true }).click();
  await expect(d.getByRole("heading", { name: "Quota and access", exact: true })).toBeFocused();
  await expect(d.getByRole("textbox", { name: /^Approved identity references\b/ })).toBeDisabled();
  await expect(d).toContainText("Team access is not configured yet. Leave this empty to create the namespace allocation.");
  await d.getByRole("textbox", { name: /^Resource quotas\b/ }).fill("pods=10");
  await d.getByRole("radio", { name: "Paste YAML", exact: true }).click();
  await d.getByRole("textbox", { name: "SubTenant manifest", exact: true }).fill("retained YAML draft");
  await d.getByRole("radio", { name: "Guided form", exact: true }).click();
  await expect(d.getByRole("textbox", { name: /^Display name\b/ })).toHaveValue("Payments");
  await d.getByRole("button", { name: "Next", exact: true }).click();
  await expect(d.getByRole("textbox", { name: /^Resource quotas\b/ })).toHaveValue("pods=10");
  await d.getByRole("button", { name: "Review SubTenant", exact: true }).click();
  await expect(d).toContainText("Changes are saved first."); await expect(d).not.toContainText("etag"); await expect(d).not.toContainText("Exact reviewed SubTenant request");
  expect(api.calls.filter((call) => call.method === "CreateSubTenant")).toHaveLength(0);
  await d.getByRole("button", { name: "Confirm create SubTenant", exact: true }).click(); await expect(d).toContainText("Retry this request");
  await d.getByRole("button", { name: "Confirm create SubTenant", exact: true }).click(); await expect(d).toHaveCount(0);
  await expect(page.getByRole("table", { name: "Cluster SubTenants", exact: true })).toContainText("Payments");
  const requests = api.calls.filter((call) => call.method === "CreateSubTenant"); expect(requests[0].body).toEqual(requests[1].body); expect(requests[0].body.requestId).toMatch(/^[0-9a-f-]{36}$/); expect(api.unexpected).toEqual([]);
});

test("view identifiers and YAML, etag update, observed generation, immediate deletion", async ({ page }) => {
  const api = await mock(page, { existing: true }); await open(page);
  const table = page.getByRole("table", { name: "Cluster SubTenants", exact: true });
  await table.getByRole("button", { name: "View", exact: true }).click();
  const detail = page.getByRole("dialog", { name: "SubTenant details", exact: true });
  await expect(detail).toContainText("remote-team"); await expect(detail.getByRole("table", { name: "SubTenant namespaces", exact: true })).toContainText("namespace-uid");
  await detail.getByRole("button", { name: "View SubTenant YAML", exact: true }).click(); await expect(detail.getByRole("textbox", { name: "SubTenant YAML", exact: true })).toContainText("namespaceKeys:");
  await detail.getByRole("button", { name: "Close", exact: true }).last().click();
  await table.getByRole("button", { name: "Edit", exact: true }).click();
  const d = page.getByRole("dialog", { name: "Edit SubTenant", exact: true });
  await d.getByRole("textbox", { name: /^Namespace keys\b/ }).fill("renamed"); await d.getByRole("button", { name: "Next", exact: true }).click(); await expect(d).toContainText("must be retained");
  await d.getByRole("textbox", { name: /^Namespace keys\b/ }).fill("payments"); await d.getByRole("textbox", { name: /^Display name\b/ }).fill("Renamed payments"); await d.getByRole("button", { name: "Next", exact: true }).click(); await d.getByRole("button", { name: "Review SubTenant", exact: true }).click(); await d.getByRole("button", { name: "Apply SubTenant update", exact: true }).click(); await expect(d).toHaveCount(0);
  await expect(table).toContainText("Awaiting observation"); const update = api.calls.find((call) => call.method === "UpdateSubTenant")!.body; expect(update.updateMask).toBe("spec.displayName"); expect(update.subtenant).toMatchObject({ id, etag: "fresh-etag" });
  await table.getByRole("button", { name: "Delete", exact: true }).click(); const deletion = page.getByRole("dialog", { name: "Delete SubTenant", exact: true });
  await expect(deletion.getByRole("button", { name: "Confirm delete SubTenant", exact: true })).toBeDisabled(); await deletion.getByRole("textbox", { name: "Confirm SubTenant ID", exact: true }).fill(id); await deletion.getByRole("button", { name: "Confirm delete SubTenant", exact: true }).click(); await expect(deletion).toHaveCount(0); await expect(page.getByText("SubTenant deleted", { exact: true })).toBeVisible();
  await expect(page.getByText("No SubTenants have been created in this cluster.", { exact: true })).toBeVisible(); expect(api.calls.find((call) => call.method === "DeleteSubTenant")!.body).toEqual({ tenantId: "acme", clusterId, subtenantId: id, etag: "updated-etag" }); expect(api.unexpected).toEqual([]);
});

test("YAML tenant guard and edited failed review creates a fresh reviewed request", async ({ page }) => {
  const api = await mock(page, { fail: true }); await open(page); await page.getByRole("button", { name: "Create SubTenant", exact: true }).click(); const d = page.getByRole("dialog", { name: "Create SubTenant", exact: true }); await d.getByRole("radio", { name: "Paste YAML", exact: true }).click(); const yaml = d.getByRole("textbox", { name: "SubTenant manifest", exact: true });
  const manifest = (tenant = "acme", name = "Payments") => stringify({ apiVersion: "cluster-vending.io/v1", kind: "SubTenant", metadata: { tenant, clusterId }, spec: { ...spec, displayName: name } });
  await yaml.fill(manifest("foreign")); await d.getByRole("button", { name: "Review SubTenant", exact: true }).click(); await expect(d).toContainText("must match the active tenant"); expect(api.calls.filter((call) => call.method === "CreateSubTenant")).toHaveLength(0);
  await yaml.fill(manifest()); await d.getByRole("button", { name: "Review SubTenant", exact: true }).click(); await d.getByRole("button", { name: "Confirm create SubTenant", exact: true }).click(); await expect(d).toContainText("Retry this request"); await d.getByRole("button", { name: "Back", exact: true }).click(); await yaml.fill(manifest("acme", "Changed draft")); await expect(d.getByRole("button", { name: "Confirm create SubTenant", exact: true })).toHaveCount(0); await d.getByRole("button", { name: "Review SubTenant", exact: true }).click(); await d.getByRole("button", { name: "Confirm create SubTenant", exact: true }).click(); await expect(d).toHaveCount(0);
  const creates = api.calls.filter((call) => call.method === "CreateSubTenant"); expect(creates[0].body.requestId).not.toBe(creates[1].body.requestId); expect(api.unexpected).toEqual([]);
});

test("existing allocations prevent disabling NaaS", async ({ page }) => {
  const api = await mock(page, { existing: true }); await page.goto(`/clusters/${clusterId}`); await page.getByRole("button", { name: "Edit cluster", exact: true }).click(); const d = page.getByRole("dialog", { name: "Edit cluster", exact: true }); await expect(d.getByRole("checkbox", { name: "Namespace as a Service (NaaS)", exact: true })).toBeDisabled(); await expect(d).toContainText("Delete all SubTenants, including those still deleting"); expect(api.unexpected).toEqual([]);
});

test("foreign results fail closed for viewers", async ({ page }) => {
  const api = await mock(page, { existing: true, foreign: true, viewer: true }); await open(page); await expect(page.getByText("SubTenants unavailable", { exact: true })).toBeVisible(); await expect(page.getByRole("button", { name: "Create SubTenant", exact: true })).toHaveCount(0); await expect(page.getByRole("table", { name: "Cluster SubTenants", exact: true })).toHaveCount(0); expect(api.unexpected).toEqual([]);
});

test("disabled NaaS exposes no tab and makes no allocation calls", async ({ page }) => {
  const api = await mock(page, { naas: false }); await page.goto(`/clusters/${clusterId}`); await expect(page.getByRole("heading", { name: "production", exact: true })).toBeVisible(); await expect(page.getByRole("tab", { name: "SubTenants", exact: true })).toHaveCount(0); expect(api.calls.filter((call) => /SubTenant/.test(call.method))).toHaveLength(0); expect(api.unexpected).toEqual([]);
});
