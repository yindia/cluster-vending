import { clone, create, fromJson, toJsonString, type JsonObject } from "@bufbuild/protobuf";
import { ClusterSchema, ClusterState, UpgradeMode, type Cluster } from "../../gen/cloud/v1/cluster_pb";
import { CreateClusterRequestSchema, ImportClusterRequestSchema, UpdateClusterRequestSchema, type UpdateClusterRequest } from "../../gen/cloud/v1/cluster_service_pb";
import { PlanSchema, type Plan } from "../../gen/cloud/v1/plan_pb";
import type { Network } from "../../gen/cloud/v1/network_pb";
import type { CloudAccount } from "../../gen/cloud/v1/tenant_service_pb";
import { supportsCapability } from "../providers/registry";

export interface ClusterForm {
  name: string; displayName: string; networkId: string;
  releaseChannel: string; upgradeMode: string; kubernetesVersion: string;
  publicAccess: boolean; privateAccess: boolean; publicCIDRs: string;
  instanceTypes: string; minSize: string; maxSize: string; diskSize: string;
  capacityType: string; adminPrincipals: string; irsa: string;
}

export const emptyClusterForm: ClusterForm = {
  name: "", displayName: "", networkId: "", releaseChannel: "0", upgradeMode: "1",
  kubernetesVersion: "", publicAccess: true, privateAccess: true, publicCIDRs: "",
  instanceTypes: "m6i.large", minSize: "1", maxSize: "3", diskSize: "", capacityType: "0",
  adminPrincipals: "", irsa: "default",
};

const split = (text: string) => text.split(/[\s,]+/).filter(Boolean);

export function clusterForm(cluster: Cluster): ClusterForm {
  const spec = cluster.spec;
  const endpoint = spec?.controlPlane?.endpointAccess;
  const pool = spec?.defaultNodePool;
  const aws = spec?.provider.case === "aws" ? spec.provider.value : undefined;
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
  };
}

// The only provider adapter in this workflow. Never infer AWS from an unknown account.
export function clusterProvider(network: Network, accounts: readonly CloudAccount[]): string {
  const account = accounts.find((item) => item.id === network.spec?.bootstrapId);
  const provider = account?.provider.case ?? "unknown";
  if (!supportsCapability(provider, "cluster.create")) throw new Error("This network's account has no supported cluster provider.");
  return provider;
}

function awsCreateSettings(form: ClusterForm): JsonObject {
  return form.irsa === "default" ? {} : { irsa: form.irsa === "true" };
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
  minSize: "spec.default_node_pool.scaling_config.min_size",
  maxSize: "spec.default_node_pool.scaling_config.max_size",
  diskSize: "spec.default_node_pool.disk_size_gb",
  capacityType: "spec.default_node_pool.capacity_type",
  adminPrincipals: "spec.access.admin_principals",
  irsa: "spec.aws.irsa",
} as const;
export type EditableClusterField = keyof typeof mutable;

export const clusterWizardSteps = ["Basics", "Compute", "Access", "Review"] as const;
const stepFields: readonly (readonly EditableClusterField[])[] = [
  ["displayName", "releaseChannel", "upgradeMode", "kubernetesVersion"],
  ["instanceTypes", "minSize", "maxSize", "diskSize", "capacityType"],
  ["publicAccess", "privateAccess", "publicCIDRs", "adminPrincipals", "irsa"],
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
  clusterProvider(network, accounts);
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

function valueOf(field: EditableClusterField, form: ClusterForm) {
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
    case "adminPrincipals": return split(form[field]);
    case "kubernetesVersion": {
      if (Number(form.upgradeMode) === UpgradeMode.AUTOMATIC) throw new Error("Automatic upgrades cannot pin a Kubernetes version.");
      if (form[field] && !/^1\.\d{2}$/.test(form[field])) throw new Error("Kubernetes version must be a minor version such as 1.33, or empty for the server default.");
      return form[field];
    }
    case "displayName": {
      if (form[field].length > 128) throw new Error("Display name must be at most 128 characters.");
      return form[field];
    }
    case "irsa": return form.irsa === "default" ? undefined : form.irsa === "true";
    default: return form[field];
  }
}

function validateRelated(form: ClusterForm, fields: ReadonlySet<string>) {
  if ((fields.has("minSize") || fields.has("maxSize")) && Number(form.minSize) > Number(form.maxSize)) throw new Error("Minimum nodes must not exceed maximum nodes.");
  if (["publicAccess", "privateAccess", "publicCIDRs"].some((field) => fields.has(field))) {
    if (!form.publicAccess && !form.privateAccess) throw new Error("Enable at least one API endpoint.");
    if (form.publicAccess && !split(form.publicCIDRs).length) throw new Error("Public API access requires at least one allowed CIDR.");
  }
}

// JSON uses lowerCamelCase; protobuf FieldMask paths use snake_case.
function setPath(target: JsonObject, path: string, value: ReturnType<typeof valueOf>) {
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
  validateRelated(form, new Set(Object.keys(mutable)));
  const input: JsonObject = { name: form.name, spec: { region: network.spec.region, network: { networkId: network.id }, aws: awsCreateSettings(form) } };
  for (const field of Object.keys(mutable) as EditableClusterField[]) {
    if (field === "irsa" || (field === "kubernetesVersion" && Number(form.upgradeMode) === UpgradeMode.AUTOMATIC)) continue;
    setPath(input, mutable[field], valueOf(field, form));
  }
  return create(CreateClusterRequestSchema, { tenantId, requestId, cluster: fromJson(ClusterSchema, input) });
}

/** Deliberately sparse even when GetCluster(asRequested) falls back to resolved defaults. */
export function buildClusterUpdate(tenantId: string, baseline: Cluster, form: ClusterForm, touched: ReadonlySet<string>): UpdateClusterRequest {
  if (!baseline.etag) throw new Error("Reload the cluster: an etag is required for a reviewed update.");
  if (form.name !== baseline.name || form.networkId !== baseline.spec?.network?.networkId) throw new Error("Name and network are immutable.");
  for (const field of touched) if (!Object.hasOwn(mutable, field)) throw new Error(`Field is immutable or unsupported: ${field}`);
  const fields = [...touched] as EditableClusterField[];
  // Changing mode must never send a resolved version back as a user pin.
  const selected = fields.filter((field) => !(field === "kubernetesVersion" && Number(form.upgradeMode) === UpgradeMode.AUTOMATIC));
  if (!selected.length) throw new Error("Edit at least one mutable field before requesting a plan.");
  validateRelated(form, new Set(selected));
  const patch: JsonObject = { id: baseline.id, etag: baseline.etag };
  for (const field of selected) setPath(patch, mutable[field], valueOf(field, form));
  return create(UpdateClusterRequestSchema, { tenantId, cluster: fromJson(ClusterSchema, patch), updateMask: { paths: selected.map((field) => mutable[field]).sort() }, validateOnly: true });
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
