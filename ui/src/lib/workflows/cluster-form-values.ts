import { fromJson, toJson, type DescMessage, type JsonValue, type MessageShape } from "@bufbuild/protobuf";
import { createValidator } from "@bufbuild/protovalidate";
import { parseComponentValues } from "./components";

const validator = createValidator();
export const clusterList = (text: string) => text.split(/[\s,]+/).filter(Boolean);
export function formInteger(text: string, min: number, max: number, label: string) {
  if (!/^\d+$/.test(text) || !Number.isSafeInteger(Number(text)) || Number(text) < min || Number(text) > max) throw new Error(`${label} must be a whole number from ${min} to ${max}.`);
  return Number(text);
}
export function checkedJson<S extends DescMessage>(schema: S, json: JsonValue): MessageShape<S> {
  const value = fromJson(schema, json);
  const result = validator.validate(schema, value);
  if (result.kind !== "valid") throw new Error(result.error.message);
  return value;
}
export function typedFormJson<S extends DescMessage>(schema: S, json: JsonValue): JsonValue {
  return toJson(schema, checkedJson(schema, json));
}
/** Reuses the bounded parser for arrays as well as objects, without accepting a
 * full resource overlay. The generated nested schema rejects unknown fields. */
export function nestedFormYaml(text: string): JsonValue {
  if (!text.trim()) return null;
  const wrapper = parseComponentValues(`value:\n${text.split("\n").map(line => `  ${line}`).join("\n")}`);
  return wrapper?.value ?? null;
}
export function formJson(text: string, label: string): JsonValue {
  if (new TextEncoder().encode(text).byteLength > 64 * 1024) throw new Error(`${label} must fit within 64 KiB.`);
  try { JSON.parse(text || "null"); return nestedFormYaml(text || "null"); }
  catch { throw new Error(`${label} must be valid JSON.`); }
}
export function stringMap(text: string, max: number, label: string): Record<string, string> {
  const value = formJson(text || "{}", label);
  if (!value || typeof value !== "object" || Array.isArray(value) || Object.entries(value).some(([key, item]) => ["__proto__", "prototype", "constructor"].includes(key) || typeof item !== "string")) throw new Error(`${label} must be a JSON object of string values.`);
  if (Object.keys(value).length > max) throw new Error(`${label} allows at most ${max} entries.`);
  return value as Record<string, string>;
}
export function qualifiedKey(key: string) {
  const parts = key.split("/");
  const name = parts.at(-1)!;
  return parts.length <= 2 && /^[A-Za-z0-9]([A-Za-z0-9_.-]{0,61}[A-Za-z0-9])?$/.test(name) &&
    (parts.length === 1 || (parts[0].length <= 253 && parts[0].split(".").every(part => /^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$/.test(part))));
}
export function labelValue(value: string) { return value === "" || /^[A-Za-z0-9]([A-Za-z0-9_.-]{0,61}[A-Za-z0-9])?$/.test(value); }
export function optionalBoolean(text: string, label: string): boolean | undefined {
  if (text === "" || text === "default") return undefined;
  if (text !== "true" && text !== "false") throw new Error(`${label} must be true, false or default.`);
  return text === "true";
}
