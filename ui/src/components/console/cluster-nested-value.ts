import type { DescEnum, JsonObject, JsonValue } from "@bufbuild/protobuf";
import { nestedFormYaml } from "@/lib/workflows/cluster-form-values";

/** Nested settings are proto JSON text in the form; the typed editors read and rewrite them. */
export type Nested<T extends JsonValue> = { value: T; error?: undefined } | { value?: undefined; error: string };

export function readObject(text: string): Nested<JsonObject> {
  try {
    const value = nestedFormYaml(text);
    if (value === null) return { value: {} };
    if (typeof value !== "object" || Array.isArray(value)) return { error: "Expected a mapping of settings." };
    return { value };
  } catch (cause) { return { error: cause instanceof Error ? cause.message : String(cause) }; }
}

export function readList(text: string): Nested<JsonObject[]> {
  try {
    const value = nestedFormYaml(text);
    if (value === null) return { value: [] };
    if (!Array.isArray(value) || value.some((item) => !item || typeof item !== "object" || Array.isArray(item))) return { error: "Expected a list of settings." };
    return { value: value as JsonObject[] };
  } catch (cause) { return { error: cause instanceof Error ? cause.message : String(cause) }; }
}

/** A copy without empty strings, lists or objects, so unset fields stay unset (the server default). */
export function compact(value: JsonObject): JsonObject {
  return Object.fromEntries(Object.entries(value).filter(([, item]) =>
    item !== "" && item !== null && item !== undefined && !(Array.isArray(item) && !item.length) &&
    !(typeof item === "object" && !Array.isArray(item) && !Object.keys(item).length)));
}

export const writeObject = (value: JsonObject) => Object.keys(compact(value)).length ? JSON.stringify(compact(value), null, 2) : "";
export const writeList = (values: readonly JsonObject[]) => values.length ? JSON.stringify(values.map(compact), null, 2) : "";

export const text = (value: JsonValue | undefined) => typeof value === "string" ? value : value === undefined || value === null ? "" : String(value);
export const strings = (value: JsonValue | undefined) => Array.isArray(value) ? value.map(text) : [];

const title = (value: string) => value.toLowerCase().replaceAll("_", " ").replace(/^./, (letter) => letter.toUpperCase());
/** Selector options of an enum by value name; "" is the server default. */
export const enumNameOptions = (schema: DescEnum, defaultLabel = "Server default") =>
  schema.values.map((value) => ({ value: value.number ? value.name : "", label: value.number ? title(value.localName) : defaultLabel }));
/** An enum given as its name or number, as its name ("" when unset). */
export function enumName(schema: DescEnum, value: JsonValue | undefined): string {
  if (value === undefined || value === null || value === "" || value === 0) return "";
  const entry = schema.values.find((item) => item.name === value || item.number === Number(value));
  return entry?.number ? entry.name : String(value);
}
