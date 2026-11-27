import { Code, ConnectError } from "@connectrpc/connect";
import type { WhoAmIResponse } from "@/gen/cloud/v1/tenant_service_pb";
import type { Section } from "@/lib/navigation";

export const validTenantId = (id: string) => /^[a-z0-9]([a-z0-9-]{0,54}[a-z0-9])?$/.test(id);

/** Any signed-in session may create a tenant when the directory offers it; the API authenticates, it does not authorize. */
export function canCreateTenant(identity: WhoAmIResponse | undefined, capability: boolean): boolean {
  return capability && !!identity;
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

/** Typed confirmation: the tenant ID exactly. */
export const tenantDeleteConfirmed = (tenantId: string, typed: string) => !!tenantId && typed === tenantId;

/** Why a tenant delete failed, for a banner. A refusal names what the tenant still has; it is not a fault. */
export function tenantDeleteFailure(error: unknown): { status: "warning" | "error"; title: string; description: string } {
  const message = error instanceof ConnectError ? error.rawMessage || error.message : error instanceof Error ? error.message : "The request could not be completed.";
  const sentence = message.replace(/\.\s*$/, "");
  if (error instanceof ConnectError && error.code === Code.FailedPrecondition) return { status: "warning", title: "The tenant still has resources", description: `${sentence}. Delete them first, then delete the tenant.` };
  if (error instanceof ConnectError && error.code === Code.NotFound) return { status: "error", title: "Tenant not found", description: `${sentence}. Refresh the tenant directory.` };
  if (error instanceof ConnectError && error.code === Code.PermissionDenied) return { status: "error", title: "Not allowed to delete this tenant", description: message };
  return { status: "error", title: "Could not delete tenant", description: message };
}
