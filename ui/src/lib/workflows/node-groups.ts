import { clone, create, toJson } from "@bufbuild/protobuf";
import { ClusterSchema, type Cluster } from "../../gen/cloud/v1/cluster_pb";
import { UpdateClusterRequestSchema, type UpdateClusterRequest } from "../../gen/cloud/v1/cluster_service_pb";
import type { GetNodeGroupOptionsResponse, NodeGroupManifest } from "../../gen/cloud/v1/node_group_manifest_pb";
import { Architecture, NodeGroupPhase, NodeGroupSpecSchema, type NodeGroupSpec, type NodeGroupStatus } from "../../gen/cloud/v1/node_group_pb";
import { CapacityType, TaintEffect, type NodePoolSpec } from "../../gen/cloud/v1/node_pool_pb";
import { checkedJson, labelValue, qualifiedKey } from "./cluster-form-values";

/** Mirrors the buf.validate rules on cloud.v1.NodeGroupSpec and ClusterSpec.node_groups. */
export const MAX_NODE_GROUPS = 20;
const MAX_NAME = 40;
const MAX_LIST = 20;
const MAX_LABELS = 64;
const MAX_TAINTS = 50;
const MAX_LIMIT = 2_147_483_647;
/** The platform's one node group: the default EKS managed node group. */
const systemGroupName = "system";
/** "platform" was a Karpenter node group of an earlier layout; it stays reserved. */
const reservedNames = ["platform", "default", systemGroupName];
export const nodeGroupSteps = ["Node group", "Scheduling", "Review"] as const;
export const nodeGroupRemoveSteps = ["Remove", "Review"] as const;

export interface LabelDraft { key: string; value: string }
export interface TaintDraft { key: string; value: string; effect: TaintEffect }
export interface NodeGroupForm {
  name: string; instanceTypes: string[]; instanceFamilies: string[];
  architectures: Architecture[]; capacityTypes: CapacityType[];
  labels: LabelDraft[]; taints: TaintDraft[]; cpu: string; memoryGib: string;
}
export type NodeGroupField = "name" | "instanceTypes" | "instanceFamilies" | "architectures" | "capacityTypes" | "labels" | "taints" | "cpu" | "memoryGib";
export interface NodeGroupIssue { field: NodeGroupField; message: string }

export function emptyNodeGroupForm(): NodeGroupForm {
  return { name: "", instanceTypes: [], instanceFamilies: [], architectures: [Architecture.AMD64], capacityTypes: [CapacityType.ON_DEMAND], labels: [], taints: [], cpu: "", memoryGib: "" };
}

/** An empty architecture or capacity list means the server default; show it as that explicit choice. */
export function nodeGroupForm(group: NodeGroupSpec): NodeGroupForm {
  return {
    name: group.name, instanceTypes: [...group.instanceTypes], instanceFamilies: [...group.instanceFamilies],
    architectures: group.architectures.length ? [...group.architectures] : [Architecture.AMD64],
    capacityTypes: group.capacityTypes.length ? [...group.capacityTypes] : [CapacityType.ON_DEMAND],
    labels: Object.entries(group.labels).map(([key, value]) => ({ key, value })),
    taints: group.taints.map(({ key, value, effect }) => ({ key, value, effect })),
    cpu: group.limits?.cpu === undefined ? "" : String(group.limits.cpu),
    memoryGib: group.limits?.memoryGib === undefined ? "" : String(group.limits.memoryGib),
  };
}

function nameIssue(name: string): string | undefined {
  if (reservedNames.includes(name)) return `Name "${name}" is reserved by the platform.`;
  if (name.length > MAX_NAME || !/^[a-z]([-a-z0-9]*[a-z0-9])?$/.test(name)) return "Name must be a lowercase DNS label starting with a letter, at most 40 characters.";
}

function listIssues(values: readonly string[], pattern: RegExp, field: "instanceTypes" | "instanceFamilies", noun: string, example: string): NodeGroupIssue[] {
  if (values.length > MAX_LIST) return [{ field, message: `Enter at most ${MAX_LIST} ${noun}.` }];
  if (values.some(value => value.length > (field === "instanceTypes" ? 64 : 32) || !pattern.test(value))) return [{ field, message: `${noun[0].toUpperCase() + noun.slice(1)} must look like ${example}.` }];
  if (new Set(values).size !== values.length) return [{ field, message: `${noun[0].toUpperCase() + noun.slice(1)} must be unique.` }];
  return [];
}

function choiceIssues<T extends number>(values: readonly T[], allowed: readonly T[], field: "architectures" | "capacityTypes", noun: string): NodeGroupIssue[] {
  if (!values.length || values.some(value => !allowed.includes(value)) || new Set(values).size !== values.length) return [{ field, message: `Choose at least one ${noun}.` }];
  return [];
}

function limitIssue(text: string, field: "cpu" | "memoryGib", label: string): NodeGroupIssue[] {
  const value = text.trim();
  if (!value) return [];
  if (!/^\d+$/.test(value) || Number(value) < 1 || Number(value) > MAX_LIMIT) return [{ field, message: `${label} limit must be a whole number from 1 to ${MAX_LIMIT}, or empty for no limit.` }];
  return [];
}

function labelIssues(labels: readonly LabelDraft[]): NodeGroupIssue[] {
  if (labels.length > MAX_LABELS) return [{ field: "labels", message: `Enter at most ${MAX_LABELS} labels.` }];
  const seen = new Map<string, number>();
  for (const [index, { key, value }] of labels.entries()) {
    const label = `Label ${index + 1}`;
    if (!key.trim()) return [{ field: "labels", message: `${label}: enter a key or remove the row.` }];
    if (!qualifiedKey(key)) return [{ field: "labels", message: `${label}: the key must be a Kubernetes qualified name.` }];
    if (!labelValue(value) || value.length > 63) return [{ field: "labels", message: `${label}: the value must be a Kubernetes label value.` }];
    const first = seen.get(key);
    if (first !== undefined) return [{ field: "labels", message: `${label}: this key already appears in row ${first}.` }];
    seen.set(key, index + 1);
  }
  return [];
}

function taintIssues(taints: readonly TaintDraft[]): NodeGroupIssue[] {
  if (taints.length > MAX_TAINTS) return [{ field: "taints", message: `Enter at most ${MAX_TAINTS} taints.` }];
  const seen = new Map<string, number>();
  for (const [index, { key, value, effect }] of taints.entries()) {
    const label = `Taint ${index + 1}`;
    if (!key.trim()) return [{ field: "taints", message: `${label}: enter a key or remove the row.` }];
    if (!qualifiedKey(key)) return [{ field: "taints", message: `${label}: the key must be a Kubernetes qualified name.` }];
    if (!labelValue(value) || value.length > 63) return [{ field: "taints", message: `${label}: the value must be a Kubernetes label value.` }];
    if (![TaintEffect.NO_SCHEDULE, TaintEffect.PREFER_NO_SCHEDULE, TaintEffect.NO_EXECUTE].includes(effect)) return [{ field: "taints", message: `${label}: choose an effect.` }];
    const identity = `${key}/${effect}`;
    const first = seen.get(identity);
    if (first !== undefined) return [{ field: "taints", message: `${label}: this key and effect pair already appears in row ${first}.` }];
    seen.set(identity, index + 1);
  }
  return [];
}

/** What the platform administrator allows (GetNodeGroupOptions): an empty list restricts nothing in its dimension. */
export interface NodeGroupAllowlist { instanceFamilies: readonly string[]; instanceTypes: readonly string[] }

export function nodeGroupAllowlist(options?: GetNodeGroupOptionsResponse): NodeGroupAllowlist {
  return { instanceFamilies: [...(options?.instanceFamilies ?? [])], instanceTypes: [...(options?.instanceTypes ?? [])] };
}

/** The server's rule, for early feedback: the server stays the authority. A type is allowed when listed or when its family is. */
function allowlistIssues(form: NodeGroupForm, allow: NodeGroupAllowlist | undefined): NodeGroupIssue[] {
  if (!allow || (!allow.instanceFamilies.length && !allow.instanceTypes.length)) return [];
  const listed = [allow.instanceFamilies.length ? `allowed families: ${allow.instanceFamilies.join(", ")}` : "", allow.instanceTypes.length ? `allowed types: ${allow.instanceTypes.join(", ")}` : ""].filter(Boolean).join("; ");
  const issues: NodeGroupIssue[] = [];
  if (allow.instanceFamilies.length) {
    const bad = form.instanceFamilies.find(family => !allow.instanceFamilies.includes(family));
    if (bad !== undefined) issues.push({ field: "instanceFamilies", message: `Instance family "${bad}" is not allowed; ${listed}.` });
  }
  const badType = form.instanceTypes.find(type => !allow.instanceTypes.includes(type) && !allow.instanceFamilies.includes(type.split(".")[0]));
  if (badType !== undefined) issues.push({ field: "instanceTypes", message: `Instance type "${badType}" is not allowed; ${listed}.` });
  return issues;
}

/** `others` are the names of the cluster's other node groups (not the one being edited); `allow` is the platform allow-list, when known. */
export function nodeGroupIssues(form: NodeGroupForm, others: readonly string[], allow?: NodeGroupAllowlist): NodeGroupIssue[] {
  const issues: NodeGroupIssue[] = [];
  const name = nameIssue(form.name);
  if (name) issues.push({ field: "name", message: name });
  else if (others.includes(form.name)) issues.push({ field: "name", message: `A node group named "${form.name}" already exists.` });
  if (!form.instanceTypes.length && !form.instanceFamilies.length) issues.push({ field: "instanceTypes", message: "Enter at least one instance type or family." });
  issues.push(
    ...listIssues(form.instanceTypes, /^[a-z][a-z0-9-]*[.][a-z0-9]+$/, "instanceTypes", "instance types", "m7i.large"),
    ...listIssues(form.instanceFamilies, /^[a-z][a-z0-9-]*$/, "instanceFamilies", "instance families", "m7i"),
    ...choiceIssues(form.architectures, [Architecture.AMD64, Architecture.ARM64], "architectures", "architecture"),
    ...choiceIssues(form.capacityTypes, [CapacityType.ON_DEMAND, CapacityType.SPOT], "capacityTypes", "capacity type"),
    ...labelIssues(form.labels), ...taintIssues(form.taints),
    ...limitIssue(form.cpu, "cpu", "CPU"), ...limitIssue(form.memoryGib, "memoryGib", "Memory"),
    ...allowlistIssues(form, allow),
  );
  return issues;
}

const stepFields: readonly (readonly NodeGroupField[])[] = [
  ["name", "instanceTypes", "instanceFamilies", "architectures", "capacityTypes"],
  ["labels", "taints", "cpu", "memoryGib"],
];
/** Called only on Next; throws the first issue of that step. */
export function validateNodeGroupStep(form: NodeGroupForm, others: readonly string[], step: number, allow?: NodeGroupAllowlist) {
  if (!stepFields[step]) throw new Error("Unknown node group step.");
  const issue = nodeGroupIssues(form, others, allow).find(item => stepFields[step].includes(item.field));
  if (issue) throw new Error(issue.message);
}

export function buildNodeGroupSpec(form: NodeGroupForm, others: readonly string[], allow?: NodeGroupAllowlist): NodeGroupSpec {
  const issue = nodeGroupIssues(form, others, allow)[0];
  if (issue) throw new Error(issue.message);
  const cpu = form.cpu.trim(), memoryGib = form.memoryGib.trim();
  const draft = create(NodeGroupSpecSchema, {
    name: form.name, instanceTypes: form.instanceTypes, instanceFamilies: form.instanceFamilies,
    architectures: form.architectures, capacityTypes: form.capacityTypes,
    labels: Object.fromEntries(form.labels.map(({ key, value }) => [key, value])),
    taints: form.taints.map(({ key, value, effect }) => ({ key, value, effect })),
    ...(cpu || memoryGib ? { limits: { ...(cpu ? { cpu: Number(cpu) } : {}), ...(memoryGib ? { memoryGib: Number(memoryGib) } : {}) } } : {}),
  });
  // The generated rules are the final guard against drift from the checks above.
  return checkedJson(NodeGroupSpecSchema, toJson(NodeGroupSpecSchema, draft));
}

export type NodeGroupChange =
  | { kind: "add"; group: NodeGroupSpec }
  | { kind: "update"; name: string; group: NodeGroupSpec }
  | { kind: "remove"; name: string };

function validateNodeGroupList(groups: readonly NodeGroupSpec[]) {
  if (groups.length > MAX_NODE_GROUPS) throw new Error(`A cluster allows at most ${MAX_NODE_GROUPS} node groups.`);
  const seen = new Set<string>();
  for (const { name } of groups) {
    const issue = nameIssue(name);
    if (issue) throw new Error(issue);
    if (seen.has(name)) throw new Error(`A node group named "${name}" already exists.`);
    seen.add(name);
  }
}

/** Returns a new list; the input and its messages are never mutated. */
export function applyNodeGroupChange(current: readonly NodeGroupSpec[], change: NodeGroupChange): NodeGroupSpec[] {
  const copy = current.map(group => clone(NodeGroupSpecSchema, group));
  let next: NodeGroupSpec[];
  if (change.kind === "add") next = [...copy, clone(NodeGroupSpecSchema, change.group)];
  else {
    const index = copy.findIndex(group => group.name === change.name);
    if (index < 0) throw new Error(`Node group "${change.name}" was not found. Reload the cluster.`);
    next = change.kind === "remove" ? copy.filter((_, position) => position !== index)
      : copy.map((group, position) => position === index ? clone(NodeGroupSpecSchema, change.group) : group);
  }
  validateNodeGroupList(next);
  return next;
}

/** The whole list is one update-mask path: it is compared and replaced as a unit. */
export function buildNodeGroupsUpdate(tenantId: string, baseline: Cluster, groups: readonly NodeGroupSpec[]): UpdateClusterRequest {
  if (!baseline.etag) throw new Error("Reload the cluster: an etag is required for a reviewed update.");
  validateNodeGroupList(groups);
  return create(UpdateClusterRequestSchema, {
    tenantId, validateOnly: true, updateMask: { paths: ["spec.node_groups"] },
    cluster: create(ClusterSchema, { id: baseline.id, etag: baseline.etag, spec: { nodeGroups: groups.map(group => clone(NodeGroupSpecSchema, group)) } }),
  });
}

export type NodeGroupTone = "success" | "error" | "warning" | "neutral";
export interface NodeGroupRow extends Record<string, unknown> {
  name: string; platformManaged: boolean; editable: boolean;
  /** False while the controller has not reported the group yet. */
  observed: boolean;
  phase: NodeGroupPhase; phaseLabel: string; tone: NodeGroupTone; message: string;
  nodes?: number; spec?: NodeGroupSpec;
  /** The group's Karpenter objects as the controller applies them; absent while unknown, and for the system group (an EKS node group, not Karpenter objects). */
  manifest?: NodeGroupManifest;
  /** What the system group is made of, from the cluster's default node pool; set on that row only. */
  system?: SystemGroupSummary;
}
/** `size` is the autoscaled range ("2–6 (autoscaled)"); `current` the node count now ("3 nodes now"), absent until the controller reports it. */
export interface SystemGroupSummary { instanceTypes: string; capacity: string; size: string; current?: string }

const phaseLabels: Record<number, [string, NodeGroupTone]> = {
  [NodeGroupPhase.UNSPECIFIED]: ["Not reported", "neutral"], [NodeGroupPhase.PENDING]: ["Pending", "warning"],
  [NodeGroupPhase.READY]: ["Ready", "success"], [NodeGroupPhase.FAILED]: ["Failed", "error"], [NodeGroupPhase.DELETING]: ["Deleting", "warning"],
};
/** The label and tone a node group phase is shown with. */
export function phaseDisplay(phase: NodeGroupPhase): [string, NodeGroupTone] {
  return phaseLabels[phase] ?? [`Unknown (${phase})`, "neutral"];
}
const waiting = "Waiting for the controller to report this node group.";
const systemWaiting = "Pending — waiting for the controller";

/** "2–6 (autoscaled)": the default managed node group's minimum and maximum size; cluster-autoscaler moves it between them. */
function systemSize(min: number, max: number): string {
  return `${min}–${max} (autoscaled)`;
}

function systemSummary(pool: NodePoolSpec, nodes: number | undefined): SystemGroupSummary {
  return {
    instanceTypes: pool.instanceTypes.join(", ") || "—", capacity: pool.capacityType === CapacityType.SPOT ? "Spot" : "On-demand",
    size: pool.scalingConfig ? systemSize(pool.scalingConfig.minSize, pool.scalingConfig.maxSize) : "—",
    current: nodes === undefined ? undefined : `${nodes} ${nodes === 1 ? "node" : "nodes"} now`,
  };
}

/** Manifests with something to show: the system group's entry has no YAML. */
export function hasManifestYaml(manifest: NodeGroupManifest | undefined): manifest is NodeGroupManifest {
  return !!manifest && !!(manifest.ec2NodeClassYaml || manifest.nodePoolYaml);
}

/**
 * The system group first, then spec order; spec-only groups are Pending and status-only groups (deleting) stay visible.
 * The system row is always shown (Pending until the controller reports it) unless `manifests` is known and has none: the
 * platform manages no node groups of an imported cluster. Each row carries its manifest by name; `defaultPool` is the
 * cluster's default node pool, which the system row summarizes. A platform group an earlier controller recorded is not shown.
 */
export function nodeGroupRows(spec: readonly NodeGroupSpec[], status: readonly NodeGroupStatus[], manifests?: readonly NodeGroupManifest[], defaultPool?: NodePoolSpec): NodeGroupRow[] {
  // First report wins: a repeated name must never produce two rows (the table keys rows by name).
  const byName = new Map<string, NodeGroupStatus>();
  for (const item of status) if (!byName.has(item.name)) byName.set(item.name, item);
  const manifestOf = new Map<string, NodeGroupManifest>();
  for (const item of manifests ?? []) if (!manifestOf.has(item.name)) manifestOf.set(item.name, item);
  const row = (name: string, observed: NodeGroupStatus | undefined, group?: NodeGroupSpec, platform = false): NodeGroupRow => {
    const phase = observed?.phase ?? NodeGroupPhase.PENDING;
    const [phaseLabel, tone] = phaseLabels[phase] ?? [`Unknown (${phase})`, "neutral" as const];
    const platformManaged = observed?.platformManaged ?? platform;
    const manifest = manifestOf.get(name);
    return {
      name, platformManaged, editable: !platformManaged && !!group && phase !== NodeGroupPhase.DELETING, observed: !!observed,
      phase, phaseLabel, tone, message: observed ? observed.message : platform ? systemWaiting : waiting, nodes: observed?.nodes, spec: group,
      manifest: hasManifestYaml(manifest) ? manifest : undefined,
      system: platformManaged && name === systemGroupName && defaultPool ? systemSummary(defaultPool, observed?.nodes) : undefined,
    };
  };
  // The platform group of an earlier layout (a Karpenter pool) is gone with its pool: never a row.
  const platform = [...byName.values()].filter(item => item.platformManaged && item.name !== "platform").map(item => row(item.name, item));
  if (!platform.length && (!manifests || manifests.some(item => item.platformManaged))) platform.push(row(systemGroupName, undefined, undefined, true));
  const shown = new Set(platform.map(item => item.name));
  const users: NodeGroupRow[] = [];
  for (const group of spec) {
    if (shown.has(group.name)) continue;
    shown.add(group.name);
    users.push(row(group.name, byName.get(group.name), group));
  }
  for (const item of byName.values()) {
    if (shown.has(item.name) || (item.platformManaged && item.name === "platform")) continue;
    shown.add(item.name);
    users.push(row(item.name, item));
  }
  return [...platform, ...users];
}

export interface NodeGroupSummary { capacity: string; instances: string; architectures: string; labels: string; taints: string; limits: string }
const effectLabels: Record<number, string> = { [TaintEffect.NO_SCHEDULE]: "NoSchedule", [TaintEffect.PREFER_NO_SCHEDULE]: "PreferNoSchedule", [TaintEffect.NO_EXECUTE]: "NoExecute" };

export function summarizeNodeGroup(group: NodeGroupSpec): NodeGroupSummary {
  const capacity = group.capacityTypes.length ? [...group.capacityTypes].sort((a, b) => a - b).map(type => type === CapacityType.SPOT ? "Spot" : "On-demand") : ["On-demand"];
  const architectures = group.architectures.length ? [...group.architectures].sort((a, b) => a - b).map(arch => arch === Architecture.ARM64 ? "ARM64" : "AMD64") : ["AMD64"];
  const instances = [...group.instanceTypes, ...group.instanceFamilies.map(family => `${family} family`)];
  const labels = Object.entries(group.labels).map(([key, value]) => `${key}=${value}`);
  const taints = group.taints.map(taint => `${taint.key}${taint.value ? `=${taint.value}` : ""}:${effectLabels[taint.effect] ?? "Unknown"}`);
  const limits = [group.limits?.cpu === undefined ? "" : `${group.limits.cpu} vCPU`, group.limits?.memoryGib === undefined ? "" : `${group.limits.memoryGib} GiB`].filter(Boolean);
  return {
    capacity: capacity.join(", "), instances: instances.join(", ") || "Any", architectures: architectures.join(", "),
    labels: labels.join(", ") || "None", taints: taints.join(", ") || "None", limits: limits.join(" · ") || "No limits",
  };
}
