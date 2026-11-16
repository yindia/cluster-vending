import { AccessRole, type WhoAmIResponse } from "@/gen/cloud/v1/tenant_service_pb";
import type { Section } from "@/lib/navigation";

export const validTenantId = (id: string) => /^[a-z0-9]([a-z0-9-]{0,54}[a-z0-9])?$/.test(id);

/** Presentation gate follows the API's local anonymous mode and explicit grants. */
export function canCreateTenant(identity: WhoAmIResponse | undefined, capability: boolean, target?: string): boolean {
  return capability && !!identity && (identity.anonymous || identity.grants.some((grant) =>
    grant.role === AccessRole.PLATFORM_ADMIN ||
    (grant.role === AccessRole.TENANT_ADMIN && grant.tenants.some((tenant) => tenant === "*" || (target === undefined || tenant === target)))));
}

export function acceptedTenantId(requested: string, returned: string | undefined): string {
  if (!validTenantId(requested) || returned !== requested) {
    throw new Error("The server did not return the requested tenant. Your current tenant has not changed. Refresh the tenant directory before trying again.");
  }
  return returned;
}

export function tenantSwitchPath(section: Section, hasResourceContext: boolean): string {
  return hasResourceContext || section === "overview" ? "/" : `/${section}`;
}
