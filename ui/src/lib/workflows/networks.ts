import { clone, create, equals, fromJson, toJsonString } from "@bufbuild/protobuf";
import {
  FlowLogTrafficType, NatGatewayMode, NetworkFlowLogsSchema, NetworkLayoutSchema,
  NetworkSchema, NetworkState, SubnetRole, type Network, type NetworkFlowLogs,
} from "../../gen/cloud/v1/network_pb";
import {
  CreateNetworkRequestSchema, ImportNetworkRequestSchema, TakeOverNetworkRequestSchema, UpdateNetworkRequestSchema,
  type CreateNetworkRequest, type ImportNetworkRequest, type TakeOverNetworkRequest, type UpdateNetworkRequest,
} from "../../gen/cloud/v1/network_service_pb";
import { Disposition, ImportPhase, ImportStatusSchema } from "../../gen/cloud/v1/import_pb";
import { PlanSchema, PlannedAction, type Plan } from "../../gen/cloud/v1/plan_pb";
import { Tier } from "../../gen/cloud/v1/tier_pb";
import type { CloudAccount } from "../../gen/cloud/v1/tenant_service_pb";
import { supportsCapability } from "../providers/registry";
import type { Cluster } from "../../gen/cloud/v1/cluster_pb";
import type { ListClustersResponse } from "../../gen/cloud/v1/cluster_service_pb";

/** Form state only; RPC values always come from generated schemas. */
export interface NetworkDraft {
  accountId: string;
  name: string;
  displayName: string;
  region: string;
  labelsJson: string;
  tier: Tier;
  cidrBlock: string;
  natMode: NatGatewayMode;
  zoneCount: string;
  subnetsJson: string;
  flowEnabled: boolean;
  trafficType: FlowLogTrafficType;
  retentionDays: string;
  aggregationSeconds: string;
}

export function newNetworkDraft(): NetworkDraft {
  return {
    accountId: "", name: "", displayName: "", region: "", labelsJson: "{}",
    tier: Tier.TIER_0, cidrBlock: "10.0.0.0/16", natMode: NatGatewayMode.SINGLE,
    zoneCount: "3", subnetsJson: "", flowEnabled: false,
    trafficType: FlowLogTrafficType.ALL, retentionDays: "30", aggregationSeconds: "600",
  };
}

export function networkDraft(network: Network): NetworkDraft {
  const spec = network.spec;
  const flow = spec?.flowLogs;
  return {
    ...newNetworkDraft(), accountId: spec?.bootstrapId ?? "", name: network.name,
    displayName: network.displayName, region: spec?.region ?? "",
    labelsJson: JSON.stringify(network.labels, null, 2), tier: spec?.tier ?? Tier.TIER_UNSPECIFIED,
    cidrBlock: spec?.layout?.cidrBlock ?? "", natMode: spec?.layout?.natGatewayMode ?? NatGatewayMode.UNSPECIFIED,
    zoneCount: String(spec?.layout?.availabilityZoneCount ?? 0),
    flowEnabled: flow?.enabled ?? false, trafficType: flow?.trafficType || FlowLogTrafficType.ALL,
    retentionDays: String(flow?.retentionDays || 30), aggregationSeconds: String(flow?.aggregationIntervalSeconds || 600),
  };
}

/** Never infer a provider from an account ID, region, or a missing oneof. */
export function networkProvider(account: CloudAccount | undefined): "aws" | undefined {
  const provider = account?.provider.case;
  return provider === "aws" && supportsCapability(provider, "network.create") ? provider : undefined;
}

export function enumLabel(values: Record<number, string>, value: number | undefined): string {
  if (value === undefined) return "Not reported";
  const name = values[value];
  if (!name) return `Unknown (${value})`;
  return name.toLowerCase().replaceAll("_", " ").replace(/^./, (letter) => letter.toUpperCase());
}

function labels(text: string): Network["labels"] {
  // The generated codec rejects non-string map values and unknown fields.
  const parsed: unknown = JSON.parse(text);
  if (!parsed || Array.isArray(parsed) || typeof parsed !== "object") throw new Error("Labels must be a JSON object of string values.");
  const value = fromJson(NetworkSchema, { labels: parsed as Record<string, string> }).labels;
  if (Object.keys(value).length > 40) throw new Error("At most 40 labels are allowed.");
  for (const [key, entry] of Object.entries(value)) {
    if (!key || key.length > 128 || !/^[A-Za-z0-9 _.:/=+@-]+$/.test(key) || entry.length > 256 || !/^[A-Za-z0-9 _.:/=+@-]*$/.test(entry)) {
      throw new Error("Labels need nonempty keys (max 128) and values (max 256) using letters, digits, spaces or _ . : / = + @ -.");
    }
    if (key.toLowerCase().startsWith("aws:")) throw new Error("Label keys cannot use the reserved aws: prefix.");
  }
  return value;
}

function validTier(tier: Tier) {
  if (![Tier.TIER_UNSPECIFIED, Tier.TIER_0, Tier.TIER_1, Tier.TIER_2].includes(tier)) throw new Error("Choose a supported tier.");
}

const retentionPeriods = [1, 3, 5, 7, 14, 30, 60, 90, 120, 150, 180, 365, 400, 545, 731, 1096, 1827, 2192, 2557, 2922, 3288, 3653];
function flowLogs(draft: NetworkDraft): NetworkFlowLogs {
  const retentionDays = Number(draft.retentionDays);
  const aggregationIntervalSeconds = Number(draft.aggregationSeconds);
  if (!retentionPeriods.includes(retentionDays)) throw new Error("Choose a valid flow log retention period, such as 7, 30, 90 or 365 days.");
  if (![60, 600].includes(aggregationIntervalSeconds)) throw new Error("Flow log aggregation must be 60 or 600 seconds.");
  if (![FlowLogTrafficType.ALL, FlowLogTrafficType.ACCEPT, FlowLogTrafficType.REJECT].includes(draft.trafficType)) throw new Error("Choose a supported traffic type.");
  return create(NetworkFlowLogsSchema, { enabled: draft.flowEnabled, trafficType: draft.trafficType, retentionDays, aggregationIntervalSeconds });
}

function ipv4Range(cidr: string, min: number, max: number): [number, number] {
  const match = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})\/(\d{1,2})$/.exec(cidr);
  if (!match) throw new Error(`Invalid IPv4 CIDR: ${cidr}`);
  const octets = match.slice(1, 5).map(Number);
  const prefix = Number(match[5]);
  if (octets.some((octet) => octet > 255) || prefix < min || prefix > max) throw new Error(`CIDR ${cidr} must use an IPv4 /${min} to /${max} prefix.`);
  const address = octets.reduce((acc, octet) => acc * 256 + octet, 0);
  const size = 2 ** (32 - prefix);
  if (address % size !== 0) throw new Error(`CIDR ${cidr} must start at its network boundary.`);
  return [address, address + size - 1];
}

/** AWS adapter boundary. Server checks the region's actual zone catalog too. */
function awsLayout(draft: NetworkDraft) {
  if (![NatGatewayMode.SINGLE, NatGatewayMode.PER_ZONE].includes(draft.natMode)) throw new Error("AWS supports SINGLE or PER_ZONE NAT in this platform.");
  const range = ipv4Range(draft.cidrBlock, 16, 24);
  const explicit = !!draft.subnetsJson.trim();
  const subnets: unknown = explicit ? JSON.parse(draft.subnetsJson) : [];
  if (!Array.isArray(subnets) || (explicit && subnets.length === 0) || subnets.length > 32) throw new Error("Explicit subnets must be a nonempty JSON array (at most 32 entries).");
  const layout = fromJson(NetworkLayoutSchema, {
    cidrBlock: draft.cidrBlock, natGatewayMode: draft.natMode,
    availabilityZoneCount: explicit ? 0 : Number(draft.zoneCount), subnets,
  });
  if (!explicit) {
    if (!Number.isInteger(layout.availabilityZoneCount) || layout.availabilityZoneCount < 2 || layout.availabilityZoneCount > 6) throw new Error("Choose 2 to 6 availability zones.");
    if (Number(draft.cidrBlock.split("/")[1]) > 22) throw new Error("The default subnet layout requires a /16 to /22 CIDR; use explicit subnets for smaller ranges.");
    return layout;
  }
  const ranges: [number, number][] = [];
  const privateZones = new Set<string>();
  const publicZones = new Set<string>();
  for (const subnet of layout.subnets) {
    if (![SubnetRole.PUBLIC, SubnetRole.PRIVATE].includes(subnet.role)) throw new Error("Every subnet needs a PUBLIC or PRIVATE role.");
    if (!/^[a-z0-9-]+-az[0-9]+$/.test(subnet.zoneId)) throw new Error("Every AWS subnet needs a zone ID, for example use1-az1 (not us-east-1a).");
    const subnetRange = ipv4Range(subnet.cidrBlock, 16, 28);
    if (subnetRange[0] < range[0] || subnetRange[1] > range[1]) throw new Error("Every subnet must be inside the network CIDR.");
    if (ranges.some(([start, end]) => subnetRange[0] <= end && subnetRange[1] >= start)) throw new Error("Subnet CIDRs must not overlap.");
    ranges.push(subnetRange);
    (subnet.role === SubnetRole.PRIVATE ? privateZones : publicZones).add(subnet.zoneId);
  }
  if (privateZones.size < 2 || publicZones.size === 0) throw new Error("AWS requires PRIVATE subnets in at least two zones and at least one PUBLIC subnet.");
  if (draft.natMode === NatGatewayMode.PER_ZONE && [...privateZones].some((zone) => !publicZones.has(zone))) throw new Error("PER_ZONE NAT needs a PUBLIC subnet in every PRIVATE subnet's zone.");
  return layout;
}

export function buildCreateNetwork(tenantId: string, draft: NetworkDraft, account: CloudAccount | undefined, requestId: string): CreateNetworkRequest {
  if (!tenantId || !requestId) throw new Error("Tenant and request ID are required.");
  if (!account || account.id !== draft.accountId || (account.tenantId && account.tenantId !== tenantId) || !networkProvider(account)) throw new Error("Choose an account with a supported provider. Unknown and planned providers cannot create networks.");
  if (!/^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$/.test(draft.name)) throw new Error("Name must start with a lowercase letter, contain only lowercase letters, digits or hyphens, and be at most 40 characters.");
  if (!/^[a-z0-9-]{1,64}$/.test(draft.region)) throw new Error("Enter a provider region identifier.");
  if (draft.displayName.length > 128) throw new Error("Display name must be at most 128 characters.");
  validTier(draft.tier);
  return create(CreateNetworkRequestSchema, {
    tenantId, requestId, network: { name: draft.name, displayName: draft.displayName, labels: labels(draft.labelsJson),
      spec: { bootstrapId: account.id, region: draft.region, tier: draft.tier, layout: awsLayout(draft),
        flowLogs: draft.flowEnabled ? flowLogs(draft) : undefined } },
  });
}

/** Validates just the visible wizard step; future fields cannot block Next. */
export function validateNetworkStep(tenantId: string, draft: NetworkDraft, step: "identity" | "metadata" | "layout" | "flowLogs", account?: CloudAccount, before?: Network): void {
  if (step === "identity") {
    if (!account || account.id !== draft.accountId || (account.tenantId && account.tenantId !== tenantId) || !networkProvider(account)) throw new Error("Choose an account with a supported provider.");
    if (!/^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$/.test(draft.name)) throw new Error("Name must start with a lowercase letter, contain only lowercase letters, digits or hyphens, and be at most 40 characters.");
    if (!/^[a-z0-9-]{1,64}$/.test(draft.region)) throw new Error("Enter a provider region identifier.");
  }
  if (step === "identity" || step === "metadata") {
    if (draft.displayName.length > 128) throw new Error("Display name must be at most 128 characters.");
    labels(draft.labelsJson); validTier(draft.tier);
  }
  if (step === "layout") awsLayout(draft);
  if (step === "flowLogs" && draft.flowEnabled) {
    flowLogs(draft);
    if (before?.spec?.flowLogs?.enabled) {
      const original = networkDraft(before);
      if (original.trafficType !== draft.trafficType || original.aggregationSeconds !== draft.aggregationSeconds) throw new Error("Turn flow logs off and apply first, then enable them with different traffic or aggregation settings.");
    }
  }
}

export interface CreateAttempt { fingerprint: string; requestId: string }
export function createAttempt(tenantId: string, draft: NetworkDraft | NetworkImportDraft, previous: CreateAttempt | undefined, uuid: () => string): CreateAttempt {
  const fingerprint = JSON.stringify([tenantId, draft]);
  return previous?.fingerprint === fingerprint ? previous : { fingerprint, requestId: uuid() };
}

export function buildNetworkUpdate(tenantId: string, before: Network, draft: NetworkDraft): UpdateNetworkRequest {
  if (!tenantId || !before.id || !before.etag || !before.spec) throw new Error("Reload the network to obtain its current spec and etag.");
  if (before.status?.state === NetworkState.IMPORTED || before.status?.state === NetworkState.DELETING) throw new Error("This network cannot be updated in its current state.");
  const original = networkDraft(before);
  const network = clone(NetworkSchema, before);
  const paths: string[] = [];
  if (draft.displayName !== before.displayName) {
    if (draft.displayName.length > 128) throw new Error("Display name must be at most 128 characters.");
    network.displayName = draft.displayName; paths.push("display_name");
  }
  const nextLabels = labels(draft.labelsJson);
  if (!equals(NetworkSchema, create(NetworkSchema, { labels: before.labels }), create(NetworkSchema, { labels: nextLabels }))) {
    network.labels = nextLabels; paths.push("labels");
  }
  validTier(draft.tier);
  if (draft.tier !== before.spec.tier) { network.spec!.tier = draft.tier; paths.push("spec.tier"); }
  const flowKeys = ["flowEnabled", "trafficType", "retentionDays", "aggregationSeconds"] as const;
  if (flowKeys.some((key) => draft[key] !== original[key])) {
    if (before.status?.import) throw new Error("Imported flow logs are carried over; their settings cannot be edited.");
    const flow = draft.flowEnabled ? flowLogs(draft) : create(NetworkFlowLogsSchema, { enabled: false });
    if (original.flowEnabled && draft.flowEnabled && (original.trafficType !== draft.trafficType || original.aggregationSeconds !== draft.aggregationSeconds)) throw new Error("Turn flow logs off and apply first, then enable them with a different traffic type or aggregation interval.");
    network.spec!.flowLogs = flow; paths.push("spec.flow_logs");
  }
  if (!paths.length) throw new Error("No mutable fields changed.");
  return create(UpdateNetworkRequestSchema, { tenantId, network, updateMask: { paths }, validateOnly: true });
}

export interface ReviewedNetworkUpdate { request: UpdateNetworkRequest; plan: Plan }
export function reviewNetworkUpdate(request: UpdateNetworkRequest, plan: Plan | undefined): ReviewedNetworkUpdate {
  if (!plan) throw new Error("The server returned no plan. Nothing has been applied.");
  return { request: clone(UpdateNetworkRequestSchema, request), plan: clone(PlanSchema, plan) };
}
export function canApplyNetworkPlan(plan: Plan): boolean {
  return plan.applicable && !!plan.planId && plan.changes.every(({ action }) => [PlannedAction.CREATE, PlannedAction.UPDATE_IN_PLACE, PlannedAction.DISRUPTIVE, PlannedAction.DELETE].includes(action));
}
export function reviewedNetworkApply(review: ReviewedNetworkUpdate, currentEtag: string): UpdateNetworkRequest {
  if (!canApplyNetworkPlan(review.plan)) throw new Error("This plan cannot be applied. Request and review a supported plan.");
  if (!currentEtag || review.request.network?.etag !== currentEtag) throw new Error("The network changed after review. Reload the editor and review a new plan.");
  const request = clone(UpdateNetworkRequestSchema, review.request);
  request.validateOnly = false;
  request.planId = review.plan.planId;
  return request;
}

export function networkIsReady(network: Network): boolean {
  return network.status?.state === NetworkState.READY && network.status.observedGeneration >= network.generation;
}

export function networkJson(network: Network): string {
  return toJsonString(NetworkSchema, network, { prettySpaces: 2 });
}

export interface NetworkImportDraft {
  accountId: string;
  name: string;
  displayName: string;
  region: string;
  vpcId: string;
  subnetIds: string;
}

export function newNetworkImportDraft(): NetworkImportDraft {
  return { accountId: "", name: "", displayName: "", region: "", vpcId: "", subnetIds: "" };
}

/** Import records an observe-only discovery request, never a takeover. */
export function validateNetworkImportStep(tenantId: string, draft: NetworkImportDraft, step: "identity" | "resources", account?: CloudAccount): void {
  if (step === "resources") { importSubnetIds(draft); return; }
  const provider = networkProvider(account);
  if (!account || account.id !== draft.accountId || (account.tenantId && account.tenantId !== tenantId) || !provider || !supportsCapability(provider, "network.import")) throw new Error("Choose an account with a supported provider for network import.");
  if (!/^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$/.test(draft.name)) throw new Error("Name must start with a lowercase letter, contain only lowercase letters, digits or hyphens, and be at most 40 characters.");
  if (draft.displayName.length > 128) throw new Error("Display name must be at most 128 characters.");
  if (draft.region.length > 64 || !/^[a-z]{2}(-gov|-iso[a-z]?)?-[a-z]+-[0-9]+$/.test(draft.region)) throw new Error("Enter an AWS region, for example us-east-1.");
}

function importSubnetIds(draft: NetworkImportDraft): string[] {
  // Exact AwsImportNetwork IDL constraints; discovery validates account/VPC membership.
  if (!/^vpc-[0-9a-f]{8,17}$/.test(draft.vpcId)) throw new Error("VPC ID must be vpc- followed by 8 to 17 lowercase hexadecimal characters.");
  const subnetIds = draft.subnetIds.split(/[\s,]+/).filter(Boolean);
  if (subnetIds.length < 1 || subnetIds.length > 32 || subnetIds.some((id) => !/^subnet-[0-9a-f]{8,17}$/.test(id))) throw new Error("Enter 1 to 32 subnet IDs, separated by commas or whitespace, each subnet- followed by 8 to 17 lowercase hexadecimal characters.");
  if (new Set(subnetIds).size !== subnetIds.length) throw new Error("Subnet IDs must be unique.");
  return subnetIds;
}

export function buildImportNetwork(tenantId: string, draft: NetworkImportDraft, account: CloudAccount | undefined, requestId: string): ImportNetworkRequest {
  if (!tenantId || !requestId) throw new Error("Tenant and request ID are required.");
  validateNetworkImportStep(tenantId, draft, "identity", account);
  const subnetIds = importSubnetIds(draft);
  return create(ImportNetworkRequestSchema, {
    tenantId, requestId, bootstrapId: account!.id, name: draft.name, displayName: draft.displayName, region: draft.region,
    provider: { case: "aws", value: { vpcId: draft.vpcId, subnetIds } },
  });
}

/** Bind consent to the reviewed ownership plan, not changing observation messages. */
export function networkTakeoverKey(network: Network): string {
  return JSON.stringify([network.tenantId, network.id, network.name, network.etag, String(network.generation),
    network.spec?.bootstrapId, network.status?.state,
    network.status?.import ? toJsonString(ImportStatusSchema, network.status.import) : null]);
}

export function networkTakeoverBlockReason(tenantId: string, network: Network, account: CloudAccount | undefined): string {
  if (!tenantId || !network.id || !network.etag || (network.tenantId && network.tenantId !== tenantId)) return "Reload the network to obtain its current tenant, ID and etag.";
  const provider = networkProvider(account);
  if (!account || account.id !== network.spec?.bootstrapId || (account.tenantId && account.tenantId !== tenantId) || !provider || !supportsCapability(provider, "network.takeover")) return "The network needs an account with a supported takeover provider.";
  if (network.status?.state !== NetworkState.IMPORTED || network.status.import?.phase !== ImportPhase.PLANNED) return "Takeover requires an IMPORTED network with a PLANNED import. Wait for discovery or resolve the reported failures.";
  const dispositions = [Disposition.MATCHES, Disposition.WILL_UPDATE, Disposition.WILL_CREATE, Disposition.NOT_IMPORTED, Disposition.CARRIED_OVER];
  if (network.status.import.resources.some(({ disposition }) => disposition === Disposition.BLOCKING)) return "Blocking resources prevent takeover. Resolve the import plan before continuing.";
  if (network.status.import.resources.some(({ disposition }) => !dispositions.includes(disposition))) return "The import plan contains an unspecified or unsupported resource disposition; takeover is disabled.";
  return "";
}

export interface NetworkTakeoverConfirmation {
  reviewKey: string;
  ownershipConfirmation: string;
  acknowledgeNotImported: boolean;
}

export function buildTakeOverNetwork(tenantId: string, network: Network, account: CloudAccount | undefined, confirmation: NetworkTakeoverConfirmation, canWrite: boolean): TakeOverNetworkRequest {
  if (!canWrite) throw new Error("Write access is required to take over a network.");
  const blocked = networkTakeoverBlockReason(tenantId, network, account);
  if (blocked) throw new Error(blocked);
  if (confirmation.reviewKey !== networkTakeoverKey(network)) throw new Error("The import plan changed. Review it and confirm ownership again.");
  if (confirmation.ownershipConfirmation !== `take over ${network.name}`) throw new Error(`Type take over ${network.name} to confirm the previous tool has stopped managing these resources.`);
  if (network.status!.import!.resources.some(({ disposition }) => disposition === Disposition.NOT_IMPORTED) && !confirmation.acknowledgeNotImported) throw new Error("Acknowledge that every NOT_IMPORTED resource has another owner or was deleted, and the previous tool no longer manages anything here.");
  return create(TakeOverNetworkRequestSchema, { tenantId, networkId: network.id, etag: network.etag, acknowledgeNotImported: confirmation.acknowledgeNotImported });
}

/** Follow all tenant pages; never present a partial scan as complete inventory. */
export async function collectNetworkClusters(networkId: string, loadPage: (pageToken: string) => Promise<ListClustersResponse>, signal: AbortSignal): Promise<Cluster[]> {
  const result = new Map<string, Cluster>();
  const seen = new Set<string>();
  let token = "";
  for (let page = 0; page < 100; page++) {
    signal.throwIfAborted();
    if (seen.has(token)) throw new Error("Cluster listing repeated a page token. Refresh related clusters to retry.");
    seen.add(token);
    const response = await loadPage(token);
    signal.throwIfAborted();
    for (const cluster of response.clusters) if (cluster.spec?.network?.networkId === networkId) result.set(cluster.id, cluster);
    token = response.nextPageToken;
    if (!token) return [...result.values()];
  }
  throw new Error("Cluster listing exceeded 100 pages. Related cluster inventory could not be fully loaded.");
}

/** Shared search for managed, imported and carried-over resource inventories. */
export function filterNetworkInventory<T extends { name: string; kind: string; externalName: string }>(resources: readonly T[], search: string): T[] {
  const terms = search.trim().toLowerCase().split(/\s+/).filter(Boolean);
  return resources.filter((resource) => {
    const text = `${resource.name} ${resource.kind} ${resource.externalName}`.toLowerCase();
    return terms.every((term) => text.includes(term));
  });
}

/** UI rows only: status references may be names despite the cluster_ids field name. */
export type NetworkClusterRow = {
  key: string;
  reference: string;
  cluster?: Cluster;
};

/** A completed list is authoritative; never union legacy status names into UUID rows. */
export function networkClusterRows(
  networkId: string,
  statusReferences: readonly string[],
  clusters: readonly Cluster[] | undefined,
  authoritative = clusters !== undefined,
): NetworkClusterRow[] {
  const matching = new Map<string, Cluster>();
  for (const cluster of clusters ?? []) {
    if (cluster.id && cluster.spec?.network?.networkId === networkId) matching.set(cluster.id, cluster);
  }
  const rows: NetworkClusterRow[] = [...matching.values()].map((cluster) => ({
    key: `cluster:${cluster.id}`, reference: cluster.name || cluster.id, cluster,
  }));
  if (authoritative) return rows;
  // Resolve by ID or resource name only within this network. Distinct UUIDs stay distinct.
  const resolved = new Set([...matching.values()].flatMap((cluster) => [cluster.id, cluster.name]));
  for (const reference of new Set(statusReferences.map((value) => value.trim()).filter(Boolean))) {
    if (!resolved.has(reference)) rows.push({ key: `reference:${reference}`, reference });
  }
  return rows;
}
