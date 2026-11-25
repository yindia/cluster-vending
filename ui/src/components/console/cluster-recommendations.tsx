"use client";

import { useCallback, useState } from "react";
import { equals } from "@bufbuild/protobuf";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { CheckboxInput } from "@astryxdesign/core/CheckboxInput";
import { EmptyState } from "@astryxdesign/core/EmptyState";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { Pagination } from "@astryxdesign/core/Pagination";
import { Selector } from "@astryxdesign/core/Selector";
import { StackItem } from "@astryxdesign/core/Stack";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { usePlatform } from "@/components/platform-provider";
import { collectPages } from "@/lib/api/pagination";
import { useAsyncQuery } from "@/lib/api/use-query";
import { ClusterRecommendationsSchema, type ClusterRecommendations, type ResourceRecommendation } from "@/gen/cloud/v1/recommendation_pb";
import {
  DEFAULT_FILTERS, KNOWN_SOURCES, STALE_AFTER_NANOS, emptyReason, isActionable, isFiltered, matchesFilters, parseNotices, sortRecommendations, sourceLabel, timestampNanos,
  type RecommendationFilters, type ShowFilter,
} from "@/lib/workflows/recommendations";
import { RecommendationInspector } from "./recommendation-inspector";
import { RecommendationNotices, RecommendationSources, RecommendationSummaryTiles } from "./recommendation-summary";
import { RecommendationTable, type RecommendationRow } from "./recommendation-table";
import { RefreshButton } from "./refresh-button";

const PAGE_SIZE = 25;

function sameRecommendationSnapshot(first: ClusterRecommendations | undefined, firstTotal: number, next: ClusterRecommendations | undefined, nextTotal: number): boolean {
  if (firstTotal !== nextTotal || Boolean(first) !== Boolean(next)) return false;
  return !first || !next || equals(ClusterRecommendationsSchema, first, next);
}

export function ClusterRecommendationsPanel({ clusterId }: { clusterId: string }) {
  const { scopeKey } = usePlatform();
  return <RecommendationsInventory key={`${scopeKey}:${clusterId}`} clusterId={clusterId} />;
}

function useRecommendations(clusterId: string, onlyChanges: boolean) {
  const { api, tenantId } = usePlatform();
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
  return useAsyncQuery(load);
}

function RecommendationsInventory({ clusterId }: { clusterId: string }) {
  const [onlyChanges, setOnlyChanges] = useState(false);
  const [filters, setFilters] = useState<RecommendationFilters>(DEFAULT_FILTERS);
  const [page, setPage] = useState(1);
  const [selected, setSelected] = useState<ResourceRecommendation>();
  const query = useRecommendations(clusterId, onlyChanges);
  const rows = query.data?.recommendations ?? [];
  const summary = query.data?.summary;
  const notices = parseNotices(summary);
  const update = (next: Partial<RecommendationFilters>) => { setFilters(previous => ({ ...previous, ...next })); setPage(1); };
  const clear = () => { setFilters(DEFAULT_FILTERS); setOnlyChanges(false); setPage(1); setSelected(undefined); };

  const filtered = sortRecommendations(rows.map((recommendation, id) => ({ id, recommendation })).filter(({ recommendation }) => matchesFilters(recommendation, filters)));
  const currentPage = Math.min(page, Math.max(1, Math.ceil(filtered.length / PAGE_SIZE)));
  const visible: RecommendationRow[] = filtered.slice((currentPage - 1) * PAGE_SIZE, currentPage * PAGE_SIZE);
  const actionable = rows.filter(isActionable).length;
  const withoutRecommendation = rows.length - actionable;

  const sampledAt = summary?.sampledAt ? timestampNanos(summary.sampledAt) : undefined;
  const changedAttempt = sampledAt !== undefined && !!summary?.attemptedAt && timestampNanos(summary.attemptedAt) > sampledAt;
  const oldSample = sampledAt !== undefined && !!query.data && query.data.readAtNanos - sampledAt > STALE_AFTER_NANOS;
  const stale = sampledAt !== undefined && (!!summary?.error || changedAttempt || oldSample || !!query.error);
  // With nothing to act on, the empty state explains why; the inventory stays one click away.
  const showEmpty = !!query.data && !actionable && !onlyChanges && filters.show === "recommended";
  const reason = emptyReason(notices, !!summary?.sampledAt);
  const bannerNotices = showEmpty ? notices.filter(notice => notice.kind !== "recording") : notices;

  return <VStack gap={5} as="section" aria-label="Cluster resource recommendations">
    <HStack gap={4} vAlign="start" width="100%">
      <StackItem size="fill"><VStack gap={1}>
        <Heading level={3}>Resource recommendations</Heading>
        <Text type="supporting">Suggested container requests from recorded usage and VPAs. Read-only: refresh reads stored results and changes nothing. Select a container for limits, usage and differences.</Text>
      </VStack></StackItem>
      <RefreshButton label="Refresh recommendations" isLoading={query.refreshing} onClick={() => { setSelected(undefined); query.refresh(); }} />
    </HStack>
    {query.error && <Banner status="error" title="Could not read recommendations" description={`${query.error}${query.data ? " Previously loaded results are shown." : ""}`} />}
    {query.loading && <Text role="status">Loading stored recommendations…</Text>}
    {query.data && <StatusBanners summary={summary} stale={stale} changedAttempt={changedAttempt} hasQueryError={!!query.error} hasTruncationNotice={notices.some(notice => notice.kind === "truncated")}
      total={query.data.total} loaded={rows.length} />}
    {query.data && summary && <VStack gap={3}>
      <RecommendationSummaryTiles summary={summary} rows={rows} onlyChanges={onlyChanges} stale={stale} readAtNanos={query.data.readAtNanos} />
      <RecommendationSources summary={summary} notices={notices} />
    </VStack>}
    <RecommendationNotices notices={bannerNotices} />
    {showEmpty && <Card padding={6}>
      <EmptyState title={reason.title} description={reason.description} headingLevel={4} isCompact
        actions={withoutRecommendation > 0 && <Button label={`Show all ${rows.length} containers`} aria-label={`Show ${withoutRecommendation} containers without recommendations`} variant="secondary" onClick={() => update({ show: "all" })} />} />
    </Card>}
    {!showEmpty && <VStack gap={3}>
      <RecommendationToolbar rows={rows} filters={filters} onlyChanges={onlyChanges} onChange={update} onClear={clear}
        onOnlyChanges={value => { setOnlyChanges(value); setFilters(previous => ({ ...previous, namespace: "" })); setPage(1); setSelected(undefined); }} />
      {query.data && <>
      <Text role="status" type="supporting">{`${filtered.length} of ${rows.length} containers${filters.show === "recommended" ? " with recommendations" : ""} · largest changes first`}</Text>
      <RecommendationTable rows={visible} rowIndexStart={(currentPage - 1) * PAGE_SIZE + 1} rowCount={filtered.length} onSelect={setSelected}
        emptyState={rows.length ? "No containers match these filters." : onlyChanges ? "No stored containers meet the ≥10% change threshold. This does not confirm workload sizing or usage availability." : "No stored container results were returned. No recommendation can be inferred from missing data."} />
      {filtered.length > PAGE_SIZE && <Pagination label="Recommendations pagination" page={currentPage} onChange={setPage} pageSize={PAGE_SIZE} totalItems={filtered.length} variant="count" />}
      </>}
    </VStack>}
    {selected && <RecommendationInspector recommendation={selected} onClose={() => setSelected(undefined)} />}
  </VStack>;
}

/** Banners about the read itself: no summary, a failed or stale sample, truncation and pagination drift. */
function StatusBanners({ summary, stale, changedAttempt, hasQueryError, hasTruncationNotice, total, loaded }: {
  summary: ClusterRecommendations | undefined; stale: boolean; changedAttempt: boolean; hasQueryError: boolean; hasTruncationNotice: boolean; total: number; loaded: number;
}) {
  const staleWhy = changedAttempt ? "A later sampling attempt has not produced a successful sample. Values reflect the last recorded sample."
    : hasQueryError ? "Refresh failed. Values reflect a previously loaded sample."
      : "The last successful sample was more than 15 minutes old when these results were read. Refresh only reads stored results.";
  return <>
    {!summary && <Banner status="warning" title="Sampling summary unavailable" description="The API returned no sampling summary. The freshness and completeness of these results cannot be confirmed." />}
    {summary && !summary.sampledAt && <Banner status="info" title="No successful sample recorded" description="The controller has not recorded a successful sample. Missing usage is not a zero-resource recommendation." />}
    {!!summary?.error && <Banner status="error" title="Last sampling attempt failed" description="Values reflect the last recorded sample.">
      <Text type="supporting">{`Server message: ${summary.error}`}</Text>
    </Banner>}
    {stale && !summary?.error && <Banner status="warning" title="Stored sample may be stale" description={staleWhy} />}
    {summary?.truncated && !hasTruncationNotice && <Banner status="warning" title="Recorded inventory is truncated" description="The controller retained the largest consumers. Other containers and namespaces may be missing from these results." />}
    {total !== loaded && <Banner status="warning" title="Stored results changed during pagination" description={`The first page reported ${total} matching containers; ${loaded} rows were loaded. Refresh to read the stored results again.`} />}
  </>;
}

function RecommendationToolbar({ rows, filters, onlyChanges, onChange, onClear, onOnlyChanges }: {
  rows: readonly ResourceRecommendation[]; filters: RecommendationFilters; onlyChanges: boolean;
  onChange: (next: Partial<RecommendationFilters>) => void; onClear: () => void; onOnlyChanges: (value: boolean) => void;
}) {
  const namespaces = [...new Set(rows.map(row => row.namespace))].filter(Boolean).sort();
  const kinds = [...new Set(rows.map(row => row.workloadKind))].sort();
  const sources = [...new Set([...KNOWN_SOURCES, ...rows.map(row => row.source)])];
  return <HStack as="search" aria-label="Recommendation filters" gap={3} vAlign="center" wrap="wrap" width="100%">
    <StackItem size="fill">
      <TextInput label="Search recommendations" isLabelHidden type="search" value={filters.search} onChange={search => onChange({ search })} placeholder="Search namespace, workload or container" hasClear autoComplete="off" width="100%" />
    </StackItem>
    <Selector label="Namespace" isLabelHidden hasSearch value={filters.namespace} onChange={namespace => onChange({ namespace })} options={[
      { value: "", label: "All namespaces" }, ...namespaces.map(value => ({ value, label: value })),
      ...(filters.namespace && !namespaces.includes(filters.namespace) ? [{ value: filters.namespace, label: `${filters.namespace} (no loaded rows)` }] : []),
    ]} />
    <Selector label="Kind" isLabelHidden value={filters.kind || "scope:all"} onChange={kind => onChange({ kind: kind === "scope:all" ? "" : kind })} options={[
      { value: "scope:all", label: "All kinds" }, ...kinds.map(value => ({ value: `kind:${value}`, label: value || "Not reported" })),
      ...(filters.kind && !kinds.some(value => `kind:${value}` === filters.kind) ? [{ value: filters.kind, label: `${filters.kind.slice(5) || "Not reported"} (no loaded rows)` }] : []),
    ]} />
    <Selector label="Recommendation source" isLabelHidden value={filters.show} onChange={show => onChange({ show: show as ShowFilter })} options={[
      { value: "recommended", label: "With recommendations" }, { value: "all", label: "All containers" }, { type: "divider" },
      ...sources.map(value => ({ value: `source:${value}`, label: sourceLabel(value) })),
    ]} />
    <CheckboxInput label="Only changes ≥10%" value={onlyChanges} onChange={onOnlyChanges} />
    {isFiltered(filters, onlyChanges) && <Button label="Clear filters" aria-label="Clear recommendation filters" variant="ghost" onClick={onClear} />}
  </HStack>;
}
