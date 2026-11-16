import { create, equals, fromJson, fromJsonString, toJson, toJsonString, type DescMessage, type JsonObject, type MessageShape } from "@bufbuild/protobuf";
import { createValidator } from "@bufbuild/protovalidate";
import { stringify } from "yaml";
import { SubTenantSchema, SubTenantSpecSchema, SubTenantQuotaSchema, SubTenantAccessSchema, type SubTenant, type SubTenantSpec } from "@/gen/cloud/v1/subtenant_pb";
import { CreateSubTenantRequestSchema, UpdateSubTenantRequestSchema, DeleteSubTenantRequestSchema } from "@/gen/cloud/v1/subtenant_service_pb";
import type { BrowserClients } from "@/lib/api/client";
import { parseComponentValues } from "./components";
import { accessGroups, identityReferences, namespaceKeys, quotaEntries, SUBTENANT_POLICY, SUBTENANT_ROLE, validateAccessGroups, validateSubTenantDraft, type SubTenantDraft } from "./subtenant-drafts";

export interface SubTenantScope { tenantId: string; clusterId: string }
export interface SubTenantReview { action: "create" | "update"; scope: SubTenantScope; resource: SubTenant; requestJson: string }
const validator = createValidator();
function checked<S extends DescMessage>(schema: S, value: MessageShape<S>) {
  const result = validator.validate(schema, value);
  if (result.kind !== "valid") throw new Error(result.error.message);
  return value;
}

export function scopedSubTenant(resource: SubTenant | undefined, scope: SubTenantScope, expectedId?: string): SubTenant {
  if (!resource?.id || resource.tenantId !== scope.tenantId || resource.clusterId !== scope.clusterId || (expectedId !== undefined && resource.id !== expectedId)) throw new Error("The API did not return the requested Sub Tenant in the active tenant and cluster. Refresh before continuing.");
  return resource;
}

export function subTenantDraft(resource: SubTenant): SubTenantDraft {
  const spec = resource.spec;
  return { adminGroups: spec?.access?.adminGroups.join("\n") ?? "", viewerGroups: spec?.access?.viewerGroups.join("\n") ?? "", name: spec?.name ?? "", deletionProtection: spec?.deletionProtection !== false, displayName: spec?.displayName ?? "", namespaceKeys: spec?.namespaceKeys.join("\n") ?? "", namespaceCount: String(spec?.quota?.namespaceCount ?? 1), quotas: Object.entries(spec?.quota?.hard ?? {}).map(([key, value]) => `${key}=${value}`).join("\n"), identityRefs: spec?.access?.bindings.map((binding) => binding.identityRef).join("\n") ?? "" };
}

export function draftSubTenantSpec(draft: SubTenantDraft, baseline?: SubTenant): SubTenantSpec {
  validateSubTenantDraft(draft, undefined, baseline?.spec?.namespaceKeys, baseline ? baseline.spec?.name ?? "" : undefined);
  return checked(SubTenantSpecSchema, create(SubTenantSpecSchema, {
    name: draft.name, deletionProtection: baseline && baseline.spec?.deletionProtection === undefined && draft.deletionProtection ? undefined : draft.deletionProtection,
    displayName: draft.displayName, namespaceKeys: namespaceKeys(draft.namespaceKeys), policyProfile: SUBTENANT_POLICY,
    quota: { namespaceCount: Number(draft.namespaceCount), hard: quotaEntries(draft.quotas) },
    access: { adminGroups: accessGroups(draft.adminGroups), viewerGroups: accessGroups(draft.viewerGroups), bindings: identityReferences(draft.identityRefs).map((identityRef) => ({ identityRef, roleProfile: SUBTENANT_ROLE })) },
  }));
}

export function subTenantYaml(scope: SubTenantScope, resource?: SubTenant): string {
  return stringify({ apiVersion: "cluster-vending.io/v1", kind: "SubTenant", metadata: { tenant: scope.tenantId, clusterId: scope.clusterId, ...(resource?.id ? { id: resource.id } : {}) }, spec: resource?.spec ? toJson(SubTenantSpecSchema, resource.spec) : { name: "", deletionProtection: true, displayName: "", access: { adminGroups: [], viewerGroups: [] }, namespaceKeys: [], quota: { namespaceCount: 1, hard: {} }, policyProfile: SUBTENANT_POLICY } });
}

function object(value: unknown, label: string): JsonObject {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error(`${label} must be an object.`);
  return value as JsonObject;
}
function onlyKeys(value: JsonObject, allowed: readonly string[], label: string) {
  for (const key of Object.keys(value)) if (!allowed.includes(key)) throw new Error(`Unknown ${label} field: ${key}.`);
}
export function yamlSubTenantSpec(text: string, scope: SubTenantScope, baseline?: SubTenant): SubTenantSpec {
  const root = object(parseComponentValues(text), "Manifest");
  onlyKeys(root, ["apiVersion", "kind", "metadata", "spec"], "manifest");
  if (root.apiVersion !== "cluster-vending.io/v1" || root.kind !== "SubTenant") throw new Error("Use apiVersion cluster-vending.io/v1 and kind SubTenant.");
  const metadata = object(root.metadata, "Metadata");
  onlyKeys(metadata, ["tenant", "clusterId", "id"], "metadata");
  if ((metadata.tenant !== undefined && metadata.tenant !== scope.tenantId) || (metadata.clusterId !== undefined && metadata.clusterId !== scope.clusterId)) throw new Error("Manifest tenant and cluster must match the active tenant and cluster.");
  if (metadata.id !== undefined && (!baseline || metadata.id !== baseline.id)) throw new Error("Sub Tenant identity is immutable. Omit metadata.id when creating.");
  const spec = checked(SubTenantSpecSchema, fromJson(SubTenantSpecSchema, object(root.spec, "Spec")));
  validateAccessGroups(spec.access?.adminGroups ?? [], spec.access?.viewerGroups ?? []);
  if (baseline && spec.name !== (baseline.spec?.name ?? "")) throw new Error("Sub Tenant name is immutable, including legacy allocations.");
  if (!baseline && !spec.name) throw new Error("Name is required for a new Sub Tenant.");
  if (baseline?.spec?.namespaceKeys.some((key) => !spec.namespaceKeys.includes(key))) throw new Error("Existing namespace keys must be retained. Namespace keys are append-only.");
  return spec;
}

export function reviewSubTenant(scope: SubTenantScope, spec: SubTenantSpec, requestId: string, baseline?: SubTenant): SubTenantReview {
  checked(SubTenantSpecSchema, spec);
  validateAccessGroups(spec.access?.adminGroups ?? [], spec.access?.viewerGroups ?? []);
  if (!baseline) {
    if (!spec.name) throw new Error("Name is required for a new Sub Tenant.");
    const request = checked(CreateSubTenantRequestSchema, create(CreateSubTenantRequestSchema, { ...scope, subtenant: { spec }, requestId }));
    return { action: "create", scope: { ...scope }, resource: create(SubTenantSchema, { ...scope, spec }), requestJson: toJsonString(CreateSubTenantRequestSchema, request) };
  }
  scopedSubTenant(baseline, scope);
  if (spec.name !== (baseline.spec?.name ?? "")) throw new Error("Sub Tenant name is immutable, including legacy allocations.");
  if (!baseline.etag) throw new Error("Reload this Sub Tenant: an etag is required for editing.");
  if (baseline.spec?.namespaceKeys.some((key) => !spec.namespaceKeys.includes(key))) throw new Error("Existing namespace keys must be retained.");
  const before = baseline.spec ?? create(SubTenantSpecSchema);
  const paths: string[] = [];
  if ((before.deletionProtection !== false) !== (spec.deletionProtection !== false)) { paths.push("spec.deletion_protection"); spec = create(SubTenantSpecSchema, { ...spec, deletionProtection: spec.deletionProtection ?? true }); }
  if (before.displayName !== spec.displayName) paths.push("spec.display_name");
  if (JSON.stringify(before.namespaceKeys) !== JSON.stringify(spec.namespaceKeys)) paths.push("spec.namespace_keys");
  if (!equals(SubTenantQuotaSchema, before.quota ?? create(SubTenantQuotaSchema), spec.quota ?? create(SubTenantQuotaSchema))) paths.push("spec.quota");
  if (before.policyProfile !== spec.policyProfile) paths.push("spec.policy_profile");
  if (!equals(SubTenantAccessSchema, before.access ?? create(SubTenantAccessSchema), spec.access ?? create(SubTenantAccessSchema))) paths.push("spec.access");
  if (!paths.length) throw new Error("Change at least one setting before reviewing an update.");
  const request = checked(UpdateSubTenantRequestSchema, create(UpdateSubTenantRequestSchema, { ...scope, subtenant: { id: baseline.id, etag: baseline.etag, spec }, updateMask: { paths } }));
  return { action: "update", scope: { ...scope }, resource: create(SubTenantSchema, { ...scope, id: baseline.id, spec, etag: baseline.etag }), requestJson: toJsonString(UpdateSubTenantRequestSchema, request) };
}

export async function applySubTenantReview(api: BrowserClients, review: SubTenantReview, scope: SubTenantScope, signal: AbortSignal) {
  if (review.scope.tenantId !== scope.tenantId || review.scope.clusterId !== scope.clusterId) throw new Error("The tenant or cluster changed. Review again in the current scope.");
  signal.throwIfAborted();
  const request = review.action === "create" ? fromJsonString(CreateSubTenantRequestSchema, review.requestJson) : fromJsonString(UpdateSubTenantRequestSchema, review.requestJson);
  if (request.tenantId !== scope.tenantId || request.clusterId !== scope.clusterId) throw new Error("The reviewed request no longer matches the active tenant and cluster.");
  const response = review.action === "create"
    ? await api.subtenant.createSubTenant(checked(CreateSubTenantRequestSchema, fromJsonString(CreateSubTenantRequestSchema, review.requestJson)), { signal })
    : await api.subtenant.updateSubTenant(checked(UpdateSubTenantRequestSchema, fromJsonString(UpdateSubTenantRequestSchema, review.requestJson)), { signal });
  signal.throwIfAborted();
  return scopedSubTenant(response.subtenant, scope, review.action === "update" ? review.resource.id : undefined);
}

export function deleteSubTenantRequest(scope: SubTenantScope, resource: SubTenant, confirmation: string) {
  scopedSubTenant(resource, scope);
  if (resource.spec?.deletionProtection !== false) throw new Error("Disable deletion protection before deleting this Sub Tenant.");
  if (confirmation !== resource.id) throw new Error("Type the exact Sub Tenant ID to confirm deletion.");
  if (!resource.etag) throw new Error("Reload this Sub Tenant: an etag is required for deletion.");
  return checked(DeleteSubTenantRequestSchema, create(DeleteSubTenantRequestSchema, { ...scope, subtenantId: resource.id, etag: resource.etag }));
}

/** Protection-only update remains available while ordinary spec edits are blocked by deletion. */
export async function disableSubTenantProtection(api: BrowserClients, scope: SubTenantScope, id: string, signal: AbortSignal) {
  signal.throwIfAborted();
  const current = scopedSubTenant((await api.subtenant.getSubTenant({ ...scope, subtenantId: id }, { signal })).subtenant, scope, id);
  signal.throwIfAborted();
  if (!current.etag) throw new Error("Refresh this Sub Tenant before disabling deletion protection: no etag was returned.");
  const request = checked(UpdateSubTenantRequestSchema, create(UpdateSubTenantRequestSchema, { ...scope, subtenant: { id, etag: current.etag, spec: { deletionProtection: false } }, updateMask: { paths: ["spec.deletion_protection"] } }));
  const updated = scopedSubTenant((await api.subtenant.updateSubTenant(request, { signal })).subtenant, scope, id);
  signal.throwIfAborted();
  if (updated.spec?.deletionProtection !== false) throw new Error("The server did not confirm that deletion protection was disabled. Refresh before continuing.");
  return updated;
}
