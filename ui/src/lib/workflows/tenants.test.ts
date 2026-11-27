import assert from "node:assert/strict";
import test from "node:test";
import { create } from "@bufbuild/protobuf";
import { WhoAmIResponseSchema } from "../../gen/cloud/v1/tenant_service_pb";
import { Code, ConnectError } from "@connectrpc/connect";
import { acceptedTenantId, canCreateTenant, tenantDeleteConfirmed, tenantDeleteFailure, tenantSwitchPath, validTenantId } from "./tenants";

test("tenant IDs enforce DNS labels of 1–56 characters", () => {
  for (const id of ["a", "tenant-2", "a".repeat(56)]) assert.equal(validTenantId(id), true);
  for (const id of ["", "a".repeat(57), "-a", "a-", "A", "a.b", " a", "a_2"]) assert.equal(validTenantId(id), false);
});

test("creation needs the directory capability and a signed-in session, no role", () => {
  assert.equal(canCreateTenant(undefined, true), false);
  assert.equal(canCreateTenant(create(WhoAmIResponseSchema, { subject: "alice" }), true), true);
  assert.equal(canCreateTenant(create(WhoAmIResponseSchema, { anonymous: true }), true), true);
  assert.equal(canCreateTenant(create(WhoAmIResponseSchema, { subject: "alice" }), false), false);
});

test("only a canonical matching create response can select the new tenant", () => {
  assert.equal(acceptedTenantId("new-tenant", "new-tenant"), "new-tenant");
  for (const returned of [undefined, "", "foreign", "NEW-TENANT"]) assert.throws(() => acceptedTenantId("new-tenant", returned));
});

test("tenant switches retain generic sections and drop resource context", () => {
  assert.equal(tenantSwitchPath("clusters", false), "/clusters");
  assert.equal(tenantSwitchPath("overview", false), "/");
  for (const section of ["accounts", "networks", "clusters", "components", "releases", "freeze"] as const) assert.equal(tenantSwitchPath(section, true), "/");
});

test("deleting a tenant needs its exact ID typed", () => {
  assert.equal(tenantDeleteConfirmed("acme", "acme"), true);
  assert.equal(tenantDeleteConfirmed("acme", "acme "), false);
  assert.equal(tenantDeleteConfirmed("acme", "ACME"), false);
  assert.equal(tenantDeleteConfirmed("", ""), false);
});

test("a refused tenant delete shows what the tenant still has", () => {
  const refused = tenantDeleteFailure(new ConnectError("tenant acme still has 2 clusters and 1 network", Code.FailedPrecondition));
  assert.deepEqual(refused, { status: "warning", title: "The tenant still has resources", description: "tenant acme still has 2 clusters and 1 network. Delete them first, then delete the tenant." });
  assert.equal(tenantDeleteFailure(new ConnectError("no tenant acme", Code.NotFound)).title, "Tenant not found");
  assert.equal(tenantDeleteFailure(new ConnectError("denied", Code.PermissionDenied)).status, "error");
  assert.deepEqual(tenantDeleteFailure(new Error("offline")), { status: "error", title: "Could not delete tenant", description: "offline" });
});
