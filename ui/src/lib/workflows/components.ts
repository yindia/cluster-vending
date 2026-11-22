import { clone, create, fromJson, fromJsonString, toJsonString, type JsonObject } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { isMap, isPair, isScalar, isSeq, parseDocument, type Node, type Pair } from "yaml";
import { ComponentManagement, ComponentPhase, ComponentSchema, type Component, type ComponentDefinition } from "../../gen/cloud/v1/component_pb";
import {
  DisableComponentRequestSchema, EnableComponentRequestSchema, UpdateComponentRequestSchema,
} from "../../gen/cloud/v1/component_service_pb";
import type { BrowserClients } from "../api/client";

export const COMPONENT_VALUES_LIMIT = 64 * 1024;
const VALUES_NODE_LIMIT = 10_000;
const VALUES_DEPTH_LIMIT = 64;
const forbiddenKeys = new Set(["__proto__", "prototype", "constructor", "<<"]);
const coreTags = new Set(["map", "seq", "str", "null", "bool", "int", "float"].map(tag => `tag:yaml.org,2002:${tag}`));
export interface ComponentMutationContext {
  tenantId: string;
  clusterId: string;
  definition: ComponentDefinition;
  current?: Component;
}
export type ComponentAction = "enable" | "update" | "disable";
/** Immutable JSON snapshots keep server preview normalization out of the apply payload. */
export interface ComponentReview {
  readonly action: ComponentAction;
  readonly requestJson: string;
  readonly preview: Component;
}

export function parseComponentValues(text: string): Component["values"] {
  const bytes = new TextEncoder();
  if (bytes.encode(text).byteLength > COMPONENT_VALUES_LIMIT) throw new Error("Values must fit within 64 KiB of UTF-8 YAML or JSON.");
  let parsed: unknown;
  try {
    const document = parseDocument(text, {
      schema: "core", version: "1.2", customTags: [], resolveKnownTags: false,
      uniqueKeys: true, merge: false, strict: true,
    });
    if (document.errors.length || document.warnings.length) throw new Error(document.errors[0]?.message ?? document.warnings[0].message);
    // Inspect the syntax tree before resolving aliases or converting mapping keys.
    const pending: { node: Node | Pair | null; depth: number }[] = [{ node: document.contents, depth: 0 }];
    let nodes = 0;
    while (pending.length) {
      const { node, depth } = pending.pop()!;
      if (++nodes > VALUES_NODE_LIMIT || depth > VALUES_DEPTH_LIMIT) throw new Error("Values exceed the node or nesting limit.");
      if (!node) continue;
      if ("tag" in node && node.tag && !coreTags.has(node.tag)) throw new Error("Custom YAML tags are not supported.");
      if (isMap(node)) {
        for (const pair of node.items) {
          if (!isScalar(pair.key) || typeof pair.key.value !== "string") throw new Error("Values require string mapping keys.");
          if (forbiddenKeys.has(pair.key.value)) throw new Error(`Unsupported mapping key: ${pair.key.value}.`);
          pending.push({ node: pair, depth: depth + 1 });
        }
      } else if (isSeq(node)) {
        for (const item of node.items) pending.push({ node: item as Node | null, depth: depth + 1 });
      } else if (isPair(node)) {
        pending.push({ node: node.key as Node | null, depth }, { node: node.value as Node | null, depth });
      }
    }
    parsed = document.toJS({ maxAliasCount: 100 });
  } catch (error) {
    throw new Error(`Enter valid YAML or JSON values: ${error instanceof Error ? error.message : "Parsing failed."}`);
  }
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new Error("Values must be a YAML or JSON object.");
  // Count expanded nodes and serialized bytes before JSON.stringify or the Struct
  // codec can expand shared aliases. Track ancestors so recursive aliases fail.
  const ancestors = new Set<object>();
  const pending: { value: unknown; depth: number; exit?: boolean }[] = [{ value: parsed, depth: 0 }];
  let nodes = 0, serializedBytes = 0;
  while (pending.length) {
    const { value, depth, exit } = pending.pop()!;
    if (exit) { ancestors.delete(value as object); continue; }
    if (++nodes > VALUES_NODE_LIMIT || depth > VALUES_DEPTH_LIMIT) throw new Error("Values exceed the expanded node or nesting limit.");
    if (value !== null && typeof value === "object") {
      if (ancestors.has(value)) throw new Error("Recursive YAML aliases are not supported.");
      ancestors.add(value);
      pending.push({ value, depth, exit: true });
      const entries = Object.entries(value);
      serializedBytes += 2 + Math.max(0, entries.length - 1);
      for (const [key, nested] of entries) {
        if (!Array.isArray(value)) {
          if (forbiddenKeys.has(key)) throw new Error(`Unsupported mapping key: ${key}.`);
          serializedBytes += bytes.encode(JSON.stringify(key)).byteLength + 1;
        }
        pending.push({ value: nested, depth: depth + 1 });
      }
    } else {
      if (typeof value === "number" && !Number.isFinite(value)) throw new Error("Values must contain only finite numbers.");
      serializedBytes += bytes.encode(JSON.stringify(value)).byteLength;
    }
    if (serializedBytes > COMPONENT_VALUES_LIMIT) throw new Error("Serialized values must fit within 64 KiB of UTF-8 JSON.");
  }
  if (bytes.encode(JSON.stringify(parsed)).byteLength > COMPONENT_VALUES_LIMIT) throw new Error("Serialized values must fit within 64 KiB of UTF-8 JSON.");
  // The generated Component codec converts the values object using google.protobuf.Struct.
  return fromJson(ComponentSchema, { values: parsed as JsonObject }).values;
}

function requireMutable(context: ComponentMutationContext, action: ComponentAction) {
  const { tenantId, clusterId, definition, current } = context;
  if (!tenantId || !clusterId) throw new Error("Select a tenant and cluster first.");
  if (definition.management !== ComponentManagement.TENANT || current && current.management !== ComponentManagement.TENANT) {
    throw new Error("Platform managed or unknown components are read-only, including entries without IDs.");
  }
  if (current && (current.tenantId !== tenantId || current.clusterId !== clusterId || current.component !== definition.name)) {
    throw new Error("The component does not belong to the selected tenant, cluster and catalog entry.");
  }
  if (action === "enable") {
    if (current?.id) throw new Error("This component is already enabled. Refresh inventory before changing it.");
  } else if (!current?.id || !current.etag) {
    throw new Error("A current component ID and etag are required. Refresh inventory first.");
  }
  if (current?.status?.phase === ComponentPhase.DELETING) throw new Error("This component is being removed. Refresh inventory before another action.");
  if (action !== "disable" && definition.disabled) throw new Error(definition.disabledReason || "This catalog component is unavailable.");
  if (action === "update" && !definition.configurable) throw new Error("This component does not expose configurable tenant values.");
}

export async function loadClusterComponents(api: BrowserClients, tenantId: string, clusterId: string, signal: AbortSignal) {
  const components: Component[] = [];
  const seen = new Set<string>();
  let pageToken = "";
  do {
    signal.throwIfAborted();
    if (seen.has(pageToken)) throw new Error("The component inventory returned a repeated page token. Refresh before editing.");
    seen.add(pageToken);
    const page = await api.component.listComponents({ tenantId, clusterId, pageSize: 200, pageToken }, { signal });
    signal.throwIfAborted();
    components.push(...page.components);
    pageToken = page.nextPageToken;
  } while (pageToken);
  return components;
}

export async function reviewEnableComponent(api: BrowserClients, context: ComponentMutationContext, valuesJson: string, requestId: string, signal: AbortSignal): Promise<ComponentReview> {
  requireMutable(context, "enable");
  if (!/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(requestId)) throw new Error("A stable UUID request ID is required.");
  const request = create(EnableComponentRequestSchema, {
    tenantId: context.tenantId, requestId, validateOnly: true,
    component: { clusterId: context.clusterId, component: context.definition.name, values: parseComponentValues(valuesJson) },
  });
  const requestJson = toJsonString(EnableComponentRequestSchema, request);
  signal.throwIfAborted();
  const response = await api.component.enableComponent(request, { signal });
  signal.throwIfAborted();
  if (!response.component) throw new Error("Validation returned no component preview; nothing can be applied.");
  return Object.freeze({ action: "enable", requestJson, preview: clone(ComponentSchema, response.component) });
}

export async function reviewUpdateComponent(api: BrowserClients, context: ComponentMutationContext, valuesJson: string, signal: AbortSignal): Promise<ComponentReview> {
  requireMutable(context, "update");
  const request = create(UpdateComponentRequestSchema, {
    tenantId: context.tenantId, validateOnly: true, updateMask: { paths: ["values"] },
    component: { id: context.current!.id, etag: context.current!.etag, values: parseComponentValues(valuesJson) },
  });
  const requestJson = toJsonString(UpdateComponentRequestSchema, request);
  signal.throwIfAborted();
  const response = await api.component.updateComponent(request, { signal });
  signal.throwIfAborted();
  if (!response.component) throw new Error("Validation returned no component preview; nothing can be applied.");
  return Object.freeze({ action: "update", requestJson, preview: clone(ComponentSchema, response.component) });
}

export function reviewDisableComponent(context: ComponentMutationContext): ComponentReview {
  requireMutable(context, "disable");
  const request = create(DisableComponentRequestSchema, { tenantId: context.tenantId, componentId: context.current!.id, etag: context.current!.etag, force: false });
  return Object.freeze({ action: "disable", requestJson: toJsonString(DisableComponentRequestSchema, request), preview: clone(ComponentSchema, context.current!) });
}

export function componentDisablePhrase(context: ComponentMutationContext) {
  return `disable ${context.tenantId}/${context.clusterId}/${context.definition.name}/${context.current?.id ?? ""}`;
}

export async function applyComponentReview(api: BrowserClients, context: ComponentMutationContext, review: ComponentReview, confirmation: string, signal: AbortSignal): Promise<Component> {
  requireMutable(context, review.action);
  signal.throwIfAborted();
  let component: Component | undefined;
  if (review.action === "enable") {
    const request = fromJsonString(EnableComponentRequestSchema, review.requestJson);
    if (!request.validateOnly || !request.requestId || request.tenantId !== context.tenantId || request.component?.clusterId !== context.clusterId || request.component.component !== context.definition.name || request.component.version) {
      throw new Error("The reviewed enable request does not match this scope. Validate again.");
    }
    request.validateOnly = false;
    component = (await api.component.enableComponent(request, { signal })).component;
  } else if (review.action === "update") {
    const request = fromJsonString(UpdateComponentRequestSchema, review.requestJson);
    if (!request.validateOnly || request.tenantId !== context.tenantId || request.component?.id !== context.current?.id || request.component?.etag !== context.current?.etag || request.component?.version || request.updateMask?.paths.length !== 1 || request.updateMask.paths[0] !== "values") {
      throw new Error("The reviewed update is stale or does not match this scope. Refresh and validate again.");
    }
    request.validateOnly = false;
    component = (await api.component.updateComponent(request, { signal })).component;
  } else {
    const request = fromJsonString(DisableComponentRequestSchema, review.requestJson);
    if (request.tenantId !== context.tenantId || request.componentId !== context.current?.id || request.etag !== context.current?.etag || request.force || confirmation !== componentDisablePhrase(context)) {
      throw new Error("Type the exact disable confirmation for the reviewed component and current etag.");
    }
    component = (await api.component.disableComponent(request, { signal })).component;
  }
  signal.throwIfAborted();
  if (!component?.id || component.tenantId !== context.tenantId || component.clusterId !== context.clusterId || component.component !== context.definition.name || review.action !== "enable" && component.id !== context.current?.id) {
    throw new Error("The response did not identify the accepted component in this scope. The outcome may be unknown; refresh inventory before retrying.");
  }
  return component;
}

export function componentReady(component: Component) {
  return !!component.id && component.generation > BigInt(0) && component.status?.phase === ComponentPhase.READY && component.status.observedGeneration >= component.generation;
}

export async function observeComponent(api: BrowserClients, tenantId: string, componentId: string, action: ComponentAction, signal: AbortSignal) {
  try {
    const response = await api.component.getComponent({ tenantId, componentId }, { signal });
    signal.throwIfAborted();
    if (!response.component || response.component.id !== componentId) throw new Error("No matching component was returned; convergence cannot be confirmed.");
    return { component: response.component, removed: false };
  } catch (error) {
    signal.throwIfAborted();
    if (action === "disable" && error instanceof ConnectError && error.code === Code.NotFound) return { component: undefined, removed: true };
    throw error;
  }
}
