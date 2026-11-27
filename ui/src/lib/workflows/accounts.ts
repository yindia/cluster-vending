import { create } from "@bufbuild/protobuf";
import type { BrowserClients } from "../api/client";
import {
  CloudAccountState,
  GetAccountOnboardingRequestSchema,
  type CloudAccount,
} from "../../gen/cloud/v1/tenant_service_pb";
import { collectPages } from "../api/pagination";

export const ACCOUNT_PAGE_SIZE = 10;
export const ACCOUNT_POLL_MS = 5_000;
export const ACCOUNT_POLL_WINDOW_MS = 120_000;

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
