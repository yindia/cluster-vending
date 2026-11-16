"use client";

import { useCallback, useState } from "react";
import { equals } from "@bufbuild/protobuf";
import type { Timestamp } from "@bufbuild/protobuf/wkt";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { CheckboxInput } from "@astryxdesign/core/CheckboxInput";
import { Dialog, DialogHeader } from "@astryxdesign/core/Dialog";
import { Divider } from "@astryxdesign/core/Divider";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack } from "@astryxdesign/core/HStack";
import { MetadataList, MetadataListItem } from "@astryxdesign/core/MetadataList";
import { Pagination } from "@astryxdesign/core/Pagination";
import { Selector } from "@astryxdesign/core/Selector";
import { proportional, type TableColumn } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { VStack } from "@astryxdesign/core/VStack";
import { usePlatform } from "@/components/platform-provider";
import { collectPages } from "@/lib/api/pagination";
import { useAsyncQuery } from "@/lib/api/use-query";
import {
  RecommendationSource,
  ClusterRecommendationsSchema,
  type ClusterRecommendations,
  type ResourceAmounts,
  type ResourceRecommendation,
} from "@/gen/cloud/v1/recommendation_pb";
import { ConsoleTable } from "./console-table";
import { CopyValue } from "./copy-value";
import { DetailSummary, StatusValue } from "./detail-layout";

const PAGE_SIZE = 25;
const sourceLabels: Readonly<Record<number, string>> = {
  [RecommendationSource.UNSPECIFIED]: "Unspecified",
  [RecommendationSource.VPA]: "VPA",
  [RecommendationSource.USAGE_ESTIMATE]: "Usage estimate",
  [RecommendationSource.INSUFFICIENT_DATA]: "Insufficient data",
  [RecommendationSource.NO_DATA]: "No data",
};
const sourceLabel = (source: number) => sourceLabels[source] ?? `Unknown (${source})`;
const canRecommend = (row: ResourceRecommendation) => row.source === RecommendationSource.VPA || row.source === RecommendationSource.USAGE_ESTIMATE;
const hasRequest = (row: ResourceRecommendation, field: keyof Pick<ResourceAmounts, "cpuMillis" | "memoryBytes">) => canRecommend(row) && (row.recommendedRequests?.[field] ?? 0n) > 0n;

function SourceStatus({ source }: { source: RecommendationSource }) {
  return <StatusValue label={sourceLabel(source)} tone={source === RecommendationSource.INSUFFICIENT_DATA ? "warning" : source === RecommendationSource.VPA || source === RecommendationSource.USAGE_ESTIMATE ? "accent" : "neutral"} />;
}

/** All resource arithmetic stays in bigint, including fractional binary units. */
function memoryAmount(value: bigint): string {
  const negative = value < 0n;
  const absolute = negative ? -value : value;
  const units = [[1n << 60n, "EiB"], [1n << 50n, "PiB"], [1n << 40n, "TiB"], [1n << 30n, "GiB"], [1n << 20n, "MiB"], [1n << 10n, "KiB"]] as const;
  const unit = units.find(([factor]) => absolute >= factor);
  if (!unit) return `${value.toLocaleString()} B`;
  const [factor, suffix] = unit;
  const whole = absolute / factor;
  const remainder = absolute % factor;
  const fraction = (remainder * 100n / factor).toString().padStart(2, "0").replace(/0+$/, "");
  const approximate = remainder * 100n % factor !== 0n ? "≈ " : "";
  return `${approximate}${negative ? "−" : ""}${whole.toLocaleString()}${fraction ? `.${fraction}` : ""} ${suffix}`;
}

function amount(value: bigint, field: "cpuMillis" | "memoryBytes"): string {
  return field === "cpuMillis" ? `${value.toLocaleString()} mCPU` : memoryAmount(value);
}

function current(row: ResourceRecommendation, field: "cpuMillis" | "memoryBytes", limits = false): string {
  const value = (limits ? row.currentLimits : row.currentRequests)?.[field];
  return value === undefined || value === 0n ? "Not set" : amount(value, field);
}

function recommended(row: ResourceRecommendation, field: "cpuMillis" | "memoryBytes"): string {
  return hasRequest(row, field) ? amount(row.recommendedRequests![field], field) : "No recommendation";
}

function timestampNanos(timestamp: Timestamp): bigint {
  return timestamp.seconds * 1_000_000_000n + BigInt(timestamp.nanos);
}

function sameRecommendationSnapshot(first: ClusterRecommendations | undefined, firstTotal: number, next: ClusterRecommendations | undefined, nextTotal: number): boolean {
  if (firstTotal !== nextTotal || Boolean(first) !== Boolean(next)) return false;
  return !first || !next || equals(ClusterRecommendationsSchema, first, next);
}

function utcTime(timestamp: Timestamp | undefined, missing: string): string {
  if (!timestamp) return missing;
  const millis = timestampNanos(timestamp) / 1_000_000n;
  // JavaScript Date's complete supported range is below Number.MAX_SAFE_INTEGER.
  if (millis < -8_640_000_000_000_000n || millis > 8_640_000_000_000_000n) return "Timestamp outside display range";
  return `${new Date(Number(millis)).toISOString().replace("T", " ").replace(/\.\d{3}Z$/, "")} UTC`;
}

export function ClusterRecommendationsPanel({ clusterId }: { clusterId: string }) {
  const { scopeKey } = usePlatform();
  return <RecommendationsInventory key={`${scopeKey}:${clusterId}`} clusterId={clusterId} />;
}

function RecommendationsInventory({ clusterId }: { clusterId: string }) {
  const { api, tenantId } = usePlatform();
  const [onlyChanges, setOnlyChanges] = useState(false);
  const [search, setSearch] = useState("");
  const [namespace, setNamespace] = useState("");
  const [kind, setKind] = useState("scope:all");
  const [source, setSource] = useState("all");
  const [page, setPage] = useState(1);
  const [selected, setSelected] = useState<ResourceRecommendation>();
  const load = useCallback(async (signal: AbortSignal) => {
    let first = true;
    let summary: ClusterRecommendations | undefined;
    let total = 0;
    const recommendations = await collectPages(async (pageToken) => {
      const response = await api.cluster.listClusterRecommendations({ tenantId, clusterId, pageSize: 200, pageToken, onlyChanges }, { signal });
      if (first) { summary = response.summary; total = response.total; first = false; }
      else if (!sameRecommendationSnapshot(summary, total, response.summary, response.total)) {
        throw new Error("The stored recommendation sample changed while loading pages. Refresh recommendations to read a consistent sample.");
      }
      return { items: response.recommendations, nextPageToken: response.nextPageToken };
    }, signal);
    return { recommendations, summary, total, readAtNanos: BigInt(Date.now()) * 1_000_000n };
  }, [api, tenantId, clusterId, onlyChanges]);
  const query = useAsyncQuery(load);
  const rows = query.data?.recommendations ?? [];
  const summary = query.data?.summary;
  const namespaces = [...new Set(rows.map((row) => row.namespace))].sort();
  const kinds = [...new Set(rows.map((row) => row.workloadKind))].sort();
  const searchText = search.trim().toLocaleLowerCase();
  const filtered = rows.map((recommendation, id) => ({ id, recommendation })).filter(({ recommendation: row }) =>
    (!namespace || row.namespace === namespace) && (source === "all" || String(row.source) === source) &&
    (kind === "scope:all" || `kind:${row.workloadKind}` === kind) &&
    (!searchText || [row.namespace, row.workloadKind, row.workloadName, row.container].some((value) => value.toLocaleLowerCase().includes(searchText))));
  const currentPage = Math.min(page, Math.max(1, Math.ceil(filtered.length / PAGE_SIZE)));
  const visible = filtered.slice((currentPage - 1) * PAGE_SIZE, currentPage * PAGE_SIZE);
  const changedAttempt = !!summary?.sampledAt && !!summary.attemptedAt && timestampNanos(summary.attemptedAt) > timestampNanos(summary.sampledAt);
  const oldSample = !!summary?.sampledAt && !!query.data && query.data.readAtNanos - timestampNanos(summary.sampledAt) > 15n * 60n * 1_000_000_000n;
  const stale = !!summary?.sampledAt && (!!summary.error || changedAttempt || oldSample || !!query.error);
  type Row = { id: number; recommendation: ResourceRecommendation };
  const columns: TableColumn<Row>[] = [
    { key: "namespace", header: "Namespace", width: proportional(1, { minWidth: 80 }), renderCell: ({ recommendation: row }) => <CopyValue value={row.namespace} label="workload namespace" /> },
    { key: "kind", header: "Kind", width: proportional(1, { minWidth: 80 }), renderCell: ({ recommendation: row }) => <Text maxLines={1}>{row.workloadKind}</Text> },
    { key: "workload", header: "Workload", width: proportional(2, { minWidth: 120 }), renderCell: ({ recommendation: row }) => <CopyValue value={row.workloadName} label="workload name" /> },
    { key: "container", header: "Container", width: proportional(2, { minWidth: 120 }), renderCell: ({ recommendation: row }) => <Button label={row.container} variant="ghost" size="sm" onClick={() => setSelected(row)} /> },
    { key: "cpu", header: "Current CPU", width: proportional(1, { minWidth: 80 }), renderCell: ({ recommendation: row }) => <Text maxLines={1}>{current(row, "cpuMillis")}</Text> },
    { key: "memory", header: "Current memory", width: proportional(1, { minWidth: 80 }), renderCell: ({ recommendation: row }) => <Text maxLines={1}>{current(row, "memoryBytes")}</Text> },
    { key: "recommendedCpu", header: "Recommended CPU", width: proportional(1, { minWidth: 80 }), renderCell: ({ recommendation: row }) => <Text maxLines={1}>{recommended(row, "cpuMillis")}</Text> },
    { key: "recommendedMemory", header: "Recommended memory", width: proportional(1, { minWidth: 80 }), renderCell: ({ recommendation: row }) => <Text maxLines={1}>{recommended(row, "memoryBytes")}</Text> },
    { key: "source", header: "Source", width: proportional(2, { minWidth: 120 }), renderCell: ({ recommendation: row }) => <SourceStatus source={row.source} /> },
  ];
  return <VStack gap={6} as="section" aria-label="Cluster resource recommendations">
    <HStack gap={4} wrap="wrap" hAlign="between" vAlign="center">
      <VStack gap={1}><Heading level={3}>Resource recommendations</Heading><Text type="supporting">Stored suggestions for container requests. Select a container to inspect limits, usage and differences.</Text></VStack>
      <Button label="Refresh recommendations" variant="secondary" isLoading={query.refreshing} onClick={() => { setSelected(undefined); query.refresh(); }} />
    </HStack>
    <Text type="supporting">Refresh reads stored API results. It does not trigger sampling or change workloads. CPU is shown in millicores; memory uses binary units.</Text>
    {query.error && <Banner status="error" title="Could not read recommendations" description={`${query.error}${query.data ? " Previously loaded results are shown." : ""}`} />}
    {query.loading && <Text role="status">Loading stored recommendations…</Text>}
    {query.data && <>
      {!summary && <Banner status="warning" title="Sampling summary unavailable" description="The API returned no sampling summary. The freshness and completeness of these results cannot be confirmed." />}
      {summary && <>
        <DetailSummary items={[
          { label: "Recorded workloads", value: summary.sampledAt ? String(summary.workloads) : "Not sampled" },
          { label: "Recorded containers", value: summary.sampledAt ? String(summary.containers) : "Not sampled" },
          { label: "Usage window", value: summary.windowDays ? `${summary.windowDays} days` : "Not reported" },
          { label: "Sample freshness", value: <StatusValue label={!summary.sampledAt ? "Never sampled" : stale ? "May be stale" : "Recorded sample"} tone={stale ? "warning" : "neutral"} /> },
        ]} />
        <MetadataList orientation="horizontal">
          <MetadataListItem label="Last sampled (UTC)">{utcTime(summary.sampledAt, "Never sampled")}</MetadataListItem>
          <MetadataListItem label="Last attempted (UTC)">{utcTime(summary.attemptedAt, "No attempt recorded")}</MetadataListItem>
          <MetadataListItem label="Metrics API"><StatusValue label={summary.metricsAvailable ? "Available" : "Unavailable"} tone={summary.metricsAvailable ? "accent" : "neutral"} /></MetadataListItem>
          <MetadataListItem label="Vertical Pod Autoscaler"><StatusValue label={summary.vpaAvailable ? "Available" : "Unavailable"} tone={summary.vpaAvailable ? "accent" : "neutral"} /></MetadataListItem>
        </MetadataList>
        {!summary.sampledAt && <Banner status="info" title="No successful sample recorded" description="The controller has not recorded a successful sample. Missing usage is not a zero-resource recommendation." />}
        {!!summary.error && <Banner status="error" title="Last sampling attempt failed" description={summary.error} />}
        {stale && <Banner status="warning" title="Stored sample may be stale" description={summary.error || changedAttempt ? "A later sampling attempt has not produced a successful sample. Values reflect the last recorded sample." : query.error ? "Refresh failed. Values reflect a previously loaded sample." : "The last successful sample was more than 15 minutes old when these results were read. This is a UI freshness indicator; refresh only reads stored results."} />}
        {summary.truncated && <Banner status="warning" title="Recorded inventory is truncated" description="The controller retained the largest consumers. Other containers and namespaces may be missing from these results." />}
        {summary.message && <Banner status="info" title="Recommendation availability" description={summary.message} />}
      </>}
      {query.data.total !== rows.length && <Banner status="warning" title="Stored results changed during pagination" description={`The first page reported ${query.data.total} matching containers; ${rows.length} rows were loaded. Refresh to read the stored results again.`} />}
    </>}
    <HStack gap={3} wrap="wrap" vAlign="end">
      <TextInput label="Search recommendations" type="search" value={search} onChange={(value) => { setSearch(value); setPage(1); }} placeholder="Namespace, kind, workload or container" hasClear autoComplete="off" />
      <Selector label="Namespace" hasSearch value={namespace} onChange={(value) => { setNamespace(value); setPage(1); }} options={[
        { value: "", label: "All loaded namespaces" }, ...namespaces.filter(Boolean).map((value) => ({ value, label: value })),
        ...(namespace && !namespaces.includes(namespace) ? [{ value: namespace, label: `${namespace} (no loaded rows)` }] : []),
      ]} />
      <Selector label="Kind" value={kind} onChange={(value) => { setKind(value); setPage(1); }} options={[
        { value: "scope:all", label: "All kinds" }, ...kinds.map((value) => ({ value: `kind:${value}`, label: value || "Not reported" })),
        ...(kind !== "scope:all" && !kinds.some((value) => `kind:${value}` === kind) ? [{ value: kind, label: `${kind.slice(5) || "Not reported"} (no loaded rows)` }] : []),
      ]} />
      <Selector label="Recommendation source" value={source} onChange={(value) => { setSource(value); setPage(1); }} options={[
        { value: "all", label: "All sources" }, ...[...new Set([0, 1, 2, 3, 4, ...rows.map((row) => row.source)])].map((value) => ({ value: String(value), label: sourceLabel(value) })),
      ]} />
      <CheckboxInput label="Only changes ≥10%" description="Uses the server's request-difference threshold." value={onlyChanges} onChange={(value) => { setOnlyChanges(value); setNamespace(""); setPage(1); setSelected(undefined); }} />
      <Button label="Clear recommendation filters" variant="ghost" onClick={() => { setSearch(""); setNamespace(""); setKind("scope:all"); setSource("all"); setOnlyChanges(false); setPage(1); setSelected(undefined); }} />
    </HStack>
    <Text type="supporting">Namespace and kind options reflect loaded results{onlyChanges ? " that meet the server's change threshold" : ""}. Insufficient data and no-data rows do not carry recommendations.</Text>
    {query.data && <VStack gap={4}>
      <Text role="status" type="supporting">{filtered.length} of {rows.length} loaded container rows{summary?.truncated ? " · Truncated inventory" : ""}</Text>
      {visible.length ? <ConsoleTable aria-label="Container resource recommendations" data={visible} columns={columns} idKey="id" rowIndexStart={(currentPage - 1) * PAGE_SIZE + 1} rowCount={filtered.length} /> : <Text>{rows.length ? "No loaded rows match these filters." : onlyChanges ? "No stored containers meet the server's change threshold. This does not confirm workload sizing or usage availability." : "No stored container results were returned. No recommendation can be inferred from missing data."}</Text>}
      {filtered.length > PAGE_SIZE && <Pagination label="Recommendations pagination" page={currentPage} onChange={setPage} pageSize={PAGE_SIZE} totalItems={filtered.length} variant="count" />}
    </VStack>}
    {selected && <RecommendationInspector recommendation={selected} onClose={() => setSelected(undefined)} />}
  </VStack>;
}

function RecommendationInspector({ recommendation: row, onClose }: { recommendation: ResourceRecommendation; onClose: () => void }) {
  const usageRecorded = row.source !== RecommendationSource.NO_DATA && row.samples > 0;
  const usage = (values: ResourceAmounts | undefined, field: "cpuMillis" | "memoryBytes") => usageRecorded && values ? amount(values[field], field) : "No recorded usage";
  const difference = (field: "cpuMillis" | "memoryBytes") => {
    if (!hasRequest(row, field) || !row.difference) return "No recommendation";
    const value = row.difference[field];
    return `${value > 0n ? "+" : ""}${amount(value, field)}`;
  };
  const percent = (field: "cpuMillis" | "memoryBytes", value: number) => !hasRequest(row, field) ? "No recommendation" : !row.currentRequests?.[field] ? "Not applicable · current request not set" : `${value > 0 ? "+" : ""}${value}%`;
  const memoryLimit = canRecommend(row) && (row.recommendedLimits?.memoryBytes ?? 0n) > 0n ? amount(row.recommendedLimits!.memoryBytes, "memoryBytes") : "No recommendation";
  return <Dialog isOpen onOpenChange={(open) => { if (!open) onClose(); }} purpose="info" width="min(54rem, calc(100vw - 2rem))" maxHeight="90dvh" padding={0}>
    <DialogHeader title={`${row.workloadName} / ${row.container}`} subtitle="Read-only container resource recommendation" onOpenChange={(open) => { if (!open) onClose(); }} />
    <VStack gap={5} padding={6}>
      <MetadataList orientation="horizontal">
        <MetadataListItem label="Namespace"><CopyValue value={row.namespace} label="namespace" /></MetadataListItem>
        <MetadataListItem label="Workload kind">{row.workloadKind}</MetadataListItem>
        <MetadataListItem label="Workload"><CopyValue value={row.workloadName} label="workload name" /></MetadataListItem>
        <MetadataListItem label="Container"><CopyValue value={row.container} label="container name" /></MetadataListItem>
        <MetadataListItem label="Source"><SourceStatus source={row.source} /></MetadataListItem>
        <MetadataListItem label="Replicas">{row.replicas}</MetadataListItem>
        <MetadataListItem label="Platform workload">{row.platform ? "Yes" : "No"}</MetadataListItem>
        <MetadataListItem label="Samples / recorded hours">{row.samples} samples / {row.dataHours} hours</MetadataListItem>
      </MetadataList>
      {!canRecommend(row) && <Banner status="info" title={sourceLabel(row.source)} description={row.source === RecommendationSource.INSUFFICIENT_DATA ? "Less than one day of usage has been recorded. There is no usage-based recommendation yet." : "No supported recommendation is available. Missing values do not mean that this container needs zero CPU or memory."} />}
      <ConsoleTable aria-label="Container requests limits and usage" idKey="metric" data={[
        { metric: "Current requests", cpu: current(row, "cpuMillis"), memory: current(row, "memoryBytes") },
        { metric: "Recommended requests", cpu: recommended(row, "cpuMillis"), memory: recommended(row, "memoryBytes") },
        { metric: "Request difference", cpu: difference("cpuMillis"), memory: difference("memoryBytes") },
        { metric: "Request difference (%)", cpu: percent("cpuMillis", row.cpuPercent), memory: percent("memoryBytes", row.memoryPercent) },
        { metric: "Current limits", cpu: current(row, "cpuMillis", true), memory: current(row, "memoryBytes", true) },
        { metric: "Recommended limits", cpu: "CPU limits are never recommended", memory: memoryLimit },
        { metric: "Peak recorded usage", cpu: usage(row.usagePeak, "cpuMillis"), memory: usage(row.usagePeak, "memoryBytes") },
        { metric: "Average recorded usage", cpu: usage(row.usageAverage, "cpuMillis"), memory: usage(row.usageAverage, "memoryBytes") },
      ]} columns={[
        { key: "metric", header: "Metric", width: proportional(2, { minWidth: 120 }) },
        { key: "cpu", header: "CPU", width: proportional(2, { minWidth: 120 }) },
        { key: "memory", header: "Memory", width: proportional(2, { minWidth: 120 }) },
      ]} />
      <Text type="supporting">Memory displays marked ≈ are truncated to two decimal places. Exact byte quantities are preserved below. No values are applied to the workload.</Text>
      <Divider />
      <MetadataList title={<Heading level={3}>Exact memory quantities</Heading>}>
        <MetadataListItem label="Current request (bytes)">{row.currentRequests?.memoryBytes ? <CopyValue value={row.currentRequests.memoryBytes.toString()} label="current memory request in bytes" /> : "Not set"}</MetadataListItem>
        <MetadataListItem label="Recommended request (bytes)">{hasRequest(row, "memoryBytes") ? <CopyValue value={row.recommendedRequests!.memoryBytes.toString()} label="recommended memory request in bytes" /> : "No recommendation"}</MetadataListItem>
        <MetadataListItem label="Current limit (bytes)">{row.currentLimits?.memoryBytes ? <CopyValue value={row.currentLimits.memoryBytes.toString()} label="current memory limit in bytes" /> : "Not set"}</MetadataListItem>
        <MetadataListItem label="Recommended limit (bytes)">{canRecommend(row) && row.recommendedLimits?.memoryBytes ? <CopyValue value={row.recommendedLimits.memoryBytes.toString()} label="recommended memory limit in bytes" /> : "No recommendation"}</MetadataListItem>
      </MetadataList>
      <HStack hAlign="end"><Button label="Close recommendation details" variant="secondary" onClick={onClose} /></HStack>
    </VStack>
  </Dialog>;
}
