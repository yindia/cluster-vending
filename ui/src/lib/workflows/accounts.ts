import { create } from "@bufbuild/protobuf";
import type { BrowserClients } from "../api/client";
import {
  BootstrapTenantRequestSchema,
  CloudAccountState,
  GetAccountOnboardingRequestSchema,
  type AwsAccount,
  type CloudAccount,
} from "../../gen/cloud/v1/tenant_service_pb";
import { requireSupportedProvider } from "../providers/registry";
import { collectPages } from "../api/pagination";

/** Local form state; provider payloads always use generated schemas. */
export type AccountBootstrapFields = Pick<CloudAccount, "accountName"> &
  Pick<AwsAccount, "accountId" | "role"> & { provider: string };

export const ACCOUNT_PAGE_SIZE = 10;
export const ACCOUNT_POLL_MS = 5_000;
export const ACCOUNT_POLL_WINDOW_MS = 120_000;

export function accountBootstrapFields(account?: CloudAccount): AccountBootstrapFields {
  return {
    provider: account ? account.provider.case ?? "" : "aws",
    accountName: account?.accountName ?? "",
    accountId: account?.provider.case === "aws" ? account.provider.value.accountId : "",
    role: account?.provider.case === "aws" ? account.provider.value.role : "cluster-vending-provisioner",
  };
}

export function validateAccount(fields: AccountBootstrapFields) {
  const errors: Partial<Record<keyof AccountBootstrapFields, string>> = {};
  if (fields.provider !== "aws") errors.provider = "Only AWS accounts are supported.";
  if (Array.from(fields.accountName.trim()).length > 128) errors.accountName = "Use at most 128 characters.";
  if (!/^\d{12}$/.test(fields.accountId.trim())) errors.accountId = "Enter a 12-digit AWS account ID.";
  if (!/^[\w+=,.@-]{1,64}$/.test(fields.role.trim())) errors.role = "Enter an IAM role name without a path (1–64 letters, digits, or _+=,.@-).";
  return errors;
}

export function accountStepErrors(fields: AccountBootstrapFields, step: number) {
  const errors = validateAccount(fields);
  if (step === 0) return { ...(errors.provider ? { provider: errors.provider } : {}), ...(errors.accountName ? { accountName: errors.accountName } : {}) };
  if (step === 1) return { ...(errors.accountId ? { accountId: errors.accountId } : {}), ...(errors.role ? { role: errors.role } : {}) };
  return errors;
}

/** Join through immutable IDs, after exhausting both tenant-scoped lists. */
export async function loadAccountRelations(api: BrowserClients, tenantId: string, id: string, signal: AbortSignal) {
  requireTenant(tenantId);
  const [allNetworks, allClusters] = await Promise.all([
    collectPages(async (pageToken) => {
      const page = await api.network.listNetworks({ tenantId, pageToken, pageSize: 200 }, { signal });
      return { items: page.networks, nextPageToken: page.nextPageToken };
    }, signal),
    collectPages(async (pageToken) => {
      const page = await api.cluster.listClusters({ tenantId, pageToken, pageSize: 200 }, { signal });
      return { items: page.clusters, nextPageToken: page.nextPageToken };
    }, signal),
  ]);
  const networks = allNetworks.filter((network) => network.spec?.bootstrapId === id);
  const networkIds = new Set(networks.map((network) => network.id));
  const clusters = allClusters.filter((cluster) => !!cluster.spec?.network && networkIds.has(cluster.spec.network.networkId));
  return { networks, clusters };
}

export function bootstrapAccountRequest(tenantId: string, fields: AccountBootstrapFields) {
  requireTenant(tenantId);
  requireSupportedProvider(fields.provider);
  const errors = validateAccount(fields);
  if (Object.keys(errors).length) throw new Error(Object.values(errors).join(" "));
  return create(BootstrapTenantRequestSchema, {
    tenantId,
    account: {
      accountName: fields.accountName.trim(),
      provider: { case: "aws", value: { accountId: fields.accountId.trim(), role: fields.role.trim() } },
    },
  });
}

function requireTenant(tenantId: string) {
  if (!tenantId.trim()) throw new Error("Select a tenant before managing accounts.");
}

export function parseVerifyRegions(value: string): string[] {
  const regions = [...new Set(value.split(",").map((region) => region.trim()).filter(Boolean))];
  if (regions.some((region) => !/^[a-z0-9]+(?:-[a-z0-9]+)+$/.test(region))) {
    throw new Error("Enter comma-separated region identifiers, without spaces inside a region.");
  }
  return regions;
}

export function accountStateLabel(account: CloudAccount): string {
  const state = account.status?.state ?? CloudAccountState.UNSPECIFIED;
  return CloudAccountState[state] ?? `Unknown (${state})`;
}

/** READY includes accounts whose access has not been checked yet. */
export function accountProgressing(account: CloudAccount): boolean {
  const status = account.status;
  return !status || status.state === CloudAccountState.BOOTSTRAPPING ||
    status.state === CloudAccountState.UNSPECIFIED || !status.access ||
    status.access.stale || !status.access.checkedTime;
}

/** Compare server attempt timestamps, never the browser's clock or an old verdict. */
export function accessAttemptKey(account: CloudAccount | undefined): string {
  const time = account?.status?.access?.attemptedTime;
  return time ? `${time.seconds}:${time.nanos}` : "";
}

export function accountPage(accounts: readonly CloudAccount[], requestedPage: number) {
  const totalPages = Math.max(1, Math.ceil(accounts.length / ACCOUNT_PAGE_SIZE));
  const page = Math.min(totalPages, Math.max(1, Math.floor(requestedPage) || 1));
  return { page, accounts: accounts.slice((page - 1) * ACCOUNT_PAGE_SIZE, page * ACCOUNT_PAGE_SIZE) };
}

export async function bootstrapAccount(api: BrowserClients, tenantId: string, fields: AccountBootstrapFields, signal?: AbortSignal) {
  signal?.throwIfAborted();
  const response = await api.tenant.bootstrapTenant(bootstrapAccountRequest(tenantId, fields), { signal });
  signal?.throwIfAborted();
  if (!response.account?.id) throw new Error("The bootstrap response did not include an account ID. Refresh the list before retrying.");
  return response.account;
}

export async function verifyAccount(api: BrowserClients, tenantId: string, id: string, regions: string, signal?: AbortSignal) {
  signal?.throwIfAborted();
  requireTenant(tenantId);
  const response = await api.tenant.verifyCloudAccount({ tenantId, id, regions: parseVerifyRegions(regions) }, { signal });
  signal?.throwIfAborted();
  return response;
}

export async function deleteAccount(api: BrowserClients, tenantId: string, id: string, signal?: AbortSignal) {
  signal?.throwIfAborted();
  requireTenant(tenantId);
  const response = await api.tenant.deleteCloudAccount({ tenantId, id }, { signal });
  signal?.throwIfAborted();
  return response;
}

export function onboardingRequest(tenantId: string, account: CloudAccount) {
  requireTenant(tenantId);
  if (account.provider.case !== "aws") throw new Error("Onboarding instructions are currently available only for AWS.");
  const aws = account.provider.value;
  // Onboarding templates support the roles and naming convention in the IDL.
  const match = /^(cluster-vending-(?:[A-Za-z0-9+=,.@_-]+-)?(reader|updater|provisioner))$/.exec(aws.role);
  if (!match) throw new Error("The template requires a role named cluster-vending-<reader|updater|provisioner> or cluster-vending-<name>-<role>. Configure an existing custom role separately.");
  return create(GetAccountOnboardingRequestSchema, {
    tenantId, id: account.id, accountId: aws.accountId, roleName: aws.role, role: match[2],
  });
}
