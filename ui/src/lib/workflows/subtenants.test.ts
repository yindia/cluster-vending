import assert from "node:assert/strict";
import test from "node:test";
import { create, fromJsonString, toJson } from "@bufbuild/protobuf";
import { SubTenantSchema, SubTenantAccessSchema, SubTenantSpecSchema, SubTenantState } from "@/gen/cloud/v1/subtenant_pb";
import { CreateSubTenantRequestSchema, GetSubTenantResponseSchema, UpdateSubTenantRequestSchema, UpdateSubTenantResponseSchema } from "@/gen/cloud/v1/subtenant_service_pb";
import type { BrowserClients } from "@/lib/api/client";
import { emptySubTenantDraft } from "./subtenant-drafts";
import { applySubTenantReview, deleteSubTenantRequest, disableSubTenantProtection, draftSubTenantSpec, reviewSubTenant, scopedSubTenant, subTenantDraft, subTenantYaml, yamlSubTenantSpec } from "./subtenants";
const scope = { tenantId: "acme", clusterId: "00000000-0000-4000-8000-000000000003" };
const id = "00000000-0000-4000-8000-000000000004";
const requestId = "00000000-0000-4000-8000-000000000005";
const draft = { ...emptySubTenantDraft, name: "payments", displayName: "Payments", namespaceKeys: "payments", quotas: "pods=10\nrequests.cpu=2" };
const spec = () => draftSubTenantSpec(draft);
const resource = () => create(SubTenantSchema, { ...scope, id, etag: "etag-1", generation: 1n, spec: spec() });

test("create review requires UUID and keeps exact scope and request across retries", async () => {
  assert.throws(() => reviewSubTenant(scope, spec(), ""));
  const review = reviewSubTenant(scope, spec(), requestId);
  const request = fromJsonString(CreateSubTenantRequestSchema, review.requestJson);
  assert.equal(request.requestId, requestId);
  assert.equal(request.tenantId, scope.tenantId);
  const requests: unknown[] = [];
  const api = { subtenant: { createSubTenant: async (value: unknown) => { requests.push(value); if (requests.length === 1) throw new Error("retry"); return { subtenant: resource() }; } } } as unknown as BrowserClients;
  await assert.rejects(applySubTenantReview(api, review, scope, new AbortController().signal), /retry/);
  await applySubTenantReview(api, review, scope, new AbortController().signal);
  assert.deepEqual(requests[0], requests[1]);
  await assert.rejects(applySubTenantReview(api, review, { ...scope, tenantId: "foreign" }, new AbortController().signal), /scope/);
  await assert.rejects(applySubTenantReview(api, { ...review, requestJson: review.requestJson.replace('"acme"', '"foreign"') }, scope, new AbortController().signal), /active tenant/);
});

test("YAML round trips generated specs and rejects foreign, output, unknown and unsafe inputs", () => {
  assert.deepEqual(yamlSubTenantSpec(subTenantYaml(scope, resource()), scope, resource()), spec());
  const valid = subTenantYaml(scope, create(SubTenantSchema, { spec: spec() }));
  assert.deepEqual(yamlSubTenantSpec(valid, scope), spec());
  for (const text of [valid.replace("tenant: acme", "tenant: foreign"), valid.replace("kind: SubTenant", "kind: Cluster"), valid + "status: {}\n", valid.replace("displayName:", "unknown:"), valid.replace("namespaceCount: 1", "namespaceCount: 0"), valid.replace("pods:", "unknown.resource:")]) assert.throws(() => yamlSubTenantSpec(text, scope));
  assert.throws(() => yamlSubTenantSpec("x".repeat(70_000), scope));
  assert.throws(() => yamlSubTenantSpec(subTenantYaml(scope, resource()), scope), /identity/);
});

test("updates require etags, retain namespace keys and mask only changed settings including cleared access", () => {
  const before = resource();
  assert.throws(() => reviewSubTenant(scope, spec(), requestId, before), /at least one/);
  const changed = draftSubTenantSpec({ ...draft, displayName: "Renamed" }, before);
  const request = fromJsonString(UpdateSubTenantRequestSchema, reviewSubTenant(scope, changed, requestId, before).requestJson);
  assert.deepEqual(request.updateMask?.paths, ["spec.display_name"]);
  assert.equal(request.subtenant?.etag, before.etag);
  assert.throws(() => draftSubTenantSpec({ ...draft, namespaceKeys: "renamed" }, before), /retained/);
  assert.throws(() => reviewSubTenant(scope, changed, requestId, { ...before, etag: "" }), /etag/);
  before.spec!.access = create(SubTenantAccessSchema, { bindings: [{ identityRef: "team", roleProfile: "workload-editor" }] });
  const clear = fromJsonString(UpdateSubTenantRequestSchema, reviewSubTenant(scope, spec(), requestId, before).requestJson);
  assert.deepEqual(clear.updateMask?.paths, ["spec.access"]);
  assert.deepEqual(clear.subtenant?.spec?.access?.bindings, []);
});

test("delete has mandatory etag, exact confirmation and no force; canonical results fail closed", async () => {
  const before = resource(); before.spec!.deletionProtection = false;
  const request = deleteSubTenantRequest(scope, before, id);
  assert.equal(request.etag, before.etag);
  assert.equal("force" in request, false);
  assert.throws(() => deleteSubTenantRequest(scope, before, "other"), /exact/);
  assert.throws(() => deleteSubTenantRequest(scope, { ...before, etag: "" }, id), /etag/);
  for (const value of [undefined, { ...before, tenantId: "foreign" }, { ...before, clusterId: id }, { ...before, id: "other" }]) assert.throws(() => scopedSubTenant(value, scope, id));
  const api = { subtenant: { createSubTenant: async () => ({ subtenant: { ...before, tenantId: "foreign" } }) } } as unknown as BrowserClients;
  await assert.rejects(applySubTenantReview(api, reviewSubTenant(scope, spec(), requestId), scope, new AbortController().signal), /active tenant/);
  await assert.rejects(applySubTenantReview(api, reviewSubTenant(scope, spec(), requestId), scope, AbortSignal.abort()), /abort/i);
});


test("names are required on creation, immutable on updates, and legacy names stay blank", () => {
  assert.throws(() => draftSubTenantSpec({ ...draft, name: "" }), /name/i);
  for (const name of ["Upper", "a.b", "-start", "end-", "a".repeat(36)]) assert.throws(() => draftSubTenantSpec({ ...draft, name }), /name/i);
  assert.doesNotThrow(() => draftSubTenantSpec({ ...draft, name: "a".repeat(35) }));
  const before = resource();
  assert.throws(() => reviewSubTenant(scope, create(SubTenantSpecSchema, { ...spec(), name: "renamed" }), requestId, before), /immutable/);
  assert.throws(() => yamlSubTenantSpec(subTenantYaml(scope, { ...before, spec: create(SubTenantSpecSchema, { ...spec(), name: "renamed" }) }), scope, before), /immutable/);
  before.spec!.name = "";
  assert.equal(draftSubTenantSpec({ ...draft, name: "", displayName: "Legacy team" }, before).name, "");
});

test("omitted protection is on, false is explicit in masks and required before deletion", () => {
  const before = resource(); before.spec!.deletionProtection = undefined;
  assert.throws(() => deleteSubTenantRequest(scope, before, id), /Disable deletion protection/);
  const changed = draftSubTenantSpec({ ...draft, deletionProtection: false }, before);
  const patch = fromJsonString(UpdateSubTenantRequestSchema, reviewSubTenant(scope, changed, requestId, before).requestJson);
  assert.deepEqual(patch.updateMask?.paths, ["spec.deletion_protection"]);
  assert.equal(patch.subtenant?.spec?.deletionProtection, false);
  before.spec!.deletionProtection = false;
  const protectedAgain = fromJsonString(UpdateSubTenantRequestSchema, reviewSubTenant(scope, create(SubTenantSpecSchema, { ...spec(), deletionProtection: undefined }), requestId, before).requestJson);
  assert.equal(protectedAgain.subtenant?.spec?.deletionProtection, true);
  assert.doesNotThrow(() => deleteSubTenantRequest(scope, before, id));
});

test("protection-only recovery reads latest etag, preserves deleting state and rejects unconfirmed or foreign replies", async () => {
  const before = resource(); before.etag = "latest-etag"; before.status = { $typeName: "cloud.v1.SubTenantStatus", observedGeneration: 1n, state: SubTenantState.DELETING, message: "", conditions: [], remoteCapsuleTenant: "payments", remoteCapsuleTenantUid: "", templateRevision: "", namespaces: [], deletionBlockers: [] };
  const requests: Parameters<BrowserClients["subtenant"]["updateSubTenant"]>[0][] = [];
  const originalSpec = create(SubTenantSpecSchema, before.spec);
  const subtenant: Pick<BrowserClients["subtenant"], "getSubTenant" | "updateSubTenant"> = {
    getSubTenant: async () => create(GetSubTenantResponseSchema, { subtenant: before }),
    updateSubTenant: async (request) => {
      requests.push(request);
      return create(UpdateSubTenantResponseSchema, { subtenant: create(SubTenantSchema, { ...before, spec: create(SubTenantSpecSchema, { ...originalSpec, deletionProtection: false }) }) });
    },
  };
  const api = { subtenant } as unknown as BrowserClients;
  const result = await disableSubTenantProtection(api, scope, id, new AbortController().signal);
  assert.equal(result.status?.state, SubTenantState.DELETING);
  assert.deepEqual(requests[0].updateMask?.paths, ["spec.deletion_protection"]);
  assert.equal(requests[0].subtenant?.etag, "latest-etag");
  assert.deepEqual(toJson(SubTenantSpecSchema, create(SubTenantSpecSchema, requests[0].subtenant?.spec)), { deletionProtection: false });
  assert.equal(requests[0].subtenant?.spec?.deletionProtection, false);
  subtenant.updateSubTenant = async () => create(UpdateSubTenantResponseSchema, { subtenant: before });
  await assert.rejects(disableSubTenantProtection(api, scope, id, new AbortController().signal), /did not confirm/);
  subtenant.getSubTenant = async () => create(GetSubTenantResponseSchema, { subtenant: create(SubTenantSchema, { ...before, tenantId: "foreign" }) });
  await assert.rejects(disableSubTenantProtection(api, scope, id, new AbortController().signal), /active tenant/);
});


test("guided and YAML group arrays round trip and access-only updates explicitly clear groups", () => {
  assert.match(subTenantYaml(scope), /adminGroups: \[\]/);
  assert.match(subTenantYaml(scope), /viewerGroups: \[\]/);
  const withGroups = draftSubTenantSpec({ ...draft, adminGroups: "payments-admins,engineering:payments", viewerGroups: "payments-viewers" });
  assert.deepEqual(withGroups.access?.adminGroups, ["payments-admins", "engineering:payments"]);
  const current = resource(); current.spec = withGroups;
  assert.equal(subTenantDraft(current).viewerGroups, "payments-viewers");
  assert.deepEqual(yamlSubTenantSpec(subTenantYaml(scope, current), scope, current), withGroups);
  const empty = draftSubTenantSpec({ ...draft, adminGroups: "", viewerGroups: "" }, current);
  const patch = fromJsonString(UpdateSubTenantRequestSchema, reviewSubTenant(scope, empty, requestId, current).requestJson);
  assert.deepEqual(patch.updateMask?.paths, ["spec.access"]);
  assert.deepEqual(patch.subtenant?.spec?.access?.adminGroups, []);
  assert.deepEqual(patch.subtenant?.spec?.access?.viewerGroups, []);
  assert.throws(() => draftSubTenantSpec({ ...draft, adminGroups: "shared", viewerGroups: "shared" }), /both/);
});
