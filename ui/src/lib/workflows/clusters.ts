import { clone, create, fromJson, toJson, toJsonString, type JsonObject, type JsonValue } from "@bufbuild/protobuf";
import { ClusterSchema, ClusterState, MaintenanceWindowSchema, UpgradeMode, type Cluster } from "../../gen/cloud/v1/cluster_pb";
import { AccessSpecSchema, OidcIdentityProviderSchema } from "../../gen/cloud/v1/access_pb";
import { TaintSchema } from "../../gen/cloud/v1/node_pool_pb";
import { SecretsEncryptionSchema } from "../../gen/cloud/v1/control_plane_pb";
import { CreateClusterRequestSchema, ImportClusterRequestSchema, UpdateClusterRequestSchema, type UpdateClusterRequest } from "../../gen/cloud/v1/cluster_service_pb";
import { PlanSchema, type Plan } from "../../gen/cloud/v1/plan_pb";
import type { Network } from "../../gen/cloud/v1/network_pb";
import type { CloudAccount } from "../../gen/cloud/v1/tenant_service_pb";
import { supportsCapability } from "../providers/registry";
import { awsClusterForm, awsClusterFormValue, awsCreateOnlyClusterFields, awsMutableClusterFields, emptyAwsClusterFields, validateAwsNodeForm, type AwsClusterFormField } from "../providers/aws-cluster-form";
import { checkedJson, clusterList, formJson, labelValue, nestedFormYaml, optionalBoolean, qualifiedKey, stringMap, typedFormJson } from "./cluster-form-values";

export interface ClusterForm {
  name: string; displayName: string; networkId: string;
  releaseChannel: string; upgradeMode: string; kubernetesVersion: string;
  publicAccess: boolean; privateAccess: boolean; publicCIDRs: string;
  instanceTypes: string; minSize: string; maxSize: string; diskSize: string;
  capacityType: string; adminPrincipals: string; irsa: string;
  naas: boolean;
  labels: string; nodeLabels: string; nodeTaints: string; desiredSize: string;
  maintenanceDays: string; maintenanceStart: string; maintenanceDuration: string;
  zoneIds: string; serviceCidr: string; encryptionEnabled: string; encryptionKeyArn: string;
  awsAmiType: string; awsAdditionalSecurityGroups: string; awsAuthenticationMode: string;
  awsSupportType: string; awsLogGroupEnabled: string; awsLogRetentionDays: string; awsLogTypes: string;
  oidcYaml: string; accessEntriesYaml: string; launchTemplateYaml: string; addonsYaml: string;
}

export const emptyClusterForm: ClusterForm = {
  name: "", displayName: "", networkId: "", releaseChannel: "0", upgradeMode: "1",
  kubernetesVersion: "", publicAccess: true, privateAccess: true, publicCIDRs: "",
  instanceTypes: "t3.medium", minSize: "1", maxSize: "3", diskSize: "", capacityType: "0",
  adminPrincipals: "", irsa: "default", naas: false,
  labels: "{}", nodeLabels: "{}", nodeTaints: "[]", desiredSize: "",
  maintenanceDays: "", maintenanceStart: "", maintenanceDuration: "", zoneIds: "", serviceCidr: "",
  encryptionEnabled: "", encryptionKeyArn: "", oidcYaml: "", accessEntriesYaml: "",
  ...emptyAwsClusterFields,
};

const split = (text: string) => text.split(/[\s,]+/).filter(Boolean);

export function clusterForm(cluster: Cluster): ClusterForm {
  const spec = cluster.spec;
  const endpoint = spec?.controlPlane?.endpointAccess;
  const pool = spec?.defaultNodePool;
  const aws = spec?.provider.case === "aws" ? spec.provider.value : undefined;
  const window = spec?.maintenanceWindow;
  const encryption = spec?.controlPlane?.secretsEncryption;
  return {
    name: cluster.name, displayName: cluster.displayName, networkId: spec?.network?.networkId ?? "",
    releaseChannel: String(spec?.releaseChannel ?? 0), upgradeMode: String(spec?.upgradeMode || UpgradeMode.MANUAL),
    kubernetesVersion: spec?.controlPlane?.kubernetesVersion ?? "",
    publicAccess: endpoint?.endpointPublicAccess ?? true, privateAccess: endpoint?.endpointPrivateAccess ?? true,
    publicCIDRs: endpoint?.publicAccessCidrs.join(", ") ?? "", instanceTypes: pool?.instanceTypes.join(", ") ?? "",
    minSize: String(pool?.scalingConfig?.minSize ?? 0), maxSize: String(pool?.scalingConfig?.maxSize ?? 1),
    diskSize: pool?.diskSizeGb ? String(pool.diskSizeGb) : "", capacityType: String(pool?.capacityType ?? 0),
    adminPrincipals: spec?.access?.adminPrincipals.join(", ") ?? "",
    irsa: aws?.irsa === undefined ? "default" : String(aws.irsa),
    naas: spec?.naas ?? false,
    labels: JSON.stringify(cluster.labels), nodeLabels: JSON.stringify(pool?.labels ?? {}),
    nodeTaints: JSON.stringify(pool?.taints.map(({ key, value, effect }) => ({ key, value, effect })) ?? []),
    desiredSize: pool?.scalingConfig?.desiredSize === undefined ? "" : String(pool.scalingConfig.desiredSize),
    maintenanceDays: window?.days.join(", ") ?? "", maintenanceStart: window?.start ?? "", maintenanceDuration: window?.duration ?? "",
    zoneIds: spec?.network?.zoneIds.join(", ") ?? "", serviceCidr: spec?.network?.serviceCidr ?? "",
    encryptionEnabled: encryption ? String(encryption.enabled || !!encryption.kmsKeyArn) : "", encryptionKeyArn: encryption?.kmsKeyArn ?? "",
    oidcYaml: spec?.access?.oidc ? JSON.stringify(toJson(OidcIdentityProviderSchema, spec.access.oidc), null, 2) : "",
    accessEntriesYaml: spec?.access?.accessEntries.length ? JSON.stringify((toJson(AccessSpecSchema, spec.access) as JsonObject).accessEntries, null, 2) : "",
    ...awsClusterForm(cluster),
  };
}

// The only provider adapter in this workflow. Never infer AWS from an unknown account.
export function clusterProvider(network: Network, accounts: readonly CloudAccount[], operation: "cluster.create" | "cluster.import" = "cluster.create"): string {
  const account = accounts.find((item) => item.id === network.spec?.bootstrapId);
  const provider = account?.provider.case ?? "unknown";
  if (!supportsCapability(provider, operation)) throw new Error("This network's account has no supported cluster provider.");
  return provider;
}

function awsCreateSettings(form: ClusterForm): JsonObject {
  const irsa = optionalBoolean(form.irsa, "IRSA");
  return irsa === undefined ? {} : { irsa };
}

const mutable = {
  displayName: "display_name",
  releaseChannel: "spec.release_channel",
  upgradeMode: "spec.upgrade_mode",
  kubernetesVersion: "spec.control_plane.kubernetes_version",
  publicAccess: "spec.control_plane.endpoint_access.endpoint_public_access",
  privateAccess: "spec.control_plane.endpoint_access.endpoint_private_access",
  publicCIDRs: "spec.control_plane.endpoint_access.public_access_cidrs",
  instanceTypes: "spec.default_node_pool.instance_types",
  diskSize: "spec.default_node_pool.disk_size_gb",
  capacityType: "spec.default_node_pool.capacity_type",
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
/** The default (system) node group is the platform's: the server refuses (FailedPrecondition) any change to its
 * taints, labels or scaling, so these are set when creating a cluster and never sent in an update mask. */
const platformManagedPool = {
  minSize: "spec.default_node_pool.scaling_config.min_size", maxSize: "spec.default_node_pool.scaling_config.max_size",
  nodeLabels: "spec.default_node_pool.labels", nodeTaints: "spec.default_node_pool.taints",
} as const;
export const platformManagedClusterFields: readonly string[] = Object.keys(platformManagedPool);
const createOnly = { ...platformManagedPool, desiredSize: "spec.default_node_pool.scaling_config.desired_size", zoneIds: "spec.network.zone_ids", serviceCidr: "spec.network.service_cidr", ...awsCreateOnlyClusterFields } as const;
type ClusterInputField = EditableClusterField | keyof typeof createOnly;
const allPaths = { ...mutable, ...createOnly };

export const clusterWizardSteps = ["Cluster settings", "Compute", "Access", "Review"] as const;
const stepFields: readonly (readonly ClusterInputField[])[] = [
  ["displayName", "labels", "releaseChannel", "upgradeMode", "kubernetesVersion", "naas", "maintenanceDays", "maintenanceStart", "maintenanceDuration"],
  ["instanceTypes", "minSize", "maxSize", "diskSize", "capacityType", "desiredSize", "nodeLabels", "nodeTaints", "awsAmiType", "launchTemplateYaml"],
  ["publicAccess", "privateAccess", "publicCIDRs", "adminPrincipals", "irsa", "zoneIds", "serviceCidr", "addonsYaml", "encryptionEnabled", "encryptionKeyArn", "oidcYaml", "accessEntriesYaml", "awsAdditionalSecurityGroups", "awsAuthenticationMode", "awsSupportType", "awsLogGroupEnabled", "awsLogRetentionDays", "awsLogTypes"],
];

/** Called only on Next/submit; editing validates just explicitly touched inputs. */
export function validateClusterFormStep(form: ClusterForm, step: number, touched?: ReadonlySet<string>) {
  if (!stepFields[step]) throw new Error("Unknown cluster form step.");
  if (step === 0 && !touched) {
    if (!/^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$/.test(form.name)) throw new Error("Name must be a lowercase DNS label starting with a letter, at most 40 characters.");
    if (!form.networkId) throw new Error("Choose an existing network.");
  }
  const fields = stepFields[step].filter((field) => (!touched || touched.has(field)) && !(field === "kubernetesVersion" && Number(form.upgradeMode) === UpgradeMode.AUTOMATIC));
  validateRelated(form, new Set(fields));
  for (const field of fields) valueOf(field, form);
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
    case "capacityType": return integer(form[field], 0, 2, "Capacity type");
    case "minSize": return integer(form[field], 0, 1000, "Minimum nodes");
    case "maxSize": return integer(form[field], 1, 1000, "Maximum nodes");
    case "diskSize": return form.diskSize ? integer(form.diskSize, 20, 16384, "Disk size") : 0;
    case "instanceTypes": {
      const values = split(form[field]);
      if (!values.length || values.length > 20 || new Set(values).size !== values.length) throw new Error("Enter 1–20 unique instance types.");
      if (values.some((value) => !/^[a-z][a-z0-9-]*[.][a-z0-9]+$/.test(value))) throw new Error("AWS instance types must look like m6i.large.");
      return values;
    }
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
      if (Number(form.upgradeMode) === UpgradeMode.AUTOMATIC) throw new Error("Automatic upgrades cannot pin a Kubernetes version.");
      if (form[field] && !/^1\.\d{2}$/.test(form[field])) throw new Error("Kubernetes version must be a minor version such as 1.33, or empty for the server default.");
      return form[field];
    }
    case "displayName": {
      if (form[field].length > 128) throw new Error("Display name must be at most 128 characters.");
      return form[field];
    }
    case "irsa": return optionalBoolean(form.irsa, "IRSA");
    case "desiredSize": return form.desiredSize ? integer(form.desiredSize, 0, 1000, "Desired nodes") : undefined;
    case "labels": {
      const labels = stringMap(form.labels, 40, "Cloud tags");
      for (const [key, value] of Object.entries(labels)) if (!/^[A-Za-z0-9 _.:/=+@-]{1,128}$/.test(key) || key.toLowerCase().startsWith("aws:") || !/^[A-Za-z0-9 _.:/=+@-]{0,256}$/.test(value)) throw new Error("Cloud tags must follow AWS tag rules and cannot use the aws: prefix.");
      return labels;
    }
    case "nodeLabels": {
      const labels = stringMap(form.nodeLabels, 64, "Node labels");
      if (Object.entries(labels).some(([key, value]) => !qualifiedKey(key) || !labelValue(value))) throw new Error("Node labels need Kubernetes qualified keys and label values.");
      return labels;
    }
    case "nodeTaints": {
      const values = formJson(form.nodeTaints || "[]", "Node taints");
      if (!Array.isArray(values) || values.length > 50) throw new Error("Node taints must be a JSON array of at most 50 entries.");
      const seen = new Set<string>();
      return values.map(value => {
        const taint = checkedJson(TaintSchema, value);
        if (!qualifiedKey(taint.key) || !labelValue(taint.value)) throw new Error("Taints need Kubernetes qualified keys and label values.");
        const key = `${taint.key}/${taint.effect}`;
        if (seen.has(key)) throw new Error("Duplicate taint key/effect pair.");
        seen.add(key);
        return toJson(TaintSchema, taint);
      });
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
    case "awsAmiType": case "awsAdditionalSecurityGroups": case "awsAuthenticationMode": case "awsSupportType": case "awsLogGroupEnabled": case "awsLogRetentionDays": case "awsLogTypes": case "launchTemplateYaml": case "addonsYaml":
      return awsClusterFormValue(field as AwsClusterFormField, form);
    default: return form[field];
  }
}

function validateRelated(form: ClusterForm, fields: ReadonlySet<string>) {
  if ((fields.has("minSize") || fields.has("maxSize")) && Number(form.minSize) > Number(form.maxSize)) throw new Error("Minimum nodes must not exceed maximum nodes.");
  if (["publicAccess", "privateAccess", "publicCIDRs"].some((field) => fields.has(field))) {
    if (!form.publicAccess && !form.privateAccess) throw new Error("Enable at least one API endpoint.");
    if (form.publicAccess && !split(form.publicCIDRs).length) throw new Error("Public API access requires at least one allowed CIDR.");
  }
  if (fields.has("desiredSize") && form.desiredSize && (Number(form.desiredSize) < Number(form.minSize) || Number(form.desiredSize) > Number(form.maxSize))) throw new Error("Desired nodes must be between minimum and maximum nodes.");
  if (["launchTemplateYaml", "diskSize", "awsAmiType"].some(field => fields.has(field))) validateAwsNodeForm(form);
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

export function buildCreateCluster(tenantId: string, form: ClusterForm, network: Network, accounts: readonly CloudAccount[], requestId: string) {
  clusterProvider(network, accounts);
  if (network.id !== form.networkId || !network.spec?.region) throw new Error("Choose an existing network with a region.");
  if (!/^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$/.test(form.name)) throw new Error("Name must be a lowercase DNS label starting with a letter, at most 40 characters.");
  validateRelated(form, new Set(Object.keys(allPaths)));
  const input: JsonObject = { name: form.name, spec: { region: network.spec.region, network: { networkId: network.id }, aws: awsCreateSettings(form) } };
  for (const field of Object.keys(allPaths) as ClusterInputField[]) {
    if (field === "irsa" || (field === "kubernetesVersion" && Number(form.upgradeMode) === UpgradeMode.AUTOMATIC)) continue;
    if (!(field in emptyClusterForm)) continue;
    if (typeof form[field] === "string" && form[field] === "" && !["displayName", "kubernetesVersion", "adminPrincipals", "publicCIDRs"].includes(field)) continue;
    setPath(input, allPaths[field], valueOf(field, form));
  }
  return create(CreateClusterRequestSchema, { tenantId, requestId, cluster: checkedJson(ClusterSchema, input) });
}

/** Deliberately sparse even when GetCluster(asRequested) falls back to resolved defaults. */
export function buildClusterUpdate(tenantId: string, baseline: Cluster, form: ClusterForm, touched: ReadonlySet<string>): UpdateClusterRequest {
  if (!baseline.etag) throw new Error("Reload the cluster: an etag is required for a reviewed update.");
  if (form.name !== baseline.name || form.networkId !== baseline.spec?.network?.networkId) throw new Error("Name and network are immutable.");
  for (const field of touched) {
    if (platformManagedClusterFields.includes(field)) throw new Error(`The default node group is managed by the platform (the system group runs the platform): ${field} cannot be changed.`);
    if (!Object.hasOwn(mutable, field)) throw new Error(`Field is immutable or unsupported: ${field}`);
  }
  const fields = [...touched] as EditableClusterField[];
  const effective = { ...clusterForm(baseline) };
  for (const field of fields) Object.assign(effective, { [field]: form[field] });
  // Changing mode must never send a resolved version back as a user pin.
  const selected = fields.filter((field) => !(field === "kubernetesVersion" && Number(effective.upgradeMode) === UpgradeMode.AUTOMATIC));
  if (!selected.length) throw new Error("Edit at least one mutable field before requesting a plan.");
  validateRelated(effective, new Set(selected));
  const beforeEncryption = baseline.spec?.controlPlane?.secretsEncryption;
  if (selected.some(field => field === "encryptionEnabled" || field === "encryptionKeyArn") && (beforeEncryption?.enabled || beforeEncryption?.kmsKeyArn)) {
    const encryption = valueOf("encryptionEnabled", effective) as JsonObject | undefined;
    if (!encryption?.enabled || (encryption.kmsKeyArn ?? "") !== (beforeEncryption.kmsKeyArn ?? "")) throw new Error("Encryption cannot be disabled or moved to another key once enabled.");
  }
  const patch: JsonObject = { id: baseline.id, etag: baseline.etag };
  for (const field of selected) setPath(patch, mutable[field], valueOf(field, effective));
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
