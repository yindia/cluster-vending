import type { Timestamp } from "@bufbuild/protobuf/wkt";
import {
  RecommendationSource,
  type ClusterRecommendations,
  type ResourceAmounts,
  type ResourceRecommendation,
} from "../../gen/cloud/v1/recommendation_pb";

export type ResourceField = "cpuMillis" | "memoryBytes";
export type Tone = "accent" | "success" | "warning" | "error" | "neutral";
export type NoticeStatus = "info" | "warning" | "error";

const SOURCE_LABELS: Readonly<Record<number, string>> = {
  [RecommendationSource.UNSPECIFIED]: "Unspecified",
  [RecommendationSource.VPA]: "VPA",
  [RecommendationSource.USAGE_ESTIMATE]: "Usage estimate",
  [RecommendationSource.INSUFFICIENT_DATA]: "Insufficient data",
  [RecommendationSource.NO_DATA]: "No data",
};

/** Every source the API defines, in display order. */
export const KNOWN_SOURCES: readonly RecommendationSource[] = [
  RecommendationSource.VPA, RecommendationSource.USAGE_ESTIMATE, RecommendationSource.INSUFFICIENT_DATA, RecommendationSource.NO_DATA, RecommendationSource.UNSPECIFIED,
];

export function sourceLabel(source: number): string {
  return SOURCE_LABELS[source] ?? `Unknown (${source})`;
}

export function sourceTone(source: number): Tone {
  if (source === RecommendationSource.INSUFFICIENT_DATA) return "warning";
  return canRecommendSource(source) ? "accent" : "neutral";
}

function canRecommendSource(source: number): boolean {
  return source === RecommendationSource.VPA || source === RecommendationSource.USAGE_ESTIMATE;
}

export function canRecommend(row: ResourceRecommendation): boolean {
  return canRecommendSource(row.source);
}

export function hasRequest(row: ResourceRecommendation, field: ResourceField): boolean {
  return canRecommend(row) && (row.recommendedRequests?.[field] ?? 0n) > 0n;
}

/** A row carries at least one recommended request. */
export function isActionable(row: ResourceRecommendation): boolean {
  return hasRequest(row, "cpuMillis") || hasRequest(row, "memoryBytes");
}

const MEMORY_UNITS = [[1n << 60n, "EiB"], [1n << 50n, "PiB"], [1n << 40n, "TiB"], [1n << 30n, "GiB"], [1n << 20n, "MiB"], [1n << 10n, "KiB"]] as const;

/** All resource arithmetic stays in bigint, including fractional binary units. */
export function memoryAmount(value: bigint): string {
  const negative = value < 0n;
  const absolute = negative ? -value : value;
  const unit = MEMORY_UNITS.find(([factor]) => absolute >= factor);
  if (!unit) return `${value.toLocaleString("en-US")} B`;
  const [factor, suffix] = unit;
  const whole = absolute / factor;
  const remainder = absolute % factor;
  const fraction = (remainder * 100n / factor).toString().padStart(2, "0").replace(/0+$/, "");
  const approximate = remainder * 100n % factor !== 0n ? "≈ " : "";
  return `${approximate}${negative ? "−" : ""}${whole.toLocaleString("en-US")}${fraction ? `.${fraction}` : ""} ${suffix}`;
}

export function amount(value: bigint, field: ResourceField): string {
  if (field === "memoryBytes") return memoryAmount(value);
  return `${value < 0n ? "−" : ""}${(value < 0n ? -value : value).toLocaleString("en-US")} mCPU`;
}

function signed(value: bigint, field: ResourceField): string {
  return `${value > 0n ? "+" : ""}${amount(value, field)}`;
}

export function current(row: ResourceRecommendation, field: ResourceField, limits = false): string {
  const value = (limits ? row.currentLimits : row.currentRequests)?.[field];
  return value === undefined || value === 0n ? "Not set" : amount(value, field);
}

export function recommended(row: ResourceRecommendation, field: ResourceField): string {
  return hasRequest(row, field) ? amount(row.recommendedRequests![field], field) : "No recommendation";
}

function percentOf(row: ResourceRecommendation, field: ResourceField): number {
  return field === "cpuMillis" ? row.cpuPercent : row.memoryPercent;
}

export interface ChangeView {
  /** "500 mCPU → 250 mCPU", or the current request alone without a recommendation. */
  value: string;
  /** "−50%", "Request not set", or why there is no recommendation. */
  detail: string;
  tone: Tone;
}

/** A request's current and recommended amounts in one cell. */
export function changeView(row: ResourceRecommendation, field: ResourceField): ChangeView {
  const now = current(row, field);
  if (!hasRequest(row, field)) {
    return { value: now, detail: `Current · ${canRecommend(row) ? "no recommendation" : sourceLabel(row.source).toLocaleLowerCase()}`, tone: "neutral" };
  }
  const next = amount(row.recommendedRequests![field], field);
  if (!row.currentRequests?.[field]) return { value: `Not set → ${next}`, detail: "New request", tone: "warning" };
  const percent = percentOf(row, field);
  return {
    value: `${now} → ${next}`,
    detail: percent === 0 ? "No change" : `${percent > 0 ? "+" : "−"}${Math.abs(percent)}%`,
    tone: percent > 0 ? "warning" : percent < 0 ? "success" : "neutral",
  };
}

export function differenceText(row: ResourceRecommendation, field: ResourceField): string {
  if (!hasRequest(row, field) || !row.difference) return "No recommendation";
  return signed(row.difference[field], field);
}

export function percentText(row: ResourceRecommendation, field: ResourceField): string {
  if (!hasRequest(row, field)) return "No recommendation";
  if (!row.currentRequests?.[field]) return "Not applicable · current request not set";
  const value = percentOf(row, field);
  return `${value > 0 ? "+" : ""}${value}%`;
}

export function usageText(row: ResourceRecommendation, values: ResourceAmounts | undefined, field: ResourceField): string {
  const recorded = row.source !== RecommendationSource.NO_DATA && row.samples > 0;
  return recorded && values ? amount(values[field], field) : "No recorded usage";
}

/** The largest request change of a row, for sorting; rows without a recommendation rank last. */
function changeWeight(row: ResourceRecommendation): number {
  if (!isActionable(row)) return -1;
  return Math.max(hasRequest(row, "cpuMillis") ? Math.abs(row.cpuPercent) : 0, hasRequest(row, "memoryBytes") ? Math.abs(row.memoryPercent) : 0);
}

function sourceRank(row: ResourceRecommendation): number {
  if (isActionable(row)) return 0;
  return row.source === RecommendationSource.INSUFFICIENT_DATA ? 1 : 2;
}

/** Actionable rows first, the biggest change first; then rows still recording, then the rest, by name. */
export function sortRecommendations<T extends { recommendation: ResourceRecommendation }>(rows: readonly T[]): T[] {
  const key = (row: ResourceRecommendation) => `${row.namespace}\u0000${row.workloadName}\u0000${row.container}`;
  return [...rows].sort((a, b) => sourceRank(a.recommendation) - sourceRank(b.recommendation)
    || changeWeight(b.recommendation) - changeWeight(a.recommendation)
    || key(a.recommendation).localeCompare(key(b.recommendation)));
}

/** The "Show" filter: actionable rows only, all rows, or one source. */
export type ShowFilter = "recommended" | "all" | `source:${number}`;

export interface RecommendationFilters {
  search: string;
  namespace: string;
  kind: string;
  show: ShowFilter;
}

export const DEFAULT_FILTERS: RecommendationFilters = { search: "", namespace: "", kind: "", show: "recommended" };

export function matchesFilters(row: ResourceRecommendation, filters: RecommendationFilters): boolean {
  const text = filters.search.trim().toLocaleLowerCase();
  if (filters.namespace && row.namespace !== filters.namespace) return false;
  if (filters.kind && `kind:${row.workloadKind}` !== filters.kind) return false;
  if (filters.show === "recommended" && !isActionable(row)) return false;
  if (filters.show.startsWith("source:") && String(row.source) !== filters.show.slice(7)) return false;
  return !text || [row.namespace, row.workloadKind, row.workloadName, row.container].some(value => value.toLocaleLowerCase().includes(text));
}

export function isFiltered(filters: RecommendationFilters, onlyChanges: boolean): boolean {
  return onlyChanges || filters.search.trim() !== "" || filters.namespace !== "" || filters.kind !== "" || filters.show !== DEFAULT_FILTERS.show;
}

export interface NetChange {
  /** Summed difference × replicas over rows that recommend this request. */
  total: bigint;
  higher: number;
  lower: number;
}

export function netChange(rows: readonly ResourceRecommendation[], field: ResourceField): NetChange {
  return rows.filter(row => hasRequest(row, field) && row.difference).reduce((acc, row) => {
    const diff = row.difference![field];
    const replicas = BigInt(Math.max(1, row.replicas));
    return { total: acc.total + diff * replicas, higher: acc.higher + (diff > 0n ? 1 : 0), lower: acc.lower + (diff < 0n ? 1 : 0) };
  }, { total: 0n, higher: 0, lower: 0 });
}

export function netChangeText(change: NetChange, field: ResourceField): string {
  return change.total === 0n ? "No net change" : signed(change.total, field);
}

export function netChangeNote(change: NetChange): string {
  if (!change.higher && !change.lower) return "No recommended change";
  return [change.lower ? `${change.lower} lower` : "", change.higher ? `${change.higher} higher` : ""].filter(Boolean).join(" · ") + " · all replicas";
}

/** One clearly titled notice parsed out of the summary's combined message. */
export interface RecommendationNotice {
  kind: "metrics" | "vpa" | "history" | "no-source" | "recording" | "truncated" | "results" | "other";
  status: NoticeStatus;
  title: string;
  description: string;
  /** The server's own wording, for the expandable detail. */
  raw: string;
}

const NOTICE_PREFIXES: readonly [RecommendationNotice["kind"], RegExp][] = [
  ["metrics", /the metrics API did not answer:/],
  ["vpa", /the Vertical Pod Autoscalers could not be listed:/],
  ["history", /the recorded usage could not be read/],
  ["no-source", /the cluster has neither metrics-server/],
  ["recording", /recording usage:/],
  ["truncated", /\d+ containers are not recorded/],
  ["results", /results unavailable:/],
];

function describe(kind: RecommendationNotice["kind"], raw: string, windowDays: number): Omit<RecommendationNotice, "kind" | "raw"> {
  const after = (marker: string) => raw.slice(raw.indexOf(marker) + marker.length).trim();
  switch (kind) {
  case "metrics": return { status: "error", title: "Metrics API is not answering", description: "The cluster registers the metrics API (metrics-server), but it did not answer the last sample, so no usage was recorded. Check that metrics-server is running and healthy." };
  case "vpa": return { status: "warning", title: "Vertical Pod Autoscalers could not be listed", description: "The last sample could not read the cluster's VPA recommendations. Usage-based estimates are unaffected." };
  case "history": return { status: "warning", title: "Recorded usage was reset", description: "The stored usage history could not be read and recording started again. Recommendations return once enough new usage is recorded." };
  case "no-source": return { status: "warning", title: "No usage source in this cluster", description: "The cluster has neither metrics-server nor a Vertical Pod Autoscaler, so no usage can be recorded and nothing is recommended. Install metrics-server to start recording." };
  case "recording": return { status: "info", title: "Recording usage", description: `Usage is being recorded. First recommendations appear after a day of usage and are reliable after a week${windowDays ? ` (the ${windowDays}-day window)` : ""}.` };
  case "truncated": return { status: "warning", title: "Some containers are not recorded", description: `${raw.replace(/^(\d+) containers are not recorded.*$/, "$1")} containers are not recorded: only the largest consumers are kept.` };
  case "results": return { status: "warning", title: "Saved results could not be read", description: `${capitalize(after("results unavailable:"))}. Recommendations are computed from the usage the platform recorded instead.` };
  default: return { status: "info", title: "Recommendation notice", description: capitalize(raw) };
  }
}

function capitalize(text: string): string {
  return text ? text[0].toLocaleUpperCase() + text.slice(1) : text;
}

/**
 * The summary message joins several unrelated notes ("; " between the controller's, a space before the API's
 * coverage notice). Split it at each known note's start so each gets its own title and severity.
 */
export function parseNotices(summary: Pick<ClusterRecommendations, "message" | "windowDays"> | undefined): RecommendationNotice[] {
  const message = summary?.message.trim() ?? "";
  if (!message) return [];
  const starts = NOTICE_PREFIXES.flatMap(([kind, pattern]) => {
    const global = new RegExp(pattern.source, "g");
    return [...message.matchAll(global)].map(match => ({ kind, index: match.index }));
  }).sort((a, b) => a.index - b.index);
  const bounds = starts.length && starts[0].index > 0 ? [{ kind: "other" as const, index: 0 }, ...starts] : starts.length ? starts : [{ kind: "other" as const, index: 0 }];
  return bounds.map((bound, i) => {
    const raw = message.slice(bound.index, bounds[i + 1]?.index ?? message.length).replace(/[;\s]+$/, "").trim();
    return { kind: bound.kind, raw, ...describe(bound.kind, raw, summary?.windowDays ?? 0) };
  }).filter(notice => notice.raw);
}

export interface AvailabilityView {
  label: string;
  tone: Tone;
  note?: string;
}

/** The metrics API as the summary flag and the notes report it together: registered is not the same as answering. */
export function metricsView(summary: Pick<ClusterRecommendations, "metricsAvailable"> | undefined, notices: readonly RecommendationNotice[]): AvailabilityView {
  if (!summary) return { label: "Unknown", tone: "neutral" };
  const failing = notices.some(notice => notice.kind === "metrics");
  if (!summary.metricsAvailable) return failing ? { label: "Not answering", tone: "error" } : { label: "Not installed", tone: "neutral" };
  return failing ? { label: "Registered · not answering", tone: "error", note: "The API is registered but its last call failed." } : { label: "Answering", tone: "success" };
}

export function vpaView(summary: Pick<ClusterRecommendations, "vpaAvailable"> | undefined, notices: readonly RecommendationNotice[]): AvailabilityView {
  if (!summary) return { label: "Unknown", tone: "neutral" };
  const failing = notices.some(notice => notice.kind === "vpa");
  if (failing) return { label: summary.vpaAvailable ? "Installed · not listed" : "Not listed", tone: "warning" };
  return summary.vpaAvailable ? { label: "Installed", tone: "success" } : { label: "Not installed", tone: "neutral" };
}

/** Why there are no recommendations yet, from the notices; the first reason that applies. */
export function emptyReason(notices: readonly RecommendationNotice[], sampled: boolean): { title: string; description: string } {
  const has = (kind: RecommendationNotice["kind"]) => notices.some(notice => notice.kind === kind);
  if (!sampled) return { title: "No sample recorded yet", description: "The controller samples the cluster every five minutes. Recommendations appear after a day of recorded usage." };
  if (has("no-source")) return { title: "Nothing to record usage from", description: "This cluster has neither metrics-server nor a Vertical Pod Autoscaler. Install metrics-server; recommendations appear a day after it starts answering." };
  if (has("metrics")) return { title: "No usage is being recorded", description: `The metrics API is not answering, so no usage is recorded.${has("recording") ? " Once it answers, first recommendations appear after a day and are reliable after a week." : " Recommendations appear a day after it answers again."}` };
  if (has("history")) return { title: "Recording started again", description: "The usage history was reset. First recommendations appear after a day of new usage." };
  if (has("recording")) return { title: "Recording usage", description: "First recommendations appear after a day of recorded usage and are reliable after a week." };
  return { title: "No recommendations yet", description: "No container has enough recorded usage or a VPA recommendation. Containers appear here once they do." };
}

export function timestampNanos(timestamp: Timestamp): bigint {
  return timestamp.seconds * 1_000_000_000n + BigInt(timestamp.nanos);
}

const DATE_LIMIT_MS = 8_640_000_000_000_000n;

export function utcTime(timestamp: Timestamp | undefined, missing: string): string {
  if (!timestamp) return missing;
  const millis = timestampNanos(timestamp) / 1_000_000n;
  // JavaScript Date's complete supported range is below Number.MAX_SAFE_INTEGER.
  if (millis < -DATE_LIMIT_MS || millis > DATE_LIMIT_MS) return "Timestamp outside display range";
  return `${new Date(Number(millis)).toISOString().replace("T", " ").replace(/:\d{2}\.\d{3}Z$/, "")} UTC`;
}

const MINUTE_NANOS = 60n * 1_000_000_000n;

/** "just now", "12 min ago", "3 h ago", "2 d ago". */
export function ageText(timestamp: Timestamp | undefined, nowNanos: bigint): string {
  if (!timestamp) return "Never";
  const minutes = (nowNanos - timestampNanos(timestamp)) / MINUTE_NANOS;
  if (minutes < 1n) return "Just now";
  if (minutes < 60n) return `${minutes} min ago`;
  if (minutes < 1440n) return `${minutes / 60n} h ago`;
  return `${minutes / 1440n} d ago`;
}

/** A sample older than this when read is marked as possibly stale (the controller samples every five minutes). */
export const STALE_AFTER_NANOS = 15n * MINUTE_NANOS;
