import { fromJson, type DescMessage, type JsonObject, type JsonValue, type MessageShape } from "@bufbuild/protobuf";
import type { Path } from "@bufbuild/protobuf/reflect";
import { createValidator } from "@bufbuild/protovalidate";
import { isMap, isScalar, isSeq, LineCounter, parseDocument, type Document, type Node } from "yaml";
import { parseComponentValues } from "./components";
import { PLACEHOLDER_ID } from "./manifest-templates";

/** The manifest envelope the CLI reads (pkg/cli/manifest.go). */
export const API_VERSION = "cluster-vending.io/v1";
export type ManifestKind = "CloudAccount" | "Network" | "Cluster" | "NetworkImport" | "ClusterImport";

/** A manifest problem with the 1-based line it points at, when one could be found. */
export class ManifestError extends Error {
  constructor(readonly detail: string, readonly line?: number) {
    super(line ? `Line ${line}: ${detail}` : detail);
    this.name = "ManifestError";
  }
}

type Segment = string | number;
const camel = (name: string) => name.replace(/_([a-z])/g, (_, char: string) => char.toUpperCase());

/** Finds lines in the text a manifest was read from: one parse, kept for every lookup. */
export class ManifestLocator {
  private readonly counter = new LineCounter();
  private readonly document: Document;
  constructor(text: string) {
    this.document = parseDocument(text, { lineCounter: this.counter, uniqueKeys: false, prettyErrors: false });
  }
  private line(node: unknown): number | undefined {
    const range = node && typeof node === "object" && "range" in node ? (node as Node).range : undefined;
    return range ? this.counter.linePos(range[0]).line : undefined;
  }
  /** The line of the deepest existing field on `path` (manifest field names): its key's line, or a list item's. */
  lineOf(path: readonly Segment[]): number | undefined {
    let node: unknown = this.document.contents;
    let line: number | undefined;
    for (const segment of path) {
      if (isMap(node)) {
        const pair = node.items.find((item) => isScalar(item.key) && String(item.key.value) === String(segment));
        if (!pair) break;
        line = this.line(pair.key) ?? line;
        node = pair.value;
      } else if (isSeq(node) && typeof segment === "number" && node.items[segment]) {
        node = node.items[segment];
        line = this.line(node) ?? line;
      } else break;
    }
    return line;
  }
  /** The line of the first mapping key named `key`, anywhere in the document. */
  lineOfKey(key: string): number | undefined {
    const pending: unknown[] = [this.document.contents];
    while (pending.length) {
      const node = pending.shift();
      if (isMap(node)) for (const pair of node.items) {
        if (isScalar(pair.key) && pair.key.value === key) return this.line(pair.key);
        pending.push(pair.value);
      } else if (isSeq(node)) pending.push(...node.items);
    }
    return undefined;
  }
  /** The line of the first scalar whose value reads `value`. */
  lineOfValue(value: string): number | undefined {
    const pending: unknown[] = [this.document.contents];
    while (pending.length) {
      const node = pending.shift();
      if (isScalar(node) && String(node.value) === value) return this.line(node);
      if (isMap(node)) for (const pair of node.items) pending.push(pair.value);
      else if (isSeq(node)) pending.push(...node.items);
    }
    return undefined;
  }
}

export interface Manifest {
  kind: ManifestKind;
  metadata: JsonObject;
  spec: JsonObject;
  at: ManifestLocator;
}

function mapping(value: JsonValue | undefined, label: string, at: ManifestLocator, path: Segment[]): JsonObject {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new ManifestError(`${label} must be a mapping.`, at.lineOf(path));
  return value;
}
function onlyKeys(value: JsonObject, allowed: readonly string[], label: string, at: ManifestLocator) {
  const extra = Object.keys(value).find((key) => !allowed.includes(key));
  if (extra) throw new ManifestError(`Unknown ${label} field "${extra}".`, at.lineOfKey(extra));
}

/**
 * Reads one manifest of `kind`: bounded YAML (64 KiB, no custom tags, duplicate keys or
 * recursive aliases), the envelope and metadata the CLI accepts, and the active tenant.
 * status, when present (a manifest copied from a get), is ignored as the CLI does.
 */
export function readManifest(text: string, kind: ManifestKind, tenantId: string): Manifest {
  if (!tenantId) throw new ManifestError("Select a tenant before continuing.");
  if (!text.trim()) throw new ManifestError(`Enter a ${kind} manifest.`);
  let root: JsonValue;
  try { root = parseComponentValues(text) as JsonValue; }
  catch (cause) {
    // The parser's message ends with a code frame; the line is reported separately.
    const message = (cause instanceof Error ? cause.message : String(cause)).replace(/^Enter valid YAML or JSON values: /, "").replace(/^Values/, "The manifest").split("\n")[0].replace(/:\s*$/, ".");
    const line = /at line (\d+)/.exec(message)?.[1];
    throw new ManifestError(`Invalid YAML: ${message.replace(/ at line \d+, column \d+\.?$/, ".")}`, line ? Number(line) : undefined);
  }
  const at = new ManifestLocator(text);
  const manifest = mapping(root, "The manifest", at, []);
  onlyKeys(manifest, ["apiVersion", "kind", "metadata", "spec", "status"], "manifest", at);
  if (manifest.apiVersion !== API_VERSION) throw new ManifestError(`Use apiVersion: ${API_VERSION}.`, at.lineOf(["apiVersion"]));
  if (manifest.kind !== kind) throw new ManifestError(`Use kind: ${kind}.`, at.lineOf(["kind"]));
  const metadata = mapping(manifest.metadata, "metadata", at, ["metadata"]);
  onlyKeys(metadata, ["name", "id", "tenant", "displayName", "labels"], "metadata", at);
  if (typeof metadata.name !== "string" || !metadata.name.trim()) throw new ManifestError("metadata.name is required.", at.lineOf(["metadata", "name"]) ?? at.lineOf(["metadata"]));
  if (metadata.tenant !== undefined && metadata.tenant !== tenantId) throw new ManifestError(`metadata.tenant must match the active tenant (${tenantId}).`, at.lineOf(["metadata", "tenant"]));
  if (metadata.displayName !== undefined && typeof metadata.displayName !== "string") throw new ManifestError("metadata.displayName must be a string.", at.lineOf(["metadata", "displayName"]));
  if (metadata.id !== undefined && typeof metadata.id !== "string") throw new ManifestError("metadata.id must be a string.", at.lineOf(["metadata", "id"]));
  const placeholder = at.lineOfValue(PLACEHOLDER_ID);
  if (placeholder) throw new ManifestError("Replace the placeholder ID with a real one: References lists this tenant's accounts and networks.", placeholder);
  const spec = manifest.spec === undefined || manifest.spec === null ? {} : mapping(manifest.spec, "spec", at, ["spec"]);
  return { kind, metadata, spec, at };
}

/** metadata.labels, refused for the kinds that take none (accounts, and imports before takeover). */
export function refuseLabels({ metadata, at }: Manifest, why: string) {
  if (metadata.labels !== undefined && Object.keys(mapping(metadata.labels, "metadata.labels", at, ["metadata", "labels"])).length) {
    throw new ManifestError(why, at.lineOf(["metadata", "labels"]));
  }
}

/** The removed fields the CLI names, with what to do instead. */
const removedFields: Record<string, string> = {
  nodeGroups: "Node groups are managed by the platform: every cluster gets its system node group and the Karpenter platform pool from the platform configuration. Remove nodeGroups.",
  defaultNodePool: "Node groups are managed by the platform: every cluster gets its system node group and the Karpenter platform pool from the platform configuration. Remove defaultNodePool.",
  naas: "naas was removed with sub-tenants (the platform no longer installs Capsule). Remove it from the manifest.",
};

/** Where a field of the decoded message sits in the manifest: metadata for identity, spec for the rest. */
export type ManifestPathOf = (segments: Segment[]) => Segment[];
export const resourcePath: ManifestPathOf = ([head, ...rest]) =>
  head === "name" || head === "displayName" || head === "labels" ? ["metadata", head, ...rest] : [head, ...rest];
export const requestPath: ManifestPathOf = ([head, ...rest]) =>
  head === "name" || head === "displayName" ? ["metadata", head, ...rest] : ["spec", head, ...rest];

const validator = createValidator();

function fieldPath(path: Path): Segment[] {
  const segments: Segment[] = [];
  for (const item of path) {
    if (item.kind === "list_sub") segments.push(item.index);
    else if (item.kind === "map_sub") segments.push(String(item.key));
    else if (item.kind === "field") segments.push(item.jsonName);
  }
  return segments;
}

/**
 * Decodes proto JSON with the generated schema (unknown fields fail) and, unless `validate` is
 * false, checks it with protovalidate. Every error names the manifest line it points at when
 * it can be found.
 */
export function decodeChecked<S extends DescMessage>(schema: S, json: JsonObject, at: ManifestLocator, pathOf: ManifestPathOf = resourcePath, validate = true): MessageShape<S> {
  let message: MessageShape<S>;
  try { message = fromJson(schema, json); }
  catch (cause) {
    const text = cause instanceof Error ? cause.message : String(cause);
    const badEnum = /enum ([\w.]+) from JSON: "([^"]*)"/.exec(text);
    if (badEnum) throw new ManifestError(`"${badEnum[2]}" is not a ${badEnum[1]} value.`, at.lineOfValue(badEnum[2]));
    const key = /key "([^"]+)" is unknown/.exec(text)?.[1] ?? /field [\w.]+\.(\w+) from JSON/.exec(text)?.[1];
    const line = key ? at.lineOfKey(camel(key)) ?? at.lineOfKey(key) : undefined;
    const removed = key ? removedFields[camel(key)] : undefined;
    throw new ManifestError(removed ?? text.replace(/^cannot decode /, "Cannot read "), line);
  }
  if (validate) validateAt(schema, message, at, pathOf);
  return message;
}

/** protovalidate, with the first violation on the manifest line of its field. */
export function validateAt<S extends DescMessage>(schema: S, message: MessageShape<S>, at: ManifestLocator, pathOf: ManifestPathOf = resourcePath) {
  const result = validator.validate(schema, message);
  if (result.kind === "error") throw new ManifestError(result.error.message);
  if (result.kind === "invalid") {
    // The deepest path is the most precise line: a field rule rather than a whole-spec rule.
    const [first] = [...result.error.violations].sort((a, b) => b.field.length - a.field.length);
    throw new ManifestError(first ? first.toString() : result.error.message, first ? at.lineOf(pathOf(fieldPath(first.field))) : undefined);
  }
}

/** A request message (create, import, update) checked with protovalidate, after its resource was. */
export function checkedRequest<S extends DescMessage>(schema: S, request: MessageShape<S>): MessageShape<S> {
  const result = validator.validate(schema, request);
  if (result.kind !== "valid") throw new ManifestError(result.error.message);
  return request;
}

/** A manifest field path written with proto (snake_case) or JSON names, as the manifest spells it. */
export function manifestPathOf(path: string): Segment[] {
  const segments = path.replace(/\[(\d+)\]/g, ".$1").split(".").filter(Boolean).map((part) => /^\d+$/.test(part) ? Number(part) : camel(part));
  return segments[0] === "spec" || segments[0] === "metadata" ? segments : resourcePath(segments);
}

/**
 * The manifest line a server error names: the first field path in its message (such as
 * spec.control_plane.kubernetes_version or labels) that the manifest states.
 */
export function serverErrorLine(text: string, message: string): number | undefined {
  const at = new ManifestLocator(text);
  for (const match of message.matchAll(/\b(?:spec|metadata|labels|display_name|displayName)(?:\.[A-Za-z_]\w*|\[\d+\])*/g)) {
    const path = manifestPathOf(match[0]);
    if (path.length < 2 && path[0] === "spec") continue;
    const line = at.lineOf(path);
    if (line) return line;
  }
  return undefined;
}

/** The line an error points at: a manifest error's own, else a field path its message names. */
export function errorLine(error: unknown, text: string): number | undefined {
  if (error instanceof ManifestError) return error.line;
  return error instanceof Error ? serverErrorLine(text, error.message) : undefined;
}
