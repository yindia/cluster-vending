import assert from "node:assert/strict";
import test from "node:test";
import { create } from "@bufbuild/protobuf";
import { AccessRole, WhoAmIResponseSchema } from "../../gen/cloud/v1/tenant_service_pb";
import { acceptedTenantId, canCreateTenant, tenantSwitchPath, validTenantId } from "./tenants";

test("tenant IDs enforce DNS labels of 1–56 characters", () => {
  for (const id of ["a", "tenant-2", "a".repeat(56)]) assert.equal(validTenantId(id), true);
  for (const id of ["", "a".repeat(57), "-a", "a-", "A", "a.b", " a", "a_2"]) assert.equal(validTenantId(id), false);
});

test("creation requires capability and verified local anonymous mode or a matching write grant", () => {
  assert.equal(canCreateTenant(undefined, true, "acme"), false);
  const anonymous = create(WhoAmIResponseSchema, { anonymous: true });
  assert.equal(canCreateTenant(anonymous, true, "acme"), true);
  assert.equal(canCreateTenant(anonymous, false, "acme"), false);
  assert.equal(canCreateTenant(create(WhoAmIResponseSchema), true, "acme"), false);
  const viewer = create(WhoAmIResponseSchema, { grants: [{ role: AccessRole.TENANT_VIEWER, tenants: ["*"] }] });
  assert.equal(canCreateTenant(viewer, true, "acme"), false);
  const admin = create(WhoAmIResponseSchema, { grants: [{ role: AccessRole.TENANT_ADMIN, tenants: ["acme"] }] });
  assert.equal(canCreateTenant(admin, true), true);
  assert.equal(canCreateTenant(admin, true, "acme"), true);
  assert.equal(canCreateTenant(admin, true, "foreign"), false);
  assert.equal(canCreateTenant(admin, false, "acme"), false);
  for (const grant of [{ role: AccessRole.PLATFORM_ADMIN, tenants: [] }, { role: AccessRole.TENANT_ADMIN, tenants: ["*"] }]) {
    assert.equal(canCreateTenant(create(WhoAmIResponseSchema, { grants: [grant] }), true, "new-tenant"), true);
  }
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
