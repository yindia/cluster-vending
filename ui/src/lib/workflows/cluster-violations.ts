import type { ClusterPolicySnapshotSummary, ClusterViolation, ClusterViolationEvent, PolicyObjectReference } from "../../gen/cloud/v1/violation_pb";
import type { Timestamp } from "@bufbuild/protobuf/wkt";
import { collectPages } from "../api/pagination";

export type PolicyRow = ClusterViolation | ClusterViolationEvent;
export type PolicyFilter = "namespace" | "kind" | "status" | "severity" | "policy" | "engine";
export type PolicyFilters = Record<PolicyFilter, string> & { search: string };
export const emptyPolicyFilters: PolicyFilters = { search: "", namespace: "all", kind: "all", status: "all", severity: "all", policy: "all", engine: "all" };

export function referenceLabel(ref: PolicyObjectReference | undefined): string {
  if (!ref) return "Not reported";
  return [ref.kind, ref.namespace, ref.name].filter(Boolean).join("/") || "Not reported";
}

export function policyFilterValue(row: PolicyRow, field: PolicyFilter): string {
  switch (field) {
    case "namespace": return row.resource?.namespace ?? "";
    case "kind": return row.resource?.kind ?? "";
    case "status": return "result" in row ? row.result : "";
    case "severity": return "severity" in row ? row.severity : "";
    // Full identity avoids merging same-named policies in different engines/scopes.
    case "policy": return JSON.stringify([row.engine, row.policy?.apiVersion, row.policy?.kind, row.policy?.namespace, row.policy?.name]);
    case "engine": return row.engine;
  }
}

function policyOptionLabel(engine: string, policy: Pick<PolicyObjectReference, "apiVersion" | "kind" | "namespace" | "name"> | undefined): string {
  const version = policy?.apiVersion ? ` (${policy.apiVersion})` : "";
  return `${engine || "Unknown engine"} · ${referenceLabel(policy as PolicyObjectReference | undefined)}${version}`;
}

/** Label for a selected policy value that no loaded row carries, built from its parsed identity. */
export function policyValueLabel(value: string): string {
  try {
    const [engine, apiVersion, kind, namespace, name] = JSON.parse(value.slice("value:".length)) as (string | null)[];
    return policyOptionLabel(engine ?? "", { apiVersion: apiVersion ?? "", kind: kind ?? "", namespace: namespace ?? "", name: name ?? "" } as PolicyObjectReference);
  } catch { return value.slice("value:".length); }
}

export function policyFilterOptions(rows: PolicyRow[], field: PolicyFilter, selected: string) {
  const values = new Map<string, string>();
  for (const row of rows) {
    const value = policyFilterValue(row, field);
    values.set(`value:${value}`, field === "policy" ? policyOptionLabel(row.engine, row.policy) : value || (field === "namespace" ? "Cluster scoped / namespace not reported" : "Not reported"));
  }
  if (selected !== "all" && !values.has(selected)) values.set(selected, `${field === "policy" ? policyValueLabel(selected) : selected.slice(6)} (no loaded rows)`);
  return [{ value: "all", label: `All ${field === "policy" ? "policies" : field === "status" ? "statuses" : field === "severity" ? "severities" : `${field}s`}` }, ...[...values].sort((a, b) => a[1].localeCompare(b[1])).map(([value, label]) => ({ value, label }))];
}

export function filterPolicyRows<T extends PolicyRow>(rows: T[], filters: PolicyFilters): T[] {
  const terms = filters.search.toLowerCase().trim().split(/\s+/).filter(Boolean);
  return rows.filter(row => {
    const text = [row.engine, referenceLabel(row.policy), row.policy?.apiVersion, referenceLabel(row.resource), row.resource?.apiVersion, row.message, row.enforcement, "result" in row ? `${row.result} ${row.rule} ${row.severity}` : `${row.reason} ${row.action}`].join(" ").toLowerCase();
    return terms.every(term => text.includes(term)) && (Object.keys(emptyPolicyFilters) as (keyof PolicyFilters)[]).every(field => field === "search" || filters[field] === "all" || `value:${policyFilterValue(row, field)}` === filters[field]);
  });
}

/** Stale means collection stopped; unlike incomplete, it says nothing about source coverage. */
export function staleSnapshotLabel(summary: ClusterPolicySnapshotSummary | undefined, utc: (time?: Timestamp) => string): string | undefined {
  if (!summary?.stale) return undefined;
  return summary.collectedAt ? `Snapshot is stale — last collected ${utc(summary.collectedAt)}` : "Snapshot is stale — collection time unknown";
}

function snapshotKey(summary: ClusterPolicySnapshotSummary | undefined) {
  return JSON.stringify(summary ? [summary.collectedAt?.seconds.toString(), summary.collectedAt?.nanos, summary.incomplete, summary.truncated, summary.notices] : null);
}

/** Publish only a complete, consistent set of retained pages. Failed refreshes stay atomic. */
export async function collectPolicySnapshot<T extends PolicyRow>(load: (pageToken: string) => Promise<{ items: T[]; nextPageToken: string; summary?: ClusterPolicySnapshotSummary; retentionNotice?: string }>, signal?: AbortSignal) {
  let first: { summary?: ClusterPolicySnapshotSummary; retentionNotice?: string } | undefined;
  let key: string | undefined;
  const rows = await collectPages(async token => {
    const response = await load(token);
    const nextKey = snapshotKey(response.summary);
    if (first && (key !== nextKey || first.retentionNotice !== response.retentionNotice)) throw new Error("The stored policy snapshot changed while pages were loading. Refresh to read a consistent snapshot.");
    if (!first) { first = response; key = nextKey; }
    return response;
  }, signal);
  return { rows, summary: first?.summary, retentionNotice: first?.retentionNotice };
}
