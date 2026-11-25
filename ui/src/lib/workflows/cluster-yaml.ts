import { fromJson, toJson, type JsonObject, type JsonValue } from "@bufbuild/protobuf";
import type { Path } from "@bufbuild/protobuf/reflect";
import { createValidator } from "@bufbuild/protovalidate";
import { isMap, isPair, isScalar, isSeq, LineCounter, parseDocument, stringify, type Node } from "yaml";
import { ClusterSchema, type Cluster } from "../../gen/cloud/v1/cluster_pb";
import { parseComponentValues } from "./components";
import { clusterDocument, clusterForm, type ClusterForm, type ClusterPlacement } from "./clusters";

const API_VERSION = "cluster-vending.io/v1";
const KIND = "Cluster";
const validator = createValidator();

/** The cluster as the CLI manifest envelope (apiVersion, kind, metadata, spec), derived from the form. */
export function clusterFormYaml(form: ClusterForm, placement: ClusterPlacement): string {
  // Canonical proto JSON: enum names, and defaults the server applies left out.
  const document = toJson(ClusterSchema, fromJson(ClusterSchema, clusterDocument(form, placement))) as JsonObject;
  const metadata: JsonObject = { name: form.name };
  if (form.displayName) metadata.displayName = form.displayName;
  const labels = document.labels as JsonObject | undefined;
  if (labels && Object.keys(labels).length) metadata.labels = labels;
  return stringify({ apiVersion: API_VERSION, kind: KIND, metadata, spec: document.spec });
}

export interface ClusterYamlContext {
  tenantId: string;
  /** Editing: the stored cluster. Its name, id and placement cannot change. */
  baseline?: Cluster;
  /** Creating: the region of a network by ID, when known. */
  regionOf?: (networkId: string) => string | undefined;
}

/** A YAML problem with the 1-based line it points at, when one could be found. */
export class ClusterYamlError extends Error {
  constructor(message: string, readonly line?: number) { super(line ? `Line ${line}: ${message}` : message); }
}

type Segment = string | number;
const camel = (name: string) => name.replace(/_([a-z])/g, (_, char: string) => char.toUpperCase());

function lineOf(text: string, path: readonly Segment[]): number | undefined {
  const counter = new LineCounter();
  const document = parseDocument(text, { lineCounter: counter, uniqueKeys: false, prettyErrors: false });
  for (let depth = path.length; depth > 0; depth--) {
    const node = document.getIn(path.slice(0, depth), true) as Node | undefined;
    if (node && typeof node === "object" && "range" in node && node.range) return counter.linePos(node.range[0]).line;
  }
  return undefined;
}

function lineOfValue(text: string, value: string): number | undefined {
  const counter = new LineCounter();
  const document = parseDocument(text, { lineCounter: counter, uniqueKeys: false, prettyErrors: false });
  const pending: unknown[] = [document.contents];
  while (pending.length) {
    const node = pending.shift();
    if (isScalar(node) && String(node.value) === value && node.range) return counter.linePos(node.range[0]).line;
    if (isMap(node)) for (const pair of node.items) pending.push(pair.value);
    else if (isSeq(node)) pending.push(...node.items);
  }
  return undefined;
}

function lineOfKey(text: string, key: string): number | undefined {
  const counter = new LineCounter();
  const document = parseDocument(text, { lineCounter: counter, uniqueKeys: false, prettyErrors: false });
  const pending: unknown[] = [document.contents];
  while (pending.length) {
    const node = pending.shift();
    if (isMap(node)) for (const pair of node.items) {
      if (isScalar(pair.key) && pair.key.value === key && pair.key.range) return counter.linePos(pair.key.range[0]).line;
      pending.push(pair.value);
    } else if (isSeq(node)) pending.push(...node.items);
    else if (isPair(node)) pending.push(node.value);
  }
  return undefined;
}

/** Maps a protovalidate field path on Cluster to the manifest path. */
function manifestPath(path: Path): Segment[] {
  const segments: Segment[] = [];
  for (const item of path) {
    if (item.kind === "list_sub") segments.push(item.index);
    else if (item.kind === "map_sub") segments.push(String(item.key));
    else if (item.kind === "field") segments.push(item.jsonName);
  }
  const [head, ...rest] = segments;
  if (head === "spec") return ["spec", ...rest];
  if (head === "name" || head === "displayName" || head === "labels") return ["metadata", head, ...rest];
  return segments;
}

function object(value: JsonValue | undefined, label: string, text: string, path: Segment[]): JsonObject {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new ClusterYamlError(`${label} must be a mapping.`, lineOf(text, path));
  return value;
}
function onlyKeys(value: JsonObject, allowed: readonly string[], label: string, text: string) {
  const extra = Object.keys(value).find((key) => !allowed.includes(key));
  if (extra) throw new ClusterYamlError(`Unknown ${label} field "${extra}".`, lineOfKey(text, extra));
}

/** Node groups are the platform's: the system node group and the Karpenter platform pool come from platform configuration. */
const platformNodeKeys = ["nodeGroups", "node_groups", "defaultNodePool", "default_node_pool"] as const;
function rejectNodeGroups(spec: JsonObject, text: string) {
  const aws = spec.aws && typeof spec.aws === "object" && !Array.isArray(spec.aws) ? spec.aws : {};
  for (const [owner, path] of [[spec, "spec"], [aws, "spec.aws"]] as const) {
    const key = platformNodeKeys.find((name) => Object.hasOwn(owner, name));
    if (key) throw new ClusterYamlError(`Node groups are managed by the platform. Remove ${path}.${key}.`, lineOfKey(text, key));
  }
}

/** What a create draft may still leave unfinished (name, network, public CIDRs): the wizard asks for them before review. */
interface Pending { paths: readonly string[]; rules: readonly string[] }

function decode(text: string, json: JsonObject, pending: Pending = { paths: [], rules: [] }): Cluster {
  let cluster: Cluster;
  try { cluster = fromJson(ClusterSchema, json); }
  catch (cause) {
    const message = cause instanceof Error ? cause.message : String(cause);
    const badEnum = /enum ([\w.]+) from JSON: "([^"]*)"/.exec(message);
    if (badEnum) throw new ClusterYamlError(`"${badEnum[2]}" is not a ${badEnum[1]} value.`, lineOfValue(text, badEnum[2]));
    const key = /key "([^"]+)" is unknown/.exec(message)?.[1] ?? /field [\w.]+\.(\w+) from JSON/.exec(message)?.[1];
    throw new ClusterYamlError(message.replace(/^cannot decode /, "Cannot read "), key ? lineOfKey(text, camel(key)) ?? lineOfKey(text, key) : undefined);
  }
  const result = validator.validate(ClusterSchema, cluster);
  if (result.kind === "invalid") {
    // The deepest path is the most precise line: a field rule rather than a whole-spec rule.
    const [first] = result.error.violations
      .filter((violation) => !pending.paths.includes(manifestPath(violation.field).join(".")) && !pending.rules.includes(violation.ruleId))
      .sort((a, b) => b.field.length - a.field.length);
    if (!first) return cluster;
    throw new ClusterYamlError(first ? first.toString() : result.error.message, first ? lineOf(text, manifestPath(first.field)) : undefined);
  }
  if (result.kind === "error") throw new ClusterYamlError(result.error.message);
  return cluster;
}

/**
 * Parses an edited manifest back into form inputs. The text is size, depth and alias bounded,
 * decoded with the generated Cluster schema (unknown fields fail) and checked with protovalidate.
 * Errors carry the line they point at.
 */
export function parseClusterFormYaml(text: string, context: ClusterYamlContext): ClusterForm {
  let root: JsonValue;
  try { root = parseComponentValues(text) as JsonValue; }
  catch (cause) {
    // The parser's message ends with a code frame; the line number is reported separately.
    const message = (cause instanceof Error ? cause.message : String(cause)).replace(/^Enter valid YAML or JSON values: /, "").replace(/^Values/, "The manifest").split("\n")[0].replace(/:\s*$/, ".");
    const line = /at line (\d+)/.exec(message)?.[1];
    throw new ClusterYamlError(`Invalid YAML: ${message.replace(/ at line \d+, column \d+\.?$/, ".")}`, line ? Number(line) : undefined);
  }
  const manifest = object(root, "The manifest", text, []);
  onlyKeys(manifest, ["apiVersion", "kind", "metadata", "spec"], "manifest", text);
  if (manifest.apiVersion !== undefined && manifest.apiVersion !== API_VERSION) throw new ClusterYamlError(`Use apiVersion: ${API_VERSION}.`, lineOf(text, ["apiVersion"]));
  if (manifest.kind !== undefined && manifest.kind !== KIND) throw new ClusterYamlError(`Use kind: ${KIND}.`, lineOf(text, ["kind"]));
  const metadata = object(manifest.metadata, "metadata", text, ["metadata"]);
  onlyKeys(metadata, ["name", "id", "tenant", "displayName", "labels"], "metadata", text);
  const spec = object(manifest.spec, "spec", text, ["spec"]);
  const { baseline, tenantId } = context;
  if (metadata.tenant !== undefined && metadata.tenant !== tenantId) throw new ClusterYamlError(`metadata.tenant must match the active tenant (${tenantId}).`, lineOf(text, ["metadata", "tenant"]));
  if (metadata.id !== undefined && metadata.id !== "" && metadata.id !== baseline?.id) throw new ClusterYamlError(baseline ? "metadata.id must stay the edited cluster's ID." : "Omit metadata.id when creating a cluster.", lineOf(text, ["metadata", "id"]));
  if (baseline && metadata.name !== baseline.name) throw new ClusterYamlError(`metadata.name cannot change: it names the cluster's resources (${baseline.name}).`, lineOf(text, ["metadata", "name"]));
  if (Object.hasOwn(spec, "tier")) throw new ClusterYamlError("Cluster tier is inherited from its network. Remove spec.tier.", lineOfKey(text, "tier"));
  rejectNodeGroups(spec, text);
  const network = (spec.network ?? {}) as JsonObject;
  const networkId = typeof network.networkId === "string" ? network.networkId : "";
  if (baseline) {
    if (networkId !== baseline.spec?.network?.networkId) throw new ClusterYamlError("spec.network.networkId cannot change: a cluster stays in the network it was created in.", lineOf(text, ["spec", "network", "networkId"]));
    if (spec.region !== baseline.spec?.region) throw new ClusterYamlError(`spec.region cannot change: it is set when the cluster is created (${baseline.spec?.region}).`, lineOf(text, ["spec", "region"]));
  } else {
    const region = networkId ? context.regionOf?.(networkId) : undefined;
    if (networkId && context.regionOf && !region) throw new ClusterYamlError("spec.network.networkId must be an existing network of this tenant.", lineOf(text, ["spec", "network", "networkId"]));
    if (region && spec.region !== undefined && spec.region !== region) throw new ClusterYamlError(`spec.region must be the network's region (${region}).`, lineOf(text, ["spec", "region"]));
    if (region && spec.region === undefined) spec.region = region;
  }
  const json: JsonObject = { name: metadata.name ?? "", spec };
  if (metadata.displayName !== undefined) json.displayName = metadata.displayName;
  if (metadata.labels !== undefined) json.labels = metadata.labels;
  const pending: Pending = baseline ? { paths: [], rules: [] } : {
    paths: [...(json.name ? [] : ["metadata.name"]), ...(networkId ? [] : ["spec.region", "spec.network.networkId"])],
    rules: ["public_access_cidrs_required", ...(networkId ? [] : ["aws.region"])],
  };
  const cluster = decode(text, json, pending);
  return clusterForm(cluster);
}
