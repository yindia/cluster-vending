import { clone, create, fromJsonString, toJson, toJsonString, type DescField, type DescMessage, type MessageShape } from "@bufbuild/protobuf";
import { isReflectList, isReflectMap, isReflectMessage, reflect, type ReflectMessage } from "@bufbuild/protobuf/reflect";
import { ClusterSchema, UpgradeMode, type Cluster } from "../../gen/cloud/v1/cluster_pb";
import { NetworkSchema, NetworkState, type Network } from "../../gen/cloud/v1/network_pb";
import { BootstrapTenantRequestSchema, type CloudAccount } from "../../gen/cloud/v1/tenant_service_pb";
import { UpdateClusterRequestSchema } from "../../gen/cloud/v1/cluster_service_pb";
import { UpdateNetworkRequestSchema } from "../../gen/cloud/v1/network_service_pb";
import { PlannedAction, type Plan } from "../../gen/cloud/v1/plan_pb";
import type { BrowserClients } from "../api/client";
import { administratorsNote, checkExtendedSupport, refuseClusterTier, runningMinor } from "./cluster-manifest";
import { accountFromManifest, resourceJson } from "./manifests";
import { checkedRequest, decodeChecked, ManifestError, manifestPathOf, readManifest, validateAt, type Manifest } from "./manifest-yaml";
import { resourceYaml } from "./resource-yaml";

export type UpdateKind = "Cluster" | "Network" | "CloudAccount";
export type UpdateBaseline =
  | { kind: "Cluster"; resource: Cluster; asRequested: boolean }
  | { kind: "Network"; resource: Network; asRequested: boolean }
  | { kind: "CloudAccount"; resource: CloudAccount };

/** The paths UpdateCluster and UpdateNetwork accept (pkg/updatemask). */
export const clusterUpdatePaths = [
  "display_name", "labels", "spec.access.access_entries", "spec.access.admin_principals", "spec.access.oidc",
  "spec.aws.addons", "spec.aws.control_plane_log_group", "spec.aws.enabled_cluster_log_types", "spec.aws.irsa",
  "spec.aws.upgrade_policy.support_type", "spec.control_plane.endpoint_access.endpoint_private_access",
  "spec.control_plane.endpoint_access.endpoint_public_access", "spec.control_plane.endpoint_access.public_access_cidrs",
  "spec.control_plane.kubernetes_version", "spec.control_plane.secrets_encryption", "spec.maintenance_window",
  "spec.release_channel", "spec.upgrade_mode",
] as const;
export const networkUpdatePaths = ["display_name", "labels", "spec.flow_logs", "spec.tier"] as const;

/** One field an update changes: its mask path and its value now and after, as they read. */
export interface FieldChange { path: string; from: string; to: string }

const fixedWhy: Record<string, string> = {
  name: "it names the object's resources; create another",
  "spec.region": "it is set when the object is created",
  "spec.network": "a cluster stays in the network it was created in",
  "spec.bootstrap_id": "a network stays in its account",
  "spec.layout": "a network's layout is set when it is created",
};
const skipped = new Set(["tenant_id", "id", "status", "generation", "etag", "create_time", "update_time"]);
const MAX_VALUE = 60;

function plain(value: unknown): unknown {
  if (isReflectMessage(value)) return toJson(value.desc, value.message);
  if (isReflectList(value)) return [...value].map(plain);
  if (isReflectMap(value)) return Object.fromEntries([...value].map(([key, entry]) => [String(key), plain(entry)]).sort(([a], [b]) => String(a).localeCompare(String(b))));
  return typeof value === "bigint" ? String(value) : value;
}
function shown(field: DescField, message: ReflectMessage): string {
  if (!message.isSet(field)) return "<none>";
  const value = message.get(field);
  if (field.fieldKind === "enum") return field.enum.values.find((item) => item.number === value)?.name ?? String(value);
  const text = typeof value === "string" ? value : JSON.stringify(plain(value));
  return [...text].length > MAX_VALUE ? `${[...text].slice(0, MAX_VALUE - 1).join("")}…` : text;
}

/**
 * The changes an update of `current` to `desired` makes, as mask paths of `accepted`, the way
 * the CLI works them out (pkg/updatemask.Diff): only what desired states counts, so a field it
 * leaves unset stays as it is. Repeated and map fields compare whole. A changed field no
 * accepted path covers is refused with why.
 */
/** `current` with the fields of `paths` taken from `desired`: the object as the update leaves it. */
export function mergeUpdate<S extends DescMessage>(schema: S, current: MessageShape<S>, desired: MessageShape<S>, paths: readonly string[]): MessageShape<S> {
  const merged = clone(schema, current);
  for (const path of paths) {
    const names = path.split(".");
    let into = reflect(schema, merged);
    let from: ReflectMessage = reflect(schema, desired);
    for (const [index, name] of names.entries()) {
      const field = into.fields.find((item) => item.name === name);
      if (!field) break;
      if (index === names.length - 1) {
        if (from.isSet(field)) into.set(field, from.get(field)); else into.clear(field);
        break;
      }
      from = from.get(field) as ReflectMessage;
      if (!into.isSet(field)) into.set(field, reflect(field.message!, create(field.message!)));
      into = into.get(field) as ReflectMessage;
    }
  }
  return merged;
}

export function diffUpdate<S extends DescMessage>(schema: S, current: MessageShape<S>, desired: MessageShape<S>, accepted: readonly string[]): { changes: FieldChange[]; fixed: string[] } {
  const changes: FieldChange[] = [];
  const fixed: string[] = [];
  const covers = (path: string) => accepted.some((item) => item.startsWith(`${path}.`));
  const walk = (prefix: string, cur: ReflectMessage, des: ReflectMessage) => {
    for (const field of des.fields) {
      if (!des.isSet(field)) continue;
      const path = prefix ? `${prefix}.${field.name}` : field.name;
      if (!prefix && skipped.has(path)) continue;
      const changed = !cur.isSet(field) || JSON.stringify(plain(cur.get(field))) !== JSON.stringify(plain(des.get(field)));
      if (accepted.includes(path)) { if (changed) changes.push({ path, from: shown(field, cur), to: shown(field, des) }); }
      else if (field.fieldKind === "message" && covers(path)) walk(path, cur.get(field) as ReflectMessage, des.get(field) as ReflectMessage);
      else if (changed) fixed.push(path);
    }
  };
  walk("", reflect(schema, current), reflect(schema, desired));
  return { changes: changes.sort((a, b) => a.path.localeCompare(b.path)), fixed: fixed.sort() };
}

function why(path: string): string {
  for (let item = path; item; item = item.includes(".") ? item.slice(0, item.lastIndexOf(".")) : "") {
    if (fixedWhy[item]) return fixedWhy[item];
  }
  return "it is set when the object is created; no update changes it";
}

function refuseFixed(fixed: readonly string[], manifest: Manifest) {
  if (!fixed.length) return;
  throw new ManifestError(fixed.map((path) => `${path} cannot be changed (${why(path)})`).join("; ") + ".", manifest.at.lineOf(manifestPathOf(fixed[0])));
}

/** metadata.id may be left out (it is the edited object) but never names another object. */
function sameObject(manifest: Manifest, id: string, label: string) {
  const stated = manifest.metadata.id;
  if (stated !== undefined && stated !== "" && stated !== id) throw new ManifestError(`metadata.id must stay the edited ${label}'s ID (${id}).`, manifest.at.lineOf(["metadata", "id"]));
}

/** The editable manifest of an object as it is: what the CLI's get prints, ready for update -f. */
export function updateManifestText(baseline: UpdateBaseline): string {
  const what = baseline.kind === "CloudAccount" ? "account" : baseline.kind.toLowerCase();
  const header = baseline.kind === "CloudAccount"
    ? "# The account as it is. Change metadata.name (the account name) or spec.aws.role.\n# The AWS account ID cannot change: connect another account instead.\n"
    : `# The ${what} as it is (cluster-vending ${what} update -f). Fields you leave out stay as they are.\n# Only the fields you change are sent, with the ${what}'s etag; review shows the server's plan.\n`;
  return header + resourceYaml(baseline.kind === "CloudAccount" ? { kind: "CloudAccount", resource: baseline.resource } : baseline.kind === "Network" ? { kind: "Network", resource: baseline.resource } : { kind: "Cluster", resource: baseline.resource });
}

/** A reviewed update: the request it sends, what it changes, and the server's plan when there is one. */
export type UpdateReview = Readonly<{
  kind: UpdateKind; tenantId: string; id: string; name: string;
  requestJson: string; changes: readonly FieldChange[]; notes: readonly string[]; plan?: Plan;
}>;

/** The update a manifest makes to `baseline`, without calling the server. */
export function parseUpdateManifest(text: string, tenantId: string, baseline: UpdateBaseline): Omit<UpdateReview, "plan"> {
  const manifest = readManifest(text, baseline.kind, tenantId);
  const notes: string[] = [];
  if (baseline.kind === "CloudAccount") {
    const current = baseline.resource;
    sameObject(manifest, current.id, "account");
    const account = accountFromManifest(manifest);
    const before = current.provider.case === "aws" ? current.provider.value : undefined;
    const after = account.provider.case === "aws" ? account.provider.value : undefined;
    if (!before || !after || before.accountId !== after.accountId) throw new ManifestError(`spec.aws.accountId cannot change (${before?.accountId ?? "unknown"}): connect another account instead.`, manifest.at.lineOf(["spec", "aws", "accountId"]));
    const changes: FieldChange[] = [];
    if (account.accountName !== current.accountName) changes.push({ path: "account_name", from: current.accountName || "<none>", to: account.accountName || "<none>" });
    if (after.role !== before.role) changes.push({ path: "aws.role", from: before.role || "<none>", to: after.role || "<none>" });
    if (!changes.length) throw new ManifestError("Nothing to change: the manifest states the account as it is.");
    if (changes.some((change) => change.path === "aws.role")) notes.push("A role change affects access for the account's networks and clusters. Update the role's CloudFormation stack too, then verify access.");
    const request = checkedRequest(BootstrapTenantRequestSchema, create(BootstrapTenantRequestSchema, { tenantId, account }));
    return { kind: "CloudAccount", tenantId, id: current.id, name: account.accountName, requestJson: toJsonString(BootstrapTenantRequestSchema, request), changes, notes };
  }
  if (baseline.kind === "Network") {
    const current = baseline.resource;
    sameObject(manifest, current.id, "network");
    if (!current.etag) throw new ManifestError("Reload the network: an etag is required for a reviewed update.");
    if (current.status?.state === NetworkState.IMPORTED || current.status?.state === NetworkState.DELETING) throw new ManifestError("This network cannot be updated in its current state.");
    const desired = decodeChecked(NetworkSchema, resourceJson(manifest), manifest.at, undefined, false);
    const { changes, fixed } = diffUpdate(NetworkSchema, current, desired, networkUpdatePaths);
    refuseFixed(fixed, manifest);
    if (!changes.length) throw new ManifestError("Nothing to change: the manifest states the network as it is.");
    // Fields left out stay as they are: the rules apply to the network as the update leaves it.
    validateAt(NetworkSchema, mergeUpdate(NetworkSchema, current, desired, changes.map((change) => change.path)), manifest.at);
    const flow = changes.find((change) => change.path === "spec.flow_logs");
    if (flow) {
      const line = manifest.at.lineOf(["spec", "flowLogs"]);
      if (current.status?.import) throw new ManifestError("Imported flow logs are carried over; spec.flowLogs cannot be changed.", line);
      const before = current.spec?.flowLogs;
      const after = desired.spec?.flowLogs;
      if (before?.enabled && after?.enabled && (before.trafficType !== after.trafficType || before.aggregationIntervalSeconds !== after.aggregationIntervalSeconds)) {
        throw new ManifestError("trafficType and aggregationIntervalSeconds cannot change while flow logs are enabled: first apply enabled: false, then enable them with the new settings.", line);
      }
    }
    if (!baseline.asRequested) notes.push("The server returned resolved values, not the network as it was requested. Only the fields you change are sent.");
    const network = clone(NetworkSchema, desired);
    network.id = current.id; network.etag = current.etag;
    const request = checkedRequest(UpdateNetworkRequestSchema, create(UpdateNetworkRequestSchema, { tenantId, network, updateMask: { paths: changes.map((change) => change.path) }, validateOnly: true }));
    return { kind: "Network", tenantId, id: current.id, name: current.name, requestJson: toJsonString(UpdateNetworkRequestSchema, request), changes, notes };
  }
  const current = baseline.resource;
  sameObject(manifest, current.id, "cluster");
  if (!current.etag) throw new ManifestError("Reload the cluster: an etag is required for a reviewed update.");
  refuseClusterTier(manifest);
  const desired = decodeChecked(ClusterSchema, resourceJson(manifest), manifest.at, undefined, false);
  // Upgrades automatic after the update: the release channel sets the version, so a manifest's is not a change.
  const mode = desired.spec?.upgradeMode || current.spec?.upgradeMode;
  const want = desired.spec?.controlPlane?.kubernetesVersion ?? "";
  const have = current.spec?.controlPlane?.kubernetesVersion ?? "";
  if (mode === UpgradeMode.AUTOMATIC && want && want !== have && desired.spec?.controlPlane) {
    notes.push(`kubernetesVersion ${want} is ignored: upgrades are automatic, the release channel sets the version${have ? ` (now ${have})` : ""}.`);
    desired.spec.controlPlane.kubernetesVersion = have;
  }
  const { changes, fixed } = diffUpdate(ClusterSchema, current, desired, clusterUpdatePaths);
  refuseFixed(fixed, manifest);
  if (!changes.length) throw new ManifestError("Nothing to change: the manifest states the cluster as it is.");
  const merged = mergeUpdate(ClusterSchema, current, desired, changes.map((change) => change.path));
  validateAt(ClusterSchema, merged, manifest.at);
  const before = current.spec?.controlPlane?.secretsEncryption;
  const after = desired.spec?.controlPlane?.secretsEncryption;
  if (changes.some((change) => change.path === "spec.control_plane.secrets_encryption") && (before?.enabled || before?.kmsKeyArn) && (!after?.enabled || (after.kmsKeyArn || "") !== (before.kmsKeyArn || ""))) {
    throw new ManifestError("Secrets encryption cannot be turned off or moved to another key once enabled.", manifest.at.lineOf(["spec", "controlPlane", "secretsEncryption"]));
  }
  if (changes.some((change) => change.path === "spec.access.admin_principals")) {
    const admins = administratorsNote(merged.spec);
    if (admins) notes.push(admins);
  }
  if (!baseline.asRequested) notes.push("The server returned resolved values, not the cluster as it was requested. Only the fields you change are sent.");
  const cluster = clone(ClusterSchema, desired);
  cluster.id = current.id; cluster.etag = current.etag;
  const request = checkedRequest(UpdateClusterRequestSchema, create(UpdateClusterRequestSchema, { tenantId, cluster, updateMask: { paths: changes.map((change) => change.path) }, validateOnly: true }));
  return { kind: "Cluster", tenantId, id: current.id, name: current.name, requestJson: toJsonString(UpdateClusterRequestSchema, request), changes, notes };
}

/** A plan the server will apply: it has an ID, is applicable, and every change is one it can make. */
export function planApplicable(plan: Plan | undefined): plan is Plan {
  return !!plan && !!plan.planId.trim() && plan.applicable && plan.changes.every(({ action }) => [PlannedAction.CREATE, PlannedAction.UPDATE_IN_PLACE, PlannedAction.DISRUPTIVE, PlannedAction.DELETE].includes(action));
}

/** Parses the manifest, checks the server's version rules, then asks the server for its plan (validate_only). */
export async function reviewUpdate(api: BrowserClients, text: string, tenantId: string, baseline: UpdateBaseline, signal: AbortSignal): Promise<UpdateReview> {
  signal.throwIfAborted();
  const parsed = parseUpdateManifest(text, tenantId, baseline);
  if (parsed.kind === "CloudAccount") return Object.freeze(parsed);
  let plan: Plan | undefined;
  const notes = [...parsed.notes];
  if (parsed.kind === "Cluster") {
    const request = fromJsonString(UpdateClusterRequestSchema, parsed.requestJson);
    const mask = request.updateMask?.paths ?? [];
    if (mask.some((path) => ["spec.control_plane.kubernetes_version", "spec.upgrade_mode", "spec.release_channel", "spec.aws.upgrade_policy.support_type"].includes(path))) {
      const current = baseline.resource as Cluster;
      const merged = mergeUpdate(ClusterSchema, current, request.cluster!, mask);
      const note = await checkExtendedSupport(api, tenantId, merged.spec, readManifest(text, "Cluster", tenantId), signal, runningMinor(current));
      if (note) notes.push(note);
    }
    plan = (await api.cluster.updateCluster(request, { signal })).plan;
  } else {
    plan = (await api.network.updateNetwork(fromJsonString(UpdateNetworkRequestSchema, parsed.requestJson), { signal })).plan;
  }
  signal.throwIfAborted();
  if (!plan) throw new Error("The server returned no plan. Nothing has been applied.");
  return Object.freeze({ ...parsed, notes, plan });
}

/** Sends exactly the reviewed request: the plan ID for clusters and networks, the reviewed account otherwise. */
export async function applyUpdate(api: BrowserClients, review: UpdateReview, signal: AbortSignal): Promise<void> {
  signal.throwIfAborted();
  if (review.kind === "CloudAccount") {
    const response = await api.tenant.bootstrapTenant(fromJsonString(BootstrapTenantRequestSchema, review.requestJson), { signal });
    if (response.account?.id !== review.id) throw new Error("The server answered with another account. Refresh before retrying.");
    return;
  }
  if (!planApplicable(review.plan)) throw new Error("This plan cannot be applied. Change the manifest and review a supported plan.");
  if (review.kind === "Cluster") {
    const request = fromJsonString(UpdateClusterRequestSchema, review.requestJson);
    request.validateOnly = false; request.planId = review.plan.planId;
    const response = await api.cluster.updateCluster(request, { signal });
    if (!response.updateRecorded) throw new Error("The server did not acknowledge the update. Reload and review again.");
    return;
  }
  const request = fromJsonString(UpdateNetworkRequestSchema, review.requestJson);
  request.validateOnly = false; request.planId = review.plan.planId;
  const response = await api.network.updateNetwork(request, { signal });
  if (!response.updateRecorded) throw new Error("The server did not acknowledge recording the update. Refresh before continuing.");
}
