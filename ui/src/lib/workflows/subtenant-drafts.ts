/** Form state only. API requests are constructed from generated SubTenant messages. */
export interface SubTenantDraft {
  name: string;
  deletionProtection: boolean;
  displayName: string;
  namespaceKeys: string;
  namespaceCount: string;
  quotas: string;
  identityRefs: string;
  adminGroups: string;
  viewerGroups: string;
}

export const emptySubTenantDraft: SubTenantDraft = { name: "", deletionProtection: true, displayName: "", namespaceKeys: "", namespaceCount: "1", quotas: "", identityRefs: "", adminGroups: "", viewerGroups: "" };
export const SUBTENANT_POLICY = "restricted-ambient-v1";
export const SUBTENANT_ROLE = "workload-editor";
export const SUBTENANT_QUOTA_KEYS = ["requests.cpu", "requests.memory", "limits.cpu", "limits.memory", "requests.ephemeral-storage", "limits.ephemeral-storage", "requests.storage", "persistentvolumeclaims", "pods", "services", "secrets", "configmaps"] as const;

function uniqueTokens(value: string, label: string): string[] {
  const tokens = value.trim().split(/[\s,]+/).filter(Boolean);
  if (new Set(tokens).size !== tokens.length) throw new Error(`${label} must not contain duplicates.`);
  return tokens;
}

export function namespaceKeys(value: string): string[] {
  const keys = uniqueTokens(value, "Namespace keys");
  if (!keys.length || keys.length > 64 || keys.some((key) => !/^[a-z0-9](?:[a-z0-9-]{0,25}[a-z0-9])?$/.test(key))) throw new Error("Enter 1–64 unique namespace keys, each a lowercase DNS label of 1–27 characters.");
  return keys;
}

export function quotaEntries(value: string): Record<string, string> {
  const entries = new Map<string, string>();
  for (const line of value.split(/\r?\n/).map((item) => item.trim()).filter(Boolean)) {
    const index = line.indexOf("=");
    if (index < 1) throw new Error("Enter quotas as resource=quantity, one per line.");
    const key = line.slice(0, index).trim(), quantity = line.slice(index + 1).trim();
    if (!(SUBTENANT_QUOTA_KEYS as readonly string[]).includes(key) || !quantity || quantity.length > 64 || /[\s=]/.test(quantity) || quantity.startsWith("-")) throw new Error("Use a supported quota resource and a nonnegative quantity of at most 64 characters. The API validates Kubernetes quantity syntax.");
    if (["persistentvolumeclaims", "pods", "services", "secrets", "configmaps"].includes(key) && !/^\d+$/.test(quantity)) throw new Error("Object-count quotas must be nonnegative whole numbers.");
    if (entries.has(key)) throw new Error(`Duplicate quota resource: ${key}.`);
    entries.set(key, quantity);
  }
  if (!entries.size || entries.size > 12) throw new Error("Enter 1–12 supported resource quotas.");
  return Object.fromEntries(entries);
}

export function validateAccessGroups(admin: readonly string[], viewer: readonly string[]) {
  if (admin.length > 32 || viewer.length > 32) throw new Error("Enter at most 32 groups for each access role.");
  const groups = [...admin, ...viewer];
  if (groups.some((group) => !group || [...group].length > 256 || group !== group.trim() || group.startsWith("system:") || /[\p{Cc}*?]/u.test(group))) throw new Error("Use exact group names of 1–256 characters, without reserved system: names, wildcards or control characters.");
  if (new Set(groups).size !== groups.length) throw new Error("Group names must be unique and cannot occur in both Admin groups and Viewer groups.");
}

export function accessGroups(value: string): string[] {
  const groups = value.split(/[,\r\n]+/).map((group) => group.trim()).filter(Boolean);
  validateAccessGroups(groups, []);
  return groups;
}

export function identityReferences(value: string): string[] {
  const refs = uniqueTokens(value, "Identity references");
  if (refs.length > 32 || refs.some((ref) => !/^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(ref))) throw new Error("Enter at most 32 approved identity references, each a lowercase DNS label of 1–63 characters. Raw groups and subjects are not accepted.");
  return refs;
}

export function validateSubTenantDraft(draft: SubTenantDraft, step?: number, retainedKeys: readonly string[] = [], retainedName?: string) {
  if (step === undefined || step === 0) {
    if (retainedName !== undefined ? draft.name !== retainedName : !/^[a-z0-9](?:[a-z0-9-]{0,33}[a-z0-9])?$/.test(draft.name)) throw new Error(retainedName !== undefined ? "Sub Tenant name is immutable." : "Enter a Sub Tenant name using 1–35 lowercase DNS characters. The name must be unique within this cluster.");
    if (draft.displayName.length > 128) throw new Error("Display name must be at most 128 characters.");
    const keys = namespaceKeys(draft.namespaceKeys);
    if (retainedKeys.some((key) => !keys.includes(key))) throw new Error("Existing namespace keys must be retained. Add keys without removing or renaming existing ones.");
  }
  if (step === undefined || step === 1) {
    if (!/^\d+$/.test(draft.namespaceCount) || Number(draft.namespaceCount) < 1 || Number(draft.namespaceCount) > 64) throw new Error("Namespace quota must be a whole number from 1 to 64.");
    if (Number(draft.namespaceCount) < namespaceKeys(draft.namespaceKeys).length) throw new Error("Namespace quota must cover every requested namespace key.");
    quotaEntries(draft.quotas);
    identityReferences(draft.identityRefs);
    validateAccessGroups(accessGroups(draft.adminGroups), accessGroups(draft.viewerGroups));
  }
}
