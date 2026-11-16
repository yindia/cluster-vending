import assert from "node:assert/strict";
import { test } from "node:test";
import { create } from "@bufbuild/protobuf";
import { AccessRole, WhoAmIResponseSchema } from "../../gen/cloud/v1/tenant_service_pb";
import { sessionAccess } from "./access";

test("presentation permissions follow the admitted session and selected tenant", () => {
  assert.deepEqual(sessionAccess(undefined, "acme"), { isAdmin: false, canWrite: false });
  const identity = create(WhoAmIResponseSchema, { grants: [{ role: AccessRole.TENANT_ADMIN, tenants: ["acme"] }] });
  assert.deepEqual(sessionAccess(identity, "acme"), { isAdmin: false, canWrite: true });
  assert.deepEqual(sessionAccess(identity, "other"), { isAdmin: false, canWrite: false });
  identity.grants[0].role = AccessRole.TENANT_VIEWER;
  assert.deepEqual(sessionAccess(identity, "acme"), { isAdmin: false, canWrite: false });
  identity.grants[0].role = AccessRole.TENANT_ADMIN;
  identity.grants[0].tenants = ["*"];
  assert.equal(sessionAccess(identity, "other").canWrite, true);
  identity.grants[0].role = AccessRole.PLATFORM_ADMIN;
  assert.deepEqual(sessionAccess(identity, "other"), { isAdmin: true, canWrite: true });
  // Anonymous is admitted by Go only in the configured local/optional auth modes.
  assert.deepEqual(sessionAccess(create(WhoAmIResponseSchema, { anonymous: true }), "acme"), { isAdmin: true, canWrite: true });
});
