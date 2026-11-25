import { clone, create, fromJson, toJson, toJsonString, type JsonObject, type JsonValue } from "@bufbuild/protobuf";
import { ClusterSchema, ClusterState, MaintenanceWindowSchema, UpgradeMode, type Cluster } from "../../gen/cloud/v1/cluster_pb";
import { AccessSpecSchema, OidcIdentityProviderSchema } from "../../gen/cloud/v1/access_pb";
import { SecretsEncryptionSchema } from "../../gen/cloud/v1/control_plane_pb";
import { CreateClusterRequestSchema, ImportClusterRequestSchema, UpdateClusterRequestSchema, type UpdateClusterRequest } from "../../gen/cloud/v1/cluster_service_pb";
import { PlanSchema, type Plan } from "../../gen/cloud/v1/plan_pb";
import type { Network } from "../../gen/cloud/v1/network_pb";
import type { CloudAccount } from "../../gen/cloud/v1/tenant_service_pb";
import { supportsCapability } from "../providers/registry";
import { awsClusterForm, awsClusterFormValue, awsCreateOnlyClusterFields, awsMutableClusterFields, emptyAwsClusterFields, type AwsClusterFormField } from "../providers/aws-cluster-form";
import { checkedJson, clusterList, nestedFormYaml, optionalBoolean, stringMap, typedFormJson } from "./cluster-form-values";

export interface ClusterForm {
  name: string; displayName: string; networkId: string;
  releaseChannel: string; upgradeMode: string; kubernetesVersion: string;
  publicAccess: boolean; privateAccess: boolean; publicCIDRs: string;
  adminPrincipals: string; irsa: string;
  naas: boolean;
  labels: string;
  maintenanceDays: string; maintenanceStart: string; maintenanceDuration: string;
  zoneIds: string; serviceCidr: string; encryptionEnabled: string; encryptionKeyArn: string;
  awsAdditionalSecurityGroups: string; awsAuthenticationMode: string;
  awsSupportType: string; awsLogGroupEnabled: string; awsLogRetentionDays: string; awsLogTypes: string;
  /** Nested messages as proto JSON text, edited by the typed editors (and accepted as YAML). */
  oidcYaml: string; accessEntriesYaml: string; addonsYaml: string;
}

/** Empty means "server default" everywhere. Node groups are not part of the form: the platform
 * creates the system node group and the Karpenter platform pool from its own configuration. */
export const emptyClusterForm: ClusterForm = {
  name: "", displayName: "", networkId: "", releaseChannel: "0", upgradeMode: "1",
  kubernetesVersion: "", publicAccess: true, privateAccess: true, publicCIDRs: "",
  adminPrincipals: "", irsa: "default", naas: false,
  labels: "{}",
  maintenanceDays: "", maintenanceStart: "", maintenanceDuration: "", zoneIds: "", serviceCidr: "",
  encryptionEnabled: "", encryptionKeyArn: "", oidcYaml: "", accessEntriesYaml: "",
  ...emptyAwsClusterFields,
};

const split = (text: string) => text.split(/[\s,]+/).filter(Boolean);

/**
 * `cluster` is the spec to edit (GetCluster as_requested). Fields set at creation are shown
 * from `resolved`, the spec the server resolved, so the form displays what the cluster really
 * runs instead of empty values.
 */
export function clusterForm(cluster: Cluster, resolved?: Cluster): ClusterForm {
  const spec = cluster.spec;
  const actual = resolved?.spec;
  const endpoint = spec?.controlPlane?.endpointAccess;
  const aws = spec?.provider.case === "aws" ? spec.provider.value : undefined;
  const window = spec?.maintenanceWindow;
  const encryption = spec?.controlPlane?.secretsEncryption;
  const zones = spec?.network?.zoneIds.length ? spec.network.zoneIds : actual?.network?.zoneIds ?? [];
  return {
    name: cluster.name, displayName: cluster.displayName, networkId: spec?.network?.networkId ?? "",
    releaseChannel: String(spec?.releaseChannel ?? 0), upgradeMode: String(spec?.upgradeMode || UpgradeMode.MANUAL),
    kubernetesVersion: spec?.controlPlane?.kubernetesVersion ?? "",
    publicAccess: endpoint?.endpointPublicAccess ?? true, privateAccess: endpoint?.endpointPrivateAccess ?? true,
    publicCIDRs: endpoint?.publicAccessCidrs.join(", ") ?? "",
    adminPrincipals: spec?.access?.adminPrincipals.join(", ") ?? "",
    irsa: aws?.irsa === undefined ? "default" : String(aws.irsa),
    naas: spec?.naas ?? false,
    labels: JSON.stringify(cluster.labels),
    maintenanceDays: window?.days.join(", ") ?? "", maintenanceStart: window?.start ?? "", maintenanceDuration: window?.duration ?? "",
    zoneIds: zones.join(", "), serviceCidr: spec?.network?.serviceCidr || actual?.network?.serviceCidr || "",
    encryptionEnabled: encryption ? String(encryption.enabled || !!encryption.kmsKeyArn) : "", encryptionKeyArn: encryption?.kmsKeyArn ?? "",
    oidcYaml: spec?.access?.oidc ? JSON.stringify(toJson(OidcIdentityProviderSchema, spec.access.oidc), null, 2) : "",
    accessEntriesYaml: spec?.access?.accessEntries.length ? JSON.stringify((toJson(AccessSpecSchema, spec.access) as JsonObject).accessEntries, null, 2) : "",
    ...awsClusterForm(cluster, resolved),
  };
}

// The only provider adapter in this workflow. Never infer AWS from an unknown account.
export function clusterProvider(network: Network, accounts: readonly CloudAccount[], operation: "cluster.create" | "cluster.import" = "cluster.create"): string {
  const account = accounts.find((item) => item.id === network.spec?.bootstrapId);
  const provider = account?.provider.case ?? "unknown";
  if (!supportsCapability(provider, operation)) throw new Error("This network's account has no supported cluster provider.");
  return provider;
}

const mutable = {
  displayName: "display_name",
  releaseChannel: "spec.release_channel",
  upgradeMode: "spec.upgrade_mode",
  kubernetesVersion: "spec.control_plane.kubernetes_version",
  publicAccess: "spec.control_plane.endpoint_access.endpoint_public_access",
  privateAccess: "spec.control_plane.endpoint_access.endpoint_private_access",
  publicCIDRs: "spec.control_plane.endpoint_access.public_access_cidrs",
  adminPrincipals: "spec.access.admin_principals",
  irsa: "spec.aws.irsa",
  naas: "spec.naas",
  labels: "labels",
  maintenanceDays: "spec.maintenance_window", maintenanceStart: "spec.maintenance_window", maintenanceDuration: "spec.maintenance_window",
  encryptionEnabled: "spec.control_plane.secrets_encryption", encryptionKeyArn: "spec.control_plane.secrets_encryption",
  oidcYaml: "spec.access.oidc", accessEntriesYaml: "spec.access.access_entries",
  ...awsMutableClusterFields,
} as const;
export type EditableClusterField = keyof typeof mutable;
const createOnly = { zoneIds: "spec.network.zone_ids", serviceCidr: "spec.network.service_cidr", ...awsCreateOnlyClusterFields } as const;
/** Fields set when the cluster is created; an update cannot change them. */
export const createOnlyClusterFields: readonly string[] = ["zoneIds", "serviceCidr", ...Object.keys(awsCreateOnlyClusterFields)];
export type ClusterInputField = EditableClusterField | keyof typeof createOnly;
const allPaths = { ...mutable, ...createOnly };
const inputFields = Object.keys(allPaths) as ClusterInputField[];

export const clusterWizardSteps = ["Cluster settings", "Access", "Add-ons and logging", "Review"] as const;
export const clusterReviewStep = clusterWizardSteps.length - 1;
const stepFields: readonly (readonly ClusterInputField[])[] = [
  ["displayName", "labels", "releaseChannel", "upgradeMode", "kubernetesVersion", "awsSupportType", "naas", "maintenanceDays", "maintenanceStart", "maintenanceDuration"],
  ["publicAccess", "privateAccess", "publicCIDRs", "adminPrincipals", "irsa", "zoneIds", "serviceCidr", "encryptionEnabled", "encryptionKeyArn", "oidcYaml", "accessEntriesYaml", "awsAdditionalSecurityGroups", "awsAuthenticationMode"],
  ["addonsYaml", "awsLogGroupEnabled", "awsLogRetentionDays", "awsLogTypes"],
];

const automatic = (form: ClusterForm) => Number(form.upgradeMode) === UpgradeMode.AUTOMATIC;

/**
 * Called only on Next/submit. Creating validates the whole step; editing (with the
 * form's `baseline`) validates only what differs from it, so an untouched edit always
 * reaches the plan preview even when the server resolved values the form cannot set.
 */
export function validateClusterFormStep(form: ClusterForm, step: number, baseline?: ClusterForm) {
  if (!stepFields[step]) throw new Error("Unknown cluster form step.");
  if (step === 0 && !baseline) {
    if (!/^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$/.test(form.name)) throw new Error("Name must be a lowercase DNS label starting with a letter, at most 40 characters.");
    if (!form.networkId) throw new Error("Choose an existing network.");
  }
  const changed = baseline ? changedClusterFields(baseline, form) : undefined;
  const fields = stepFields[step].filter((field) => (!changed || changed.has(field)) && !(field === "kubernetesVersion" && automatic(form)));
  validateRelated(form, new Set(fields));
  for (const field of fields) valueOf(field, form);
}

/** Validates every input step; the first failure names the step to return to. */
export function validateClusterForm(form: ClusterForm, baseline?: ClusterForm): void {
  for (let step = 0; step < clusterReviewStep; step++) {
    try { validateClusterFormStep(form, step, baseline); }
    catch (cause) { throw new ClusterStepError(step, cause instanceof Error ? cause.message : String(cause)); }
  }
}
export class ClusterStepError extends Error {
  constructor(readonly step: number, message: string) { super(`${clusterWizardSteps[step]}: ${message}`); }
}

export interface ClusterImportForm { name: string; displayName: string; networkId: string; defaultNodeGroupName: string }
export function buildImportCluster(tenantId: string, form: ClusterImportForm, network: Network, accounts: readonly CloudAccount[], requestId: string) {
  clusterProvider(network, accounts, "cluster.import");
  if (network.id !== form.networkId || !network.spec?.region) throw new Error("Choose an existing network with a region.");
  if (!/^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$/.test(form.name)) throw new Error("Cluster name must be a lowercase DNS label starting with a letter, at most 40 characters.");
  if (form.displayName.length > 128) throw new Error("Display name must be at most 128 characters.");
  if (!/^[0-9A-Za-z][A-Za-z0-9_-]{0,62}$/.test(form.defaultNodeGroupName)) throw new Error("Enter the existing node group name, at most 63 letters, digits, underscores or hyphens.");
  return create(ImportClusterRequestSchema, { tenantId, name: form.name, displayName: form.displayName, networkId: network.id, requestId, provider: { case: "aws", value: { defaultNodeGroupName: form.defaultNodeGroupName } } });
}

function integer(value: string, min: number, max: number, label: string): number {
  if (!/^\d+$/.test(value) || Number(value) < min || Number(value) > max) throw new Error(`${label} must be a whole number from ${min} to ${max}.`);
  return Number(value);
}

function valueOf(field: ClusterInputField, form: ClusterForm): JsonValue | undefined {
  switch (field) {
    case "releaseChannel": return integer(form[field], 0, 3, "Release channel");
    case "upgradeMode": return integer(form[field], 1, 2, "Upgrade mode");
    case "publicCIDRs": {
      const cidrs = split(form[field]);
      if (cidrs.length > 40 || new Set(cidrs).size !== cidrs.length || cidrs.some((cidr) => {
        const match = /^(\d{1,3}(?:\.\d{1,3}){3})\/(\d{1,2})$/.exec(cidr);
        return !match || Number(match[2]) > 32 || match[1].split(".").some((n) => Number(n) > 255);
      })) throw new Error("Enter up to 40 unique IPv4 CIDRs, such as 203.0.113.0/24.");
      return cidrs;
    }
    case "adminPrincipals": {
      const principals = split(form.adminPrincipals);
      if (principals.length > 20 || new Set(principals).size !== principals.length || principals.some(value => !/^arn:aws[a-z-]*:iam::[0-9]{12}:(role|user)\/[A-Za-z0-9+=,.@_/-]+$/.test(value) || value.includes(":role/aws-service-role/"))) throw new Error("Enter up to 20 unique IAM role or user ARNs; service-linked roles are not accepted.");
      return principals;
    }
    case "kubernetesVersion": {
      if (automatic(form)) throw new Error("Automatic upgrades cannot pin a Kubernetes version.");
      if (form[field] && !/^1\.\d{2}$/.test(form[field])) throw new Error("Kubernetes version must be a minor version such as 1.33, or empty for the server default.");
      return form[field];
    }
    case "displayName": {
      if (form[field].length > 128) throw new Error("Display name must be at most 128 characters.");
      return form[field];
    }
    case "irsa": return optionalBoolean(form.irsa, "IRSA");
    case "labels": {
      const labels = stringMap(form.labels, 40, "Cloud tags");
      for (const [key, value] of Object.entries(labels)) if (!/^[A-Za-z0-9 _.:/=+@-]{1,128}$/.test(key) || key.toLowerCase().startsWith("aws:") || !/^[A-Za-z0-9 _.:/=+@-]{0,256}$/.test(value)) throw new Error("Cloud tags must follow AWS tag rules and cannot use the aws: prefix.");
      return labels;
    }
    case "zoneIds": {
      const zones = clusterList(form.zoneIds);
      if (zones.length && (zones.length < 2 || new Set(zones).size !== zones.length || zones.some(zone => !/^[a-z]+[0-9]-az[0-9]+$/.test(zone)))) throw new Error("Choose at least two unique AWS zone IDs.");
      return zones;
    }
    case "serviceCidr": {
      const cidr = form.serviceCidr;
      const match = /^(\d{1,3}(?:\.\d{1,3}){3})\/(1[2-9]|2[0-4])$/.exec(cidr);
      if (cidr && (!match || match[1].split(".").some(n => Number(n) > 255))) throw new Error("Service CIDR must be an IPv4 /12 to /24 range.");
      return cidr;
    }
    case "maintenanceDays": case "maintenanceStart": case "maintenanceDuration": {
      if (!form.maintenanceDays.trim() && !form.maintenanceStart && !form.maintenanceDuration) return undefined;
      const weekdays = ["monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"];
      const days = clusterList(form.maintenanceDays).map(day => { const index = weekdays.indexOf(day.toLowerCase()); return index >= 0 ? index + 1 : integer(day, 1, 7, "UTC weekday"); });
      return typedFormJson(MaintenanceWindowSchema, { days, start: form.maintenanceStart, duration: form.maintenanceDuration });
    }
    case "encryptionEnabled": case "encryptionKeyArn": {
      if (!form.encryptionEnabled && !form.encryptionKeyArn) return undefined;
      const enabled = optionalBoolean(form.encryptionEnabled, "Encryption enabled");
      if (enabled === false && form.encryptionKeyArn) throw new Error("An encryption key implies enabled encryption.");
      return typedFormJson(SecretsEncryptionSchema, { enabled: enabled ?? !!form.encryptionKeyArn, kmsKeyArn: form.encryptionKeyArn });
    }
    case "oidcYaml": { const value = nestedFormYaml(form.oidcYaml); return value === null ? undefined : typedFormJson(OidcIdentityProviderSchema, value); }
    case "accessEntriesYaml": {
      const json = typedFormJson(AccessSpecSchema, { accessEntries: nestedFormYaml(form.accessEntriesYaml) ?? [] }) as JsonObject;
      return json.accessEntries ?? [];
    }
    case "awsAdditionalSecurityGroups": case "awsAuthenticationMode": case "awsSupportType": case "awsLogGroupEnabled": case "awsLogRetentionDays": case "awsLogTypes": case "addonsYaml":
      return awsClusterFormValue(field as AwsClusterFormField, form);
    default: return form[field];
  }
}

function validateRelated(form: ClusterForm, fields: ReadonlySet<string>) {
  if (["publicAccess", "privateAccess", "publicCIDRs"].some((field) => fields.has(field))) {
    if (!form.publicAccess && !form.privateAccess) throw new Error("Enable at least one API endpoint.");
    if (form.publicAccess && !split(form.publicCIDRs).length) throw new Error("Public API access requires at least one allowed CIDR.");
  }
  if (["adminPrincipals", "accessEntriesYaml", "awsAuthenticationMode"].some(field => fields.has(field))) {
    const admins = split(form.adminPrincipals);
    const entries = (valueOf("accessEntriesYaml", form) ?? []) as JsonObject[];
    if (entries.some(entry => admins.includes(String(entry.principalArn)))) throw new Error("A principal cannot be both an administrator and an access entry.");
    if (form.awsAuthenticationMode === "3" && (admins.length || entries.length)) throw new Error("IAM access entries require API or API_AND_CONFIG_MAP authentication mode.");
  }
}

// JSON uses lowerCamelCase; protobuf FieldMask paths use snake_case.
function setPath(target: JsonObject, path: string, value: JsonValue | undefined) {
  const parts = path.split(".").map((part) => part.replace(/_([a-z])/g, (_, char: string) => char.toUpperCase()));
  let current = target;
  for (const part of parts.slice(0, -1)) {
    current[part] ??= {};
    current = current[part] as JsonObject;
  }
  if (value !== undefined) current[parts.at(-1)!] = value;
}

const sentWhenEmpty = ["displayName", "kubernetesVersion", "adminPrincipals", "publicCIDRs"];

export interface ClusterPlacement { region: string; networkId: string }

/**
 * The cluster as proto JSON (name, display name, labels and spec) for these inputs.
 * Empty inputs are left out so the server applies its own defaults. Node groups are never
 * sent: the platform creates the system node group and the Karpenter platform pool.
 */
export function clusterDocument(form: ClusterForm, placement: ClusterPlacement): JsonObject {
  const input: JsonObject = { name: form.name, spec: { region: placement.region, network: { networkId: placement.networkId }, aws: {} } };
  for (const field of inputFields) {
    if (field === "kubernetesVersion" && automatic(form)) continue;
    if (typeof form[field] === "string" && form[field] === "" && !sentWhenEmpty.includes(field)) continue;
    setPath(input, allPaths[field], valueOf(field, form));
  }
  return input;
}

export function buildCreateCluster(tenantId: string, form: ClusterForm, network: Network, accounts: readonly CloudAccount[], requestId: string) {
  clusterProvider(network, accounts);
  if (network.id !== form.networkId || !network.spec?.region) throw new Error("Choose an existing network with a region.");
  if (!/^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$/.test(form.name)) throw new Error("Name must be a lowercase DNS label starting with a letter, at most 40 characters.");
  validateRelated(form, new Set(inputFields));
  const input = clusterDocument(form, { region: network.spec.region, networkId: network.id });
  return create(CreateClusterRequestSchema, { tenantId, requestId, cluster: checkedJson(ClusterSchema, input) });
}

const canonical = (value: JsonValue | undefined) => value === undefined ? "<unset>" : JSON.stringify(value);
function sameValue(field: ClusterInputField, before: ClusterForm, after: ClusterForm): boolean {
  if (JSON.stringify(before[field]) === JSON.stringify(after[field])) return true;
  // Both sides read the same way, including the same refusal (a version under automatic upgrades): not a change.
  const read = (form: ClusterForm) => { try { return canonical(valueOf(field, form)); } catch (cause) { return `<error>${cause instanceof Error ? cause.message : String(cause)}`; } };
  return read(before) === read(after);
}

/** The inputs whose value differs from `baseline`: compared as the values they send, so
 * reformatting (spacing, order of equal JSON) is not a change. */
export function changedClusterFields(baseline: ClusterForm, form: ClusterForm): Set<ClusterInputField> {
  return new Set(inputFields.filter((field) => !sameValue(field, baseline, form)));
}

/** The update-mask fields of an edit: every changed input except a Kubernetes version under automatic upgrades. */
export function clusterUpdateFields(baseline: ClusterForm, form: ClusterForm): ClusterInputField[] {
  return [...changedClusterFields(baseline, form)].filter((field) => !(field === "kubernetesVersion" && automatic(form)));
}

const fieldLabels: Partial<Record<ClusterInputField, string>> = {
  zoneIds: "Availability zone IDs", serviceCidr: "Kubernetes service CIDR", awsAdditionalSecurityGroups: "Additional cluster security groups",
  awsAuthenticationMode: "AWS authentication mode",
};
export const clusterFieldLabel = (field: string) => fieldLabels[field as ClusterInputField] ?? field.replace(/Yaml$/, "").replace(/([A-Z])/g, " $1").toLowerCase();

/**
 * A sparse, reviewed update: the mask is exactly the paths whose value differs from
 * `baselineForm` (the form as it opened), never the whole spec. `baseline` supplies the
 * id, etag and the stored values that rules such as "encryption stays on" compare with.
 */
export function buildClusterUpdate(tenantId: string, baseline: Cluster, form: ClusterForm, baselineForm: ClusterForm = clusterForm(baseline)): UpdateClusterRequest {
  if (!baseline.etag) throw new Error("Reload the cluster: an etag is required for a reviewed update.");
  if (form.name !== baseline.name || form.networkId !== baseline.spec?.network?.networkId) throw new Error("Name and network are immutable.");
  const changed = changedClusterFields(baselineForm, form);
  for (const field of changed) {
    if (!Object.hasOwn(mutable, field)) throw new Error(`${clusterFieldLabel(field)} is immutable: it is set when the cluster is created.`);
  }
  const selected = clusterUpdateFields(baselineForm, form) as EditableClusterField[];
  if (!selected.length) throw new Error("No changes to preview. Edit at least one setting before requesting a plan.");
  validateRelated(form, new Set(selected));
  const beforeEncryption = baseline.spec?.controlPlane?.secretsEncryption;
  if (selected.some(field => field === "encryptionEnabled" || field === "encryptionKeyArn") && (beforeEncryption?.enabled || beforeEncryption?.kmsKeyArn)) {
    const encryption = valueOf("encryptionEnabled", form) as JsonObject | undefined;
    if (!encryption?.enabled || (encryption.kmsKeyArn ?? "") !== (beforeEncryption.kmsKeyArn ?? "")) throw new Error("Encryption cannot be disabled or moved to another key once enabled.");
  }
  const patch: JsonObject = { id: baseline.id, etag: baseline.etag };
  for (const field of selected) {
    setPath(patch, mutable[field], valueOf(field, form));
  }
  return create(UpdateClusterRequestSchema, { tenantId, cluster: fromJson(ClusterSchema, patch), updateMask: { paths: [...new Set(selected.map((field) => mutable[field]))].sort() }, validateOnly: true });
}

export interface ReviewedClusterPlan { request: UpdateClusterRequest; plan: Plan; fingerprint: string }
export function clusterRequestFingerprint(request: UpdateClusterRequest): string {
  const normalized = clone(UpdateClusterRequestSchema, request);
  normalized.validateOnly = true;
  normalized.planId = "";
  return toJsonString(UpdateClusterRequestSchema, normalized);
}
export function bindClusterPlan(request: UpdateClusterRequest, plan: Plan): ReviewedClusterPlan {
  return { request: clone(UpdateClusterRequestSchema, request), plan: clone(PlanSchema, plan), fingerprint: clusterRequestFingerprint(request) };
}
export function clusterPlanApplicable(plan: Plan): boolean {
  return !!plan.planId.trim() && plan.applicable && plan.changes.every((change) => [1, 2, 3, 4].includes(change.action));
}
export function applyReviewedClusterPlan(review: ReviewedClusterPlan, current: UpdateClusterRequest): UpdateClusterRequest {
  if (!clusterPlanApplicable(review.plan)) throw new Error("This plan cannot be applied. Request a supported, applicable plan with an ID.");
  if (clusterRequestFingerprint(current) !== review.fingerprint || clusterRequestFingerprint(review.request) !== review.fingerprint) throw new Error("Inputs or etag changed. Review a new plan before applying.");
  const request = clone(UpdateClusterRequestSchema, review.request);
  request.validateOnly = false;
  request.planId = review.plan.planId;
  return request;
}

/** Keep a retry's UUID until any input changes, including an edit back to an old value. */
export function createRequestIdentity(uuid: () => string) {
  let requestId: string | undefined;
  return { invalidate() { requestId = undefined; }, get() { return requestId ??= uuid(); } };
}

export function clusterIsConverging(cluster: Cluster): boolean {
  return [ClusterState.PROVISIONING, ClusterState.UPDATING, ClusterState.DELETING].includes(cluster.status?.state ?? 0) ||
    cluster.status?.import?.phase === 1 || cluster.status?.import?.phase === 4 ||
    (cluster.status?.state !== ClusterState.FAILED && cluster.generation > (cluster.status?.observedGeneration ?? 0n));
}
