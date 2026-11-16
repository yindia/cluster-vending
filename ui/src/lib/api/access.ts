import { AccessRole, type WhoAmIResponse } from "../../gen/cloud/v1/tenant_service_pb";

/** Presentation gates only: every RPC is still authorized by the Go server. */
export function sessionAccess(identity: WhoAmIResponse | undefined, tenantId: string) {
  const isAdmin = !!identity && (identity.anonymous || identity.grants.some((grant) => grant.role === AccessRole.PLATFORM_ADMIN));
  const canWrite = isAdmin || !!identity?.grants.some((grant) => grant.role === AccessRole.TENANT_ADMIN && (grant.tenants.includes(tenantId) || grant.tenants.includes("*")));
  return { isAdmin, canWrite };
}
