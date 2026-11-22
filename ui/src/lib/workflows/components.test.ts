import assert from "node:assert/strict";
import { test, type TestContext } from "node:test";
import { create, toJson } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { createBrowserClients } from "../api/client";
import {
  ComponentDefinitionSchema, ComponentManagement, ComponentPhase, ComponentSchema,
} from "../../gen/cloud/v1/component_pb";
import {
  DisableComponentResponseSchema, EnableComponentRequestSchema, EnableComponentResponseSchema,
  GetComponentResponseSchema, ListComponentsResponseSchema, UpdateComponentRequestSchema, UpdateComponentResponseSchema,
} from "../../gen/cloud/v1/component_service_pb";
import {
  COMPONENT_VALUES_LIMIT, applyComponentReview, componentDisablePhrase, componentReady,
  loadClusterComponents, observeComponent, parseComponentValues, reviewDisableComponent,
  reviewEnableComponent, reviewUpdateComponent, type ComponentMutationContext,
} from "./components";

const clusterId = "11111111-1111-4111-8111-111111111111";
const componentId = "22222222-2222-4222-8222-222222222222";
const requestId = "33333333-3333-4333-8333-333333333333";
const signal = () => new AbortController().signal;
function context(enabled = false): ComponentMutationContext {
  return {
    tenantId: "acme", clusterId,
    definition: create(ComponentDefinitionSchema, { name: "policies", configurable: true, management: ComponentManagement.TENANT }),
    current: enabled ? create(ComponentSchema, {
      id: componentId, tenantId: "acme", clusterId, component: "policies", etag: "etag-7",
      management: ComponentManagement.TENANT, version: "server-version", generation: BigInt(7), values: { old: true },
    }) : undefined,
  };
}
function component() { return context(true).current!; }
function client(t: TestContext) {
  t.mock.method(globalThis, "fetch", () => { throw new Error("Real network calls are forbidden in component tests"); });
  return createBrowserClients({ baseUrl: "https://offline.invalid" });
}

test("values use the generated Struct codec, accept nested JSON and enforce UTF-8 bytes", () => {
  assert.deepEqual(parseComponentValues('{"nested":{"items":[true,1,null,"日本語"]}}'), { nested: { items: [true, 1, null, "日本語"] } });
  for (const value of ["null", "[]", "1", '"text"', "", "# just a comment"]) assert.throws(() => parseComponentValues(value), /object/);
  const exact = JSON.stringify({ x: "a".repeat(COMPONENT_VALUES_LIMIT - 8) });
  assert.equal(new TextEncoder().encode(exact).byteLength, COMPONENT_VALUES_LIMIT);
  assert.ok(parseComponentValues(exact));
  assert.throws(() => parseComponentValues(exact + " "), /64 KiB/);
  const unicode = JSON.stringify({ x: "界".repeat(22_000) });
  assert.ok(unicode.length < COMPONENT_VALUES_LIMIT);
  assert.throws(() => parseComponentValues(unicode), /64 KiB/);
});

test("Helm YAML preserves scalar types, nested objects, comments, lists and block strings", () => {
  assert.deepEqual(parseComponentValues(`# Helm tenant overrides
replicas: 2
enabled: true
annotations:
  example.org/note: 日本語
  date: 2026-11-16
  legacyBoolean: on
  quoted: "false"
items: [null, 1.5, false]
message: |
  first line
  second line
`), {
    replicas: 2, enabled: true,
    annotations: { "example.org/note": "日本語", date: "2026-11-16", legacyBoolean: "on", quoted: "false" },
    items: [null, 1.5, false], message: "first line\nsecond line\n",
  });
  assert.deepEqual(parseComponentValues("default: &default {replicas: 2}\ncopy: *default"), { default: { replicas: 2 }, copy: { replicas: 2 } });
});

test("malformed YAML, multiple documents, duplicate keys and non-string keys are rejected", () => {
  for (const input of ["replicas: [1,", "first: 1\n---\nsecond: 2", "replicas: 1\nreplicas: 2", '{"nested":{"x":1,"x":2}}', "? [x, y]\n: z", "1: one\n'1': another", "null: value"]) {
    assert.throws(() => parseComponentValues(input), /valid YAML|mapping keys/);
  }
});

test("non-finite numbers are rejected in both YAML and JSON, including nested collections", () => {
  for (const input of ['{"bad":1e999}', "bad: .inf", "bad: -.Inf", "bad: .NaN", "nested: [1, {bad: 1e999}]"]) {
    assert.throws(() => parseComponentValues(input), /finite numbers/);
  }
});

test("custom tags, merge keys and prototype keys cannot reach the Struct codec", () => {
  for (const input of ["x: !custom anything", "x: !!js/function function() {}", "x: !!timestamp 2026-11-16", "x: !!set {a: null}", "base: &base {a: 1}\nx: {<<: *base}"]) {
    assert.throws(() => parseComponentValues(input), /valid YAML|Unsupported mapping key/);
  }
  for (const key of ["__proto__", "constructor", "prototype"]) {
    for (const input of [`nested:\n  ${key}: {polluted: true}`, `{"items":[{"${key}":{"polluted":true}}]}`]) {
      assert.throws(() => parseComponentValues(input), /Unsupported mapping key/);
    }
  }
  assert.equal(({} as { polluted?: boolean }).polluted, undefined);
});

test("alias bombs, recursive aliases, unknown aliases and excessive nodes or depth fail within bounds", { timeout: 5000 }, () => {
  const bomb = ["a: &a [1, 2, 3, 4, 5, 6, 7, 8, 9, 10]", ...["b", "c", "d", "e"].map((key, index) => `${key}: &${key} [${Array(10).fill(`*${["a", "b", "c", "d"][index]}`).join(", ")}]`)].join("\n");
  for (const input of ["x: *missing", "x: &x [*x]", `x: &x value\ny: [${Array(101).fill("*x").join(", ")}]`, bomb]) {
    assert.throws(() => parseComponentValues(input), /alias|Alias|resource exhaustion/);
  }
  assert.throws(() => parseComponentValues(`items: [${"0,".repeat(10_001)}]`), /node or nesting limit/);
  assert.throws(() => parseComponentValues(`items: ${"[".repeat(70)}0${"]".repeat(70)}`), /node or nesting limit/);
});

test("serialized limits apply to escaping and expanded aliases as well as source bytes", () => {
  // Escaped YAML can serialize to more JSON bytes than its original source.
  const expandedEscapes = `text: "${"\\0".repeat(12_000)}"`;
  assert.throws(() => parseComponentValues(expandedEscapes), /64 KiB/);
  const aliases = `original: &large '${"a".repeat(33_000)}'\ncopy: *large`;
  assert.ok(new TextEncoder().encode(aliases).byteLength < COMPONENT_VALUES_LIMIT);
  assert.throws(() => parseComponentValues(aliases), /64 KiB/);
});

test("enable validates first and applies exactly that request, preserving request ID and ignoring preview normalization", async t => {
  const api = client(t), ctx = context();
  const preview = create(ComponentSchema, { values: { normalized: true }, version: "platform-chosen", status: { effectiveValues: { merged: true } } });
  const call = t.mock.method(api.component, "enableComponent", async (request: Parameters<typeof api.component.enableComponent>[0]) => create(EnableComponentResponseSchema, { component: request?.validateOnly ? preview : component() }));
  const review = await reviewEnableComponent(api, ctx, '# tenant values\nreplicas: 2', requestId, signal());
  assert.equal(call.mock.callCount(), 1);
  const validated = create(EnableComponentRequestSchema, call.mock.calls[0].arguments[0]);
  assert.equal(validated.validateOnly, true);
  assert.equal(validated.requestId, requestId);
  assert.equal(validated.component?.version, "");
  preview.values = { mutatedLater: true };
  assert.deepEqual(review.preview.values, { normalized: true });
  assert.equal((await applyComponentReview(api, ctx, review, "", signal())).id, componentId);
  const applied = create(EnableComponentRequestSchema, call.mock.calls[1].arguments[0]);
  assert.deepEqual(toJson(EnableComponentRequestSchema, applied), toJson(EnableComponentRequestSchema, { ...validated, validateOnly: false }));
  assert.deepEqual(applied.component?.values, { replicas: 2 });
  await applyComponentReview(api, ctx, review, "", signal());
  assert.equal(call.mock.calls[2].arguments[0]?.requestId, requestId, "an explicit retry keeps the idempotency key");
});

test("update validates values only with the current etag, then applies the same mask and payload", async t => {
  const api = client(t), ctx = context(true);
  const call = t.mock.method(api.component, "updateComponent", async () => create(UpdateComponentResponseSchema, { component: component() }));
  const review = await reviewUpdateComponent(api, ctx, 'new: true', signal());
  const validated = create(UpdateComponentRequestSchema, call.mock.calls[0].arguments[0]);
  assert.deepEqual(validated.updateMask?.paths, ["values"]);
  assert.equal(validated.component?.etag, "etag-7");
  assert.equal(validated.component?.id, componentId);
  assert.equal(validated.component?.version, "");
  assert.equal(validated.component?.clusterId, "");
  assert.equal(validated.validateOnly, true);
  assert.deepEqual(validated.component?.values, { new: true });
  await applyComponentReview(api, ctx, review, "", signal());
  assert.deepEqual(toJson(UpdateComponentRequestSchema, create(UpdateComponentRequestSchema, call.mock.calls[1].arguments[0])), toJson(UpdateComponentRequestSchema, { ...validated, validateOnly: false }));
  ctx.current!.etag = "new-etag";
  await assert.rejects(applyComponentReview(api, ctx, review, "", signal()), /stale/);
  assert.equal(call.mock.callCount(), 2);
});

test("missing previews, invalid values and absent current etags never yield an applicable review", async t => {
  const api = client(t);
  const call = t.mock.method(api.component, "enableComponent", async () => create(EnableComponentResponseSchema));
  await assert.rejects(reviewEnableComponent(api, context(), "{}", requestId, signal()), /no component preview/);
  await assert.rejects(reviewEnableComponent(api, context(), "[]", requestId, signal()), /object/);
  await assert.rejects(reviewEnableComponent(api, context(), "x: 1\nx: 2", requestId, signal()), /valid YAML/);
  assert.equal(call.mock.callCount(), 1);
  const ctx = context(true); ctx.current!.etag = "";
  await assert.rejects(reviewUpdateComponent(api, ctx, "{}", signal()), /ID and etag/);
  assert.throws(() => reviewDisableComponent(ctx), /ID and etag/);
});

test("platform and unknown management remain read-only, even for rows without IDs", async t => {
  const api = client(t);
  for (const management of [ComponentManagement.PLATFORM, ComponentManagement.UNSPECIFIED]) {
    const ctx = context(); ctx.definition.management = management;
    await assert.rejects(reviewEnableComponent(api, ctx, "{}", requestId, signal()), /read-only/);
    ctx.current = create(ComponentSchema, { management });
    assert.throws(() => reviewDisableComponent(ctx), /read-only/);
    ctx.definition.management = ComponentManagement.TENANT;
    await assert.rejects(reviewEnableComponent(api, ctx, "{}", requestId, signal()), /read-only/);
  }
});

test("disable requires exact scoped confirmation and etag and never requests force", async t => {
  const api = client(t), ctx = context(true), review = reviewDisableComponent(ctx);
  const call = t.mock.method(api.component, "disableComponent", async () => create(DisableComponentResponseSchema, { component: component() }));
  assert.equal(call.mock.callCount(), 0, "reviewing removal has no API side effects");
  await assert.rejects(applyComponentReview(api, ctx, review, ctx.definition.name, signal()), /exact disable confirmation/);
  const phrase = componentDisablePhrase(ctx);
  assert.ok(phrase.includes(ctx.tenantId) && phrase.includes(clusterId) && phrase.includes(componentId));
  await applyComponentReview(api, ctx, review, phrase, signal());
  assert.equal(call.mock.calls[0].arguments[0]?.etag, "etag-7");
  assert.equal(call.mock.calls[0].arguments[0]?.force, false);
  ctx.current!.etag = "changed";
  await assert.rejects(applyComponentReview(api, ctx, review, phrase, signal()), /current etag/);
  assert.equal(call.mock.callCount(), 1);
});

test("Go validation and dependency failures remain authoritative and visible", async t => {
  const api = client(t);
  const failure = new ConnectError("required scanner is not enabled; conflicting addon installed", Code.FailedPrecondition);
  t.mock.method(api.component, "enableComponent", async () => { throw failure; });
  await assert.rejects(reviewEnableComponent(api, context(), "{}", requestId, signal()), actual => actual === failure);
  const ctx = context(true), review = reviewDisableComponent(ctx);
  t.mock.method(api.component, "disableComponent", async () => { throw failure; });
  await assert.rejects(applyComponentReview(api, ctx, review, componentDisablePhrase(ctx), signal()), actual => actual === failure);
});

test("reviews cannot cross tenant, cluster or catalog selections", async t => {
  const api = client(t), ctx = context();
  t.mock.method(api.component, "enableComponent", async () => create(EnableComponentResponseSchema, { component: component() }));
  const review = await reviewEnableComponent(api, ctx, "{}", requestId, signal());
  for (const changed of [{ ...ctx, tenantId: "other" }, { ...ctx, clusterId: componentId }, { ...ctx, definition: { ...ctx.definition, name: "other" } }]) {
    await assert.rejects(applyComponentReview(api, changed, review, "", signal()), /does not match this scope/);
  }
  await assert.rejects(reviewUpdateComponent(api, { ...context(true), clusterId: componentId }, "{}", signal()), /does not belong/);
});

test("editing scope aborts late validation results instead of producing a review", async t => {
  const api = client(t), controller = new AbortController();
  let release!: () => void;
  const delayed = new Promise<void>(resolve => { release = resolve; });
  t.mock.method(api.component, "enableComponent", async () => { await delayed; return create(EnableComponentResponseSchema, { component: component() }); });
  const result = reviewEnableComponent(api, context(), "{}", requestId, controller.signal);
  controller.abort(); release();
  await assert.rejects(result, { name: "AbortError" });
});

test("accepted READY from an older generation is not convergence", () => {
  const value = component();
  value.status = create(ComponentSchema, { status: { phase: ComponentPhase.READY, observedGeneration: BigInt(6) } }).status;
  assert.equal(componentReady(value), false);
  value.status!.observedGeneration = BigInt(7);
  assert.equal(componentReady(value), true);
  value.status!.phase = ComponentPhase.PENDING;
  assert.equal(componentReady(value), false);
  value.status!.phase = ComponentPhase.READY; value.id = "";
  assert.equal(componentReady(value), false, "a validate-only preview without an ID cannot prove readiness");
});

test("disable disappearance needs scoped NOT_FOUND; denied access and missing data never imply removal", async t => {
  const api = client(t);
  const call = t.mock.method(api.component, "getComponent", async () => { throw new ConnectError("gone", Code.NotFound); });
  assert.equal((await observeComponent(api, "acme", componentId, "disable", signal())).removed, true);
  await assert.rejects(observeComponent(api, "acme", componentId, "enable", signal()), /gone/);
  call.mock.mockImplementation(async () => { throw new ConnectError("denied", Code.PermissionDenied); });
  await assert.rejects(observeComponent(api, "acme", componentId, "disable", signal()), /denied/);
  call.mock.mockImplementation(async () => create(GetComponentResponseSchema));
  await assert.rejects(observeComponent(api, "acme", componentId, "disable", signal()), /convergence cannot be confirmed/);
});

test("editor inventory consumes every page so a later enabled row is never mistaken for not enabled", async t => {
  const api = client(t);
  const call = t.mock.method(api.component, "listComponents", async (request: Parameters<typeof api.component.listComponents>[0]) => create(ListComponentsResponseSchema, request?.pageToken ? { components: [component()] } : { nextPageToken: "next" }));
  const inventory = await loadClusterComponents(api, "acme", clusterId, signal());
  assert.equal(inventory[0].id, componentId);
  assert.equal(call.mock.callCount(), 2);
  assert.equal(call.mock.calls[1].arguments[0]?.clusterId, clusterId);
  call.mock.mockImplementation(async () => create(ListComponentsResponseSchema, { nextPageToken: "repeat" }));
  await assert.rejects(loadClusterComponents(api, "acme", clusterId, signal()), /repeated page token/);
});
