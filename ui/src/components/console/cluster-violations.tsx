"use client";

import { useCallback, useEffect, useId, useRef, useState } from "react";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Dialog, DialogHeader } from "@astryxdesign/core/Dialog";
import { Link } from "@astryxdesign/core/Link";
import { MetadataList, MetadataListItem } from "@astryxdesign/core/MetadataList";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { Pagination } from "@astryxdesign/core/Pagination";
import { Selector } from "@astryxdesign/core/Selector";
import { StackItem } from "@astryxdesign/core/Stack";
import { proportional, type TableColumn } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { timestampDate, type Timestamp } from "@bufbuild/protobuf/wkt";
import { usePlatform } from "@/components/platform-provider";
import { useAsyncQuery } from "@/lib/api/use-query";
import { collectPolicySnapshot, emptyPolicyFilters, filterPolicyRows, policyFilterOptions, referenceLabel, staleSnapshotLabel, type PolicyFilter, type PolicyRow } from "@/lib/workflows/cluster-violations";
import { ConsoleTable } from "./console-table";
import { CopyButton } from "./copy-value";
import { StatusValue } from "./detail-layout";
import { RefreshButton } from "./refresh-button";

function utcTime(time?: Timestamp): string {
  if (!time) return "Not reported";
  try { return `${timestampDate(time).toISOString().replace("T", " ").replace("Z", "")} UTC`; }
  catch { return "Invalid timestamp"; }
}

function Outcome({ item }: { item: PolicyRow }) {
  if ("reason" in item) return <VStack gap={1}>
    <Text type="label" maxLines={1} hasTruncateTooltip>{item.reason || "Not reported"}</Text>
    <Text type="supporting" color="secondary">{item.count > 0 ? `${item.count} occurrence${item.count === 1 ? "" : "s"}` : "Count unavailable"}</Text>
  </VStack>;
  const tone = item.result === "fail" || item.result === "error" ? "error" : item.result === "warn" ? "warning" : "neutral";
  const label = item.result === "fail" ? "Failed" : item.result === "warn" ? "Warning" : item.result || "Not reported";
  return <VStack gap={1}><StatusValue label={label} tone={tone} />
    {item.severity && <Text type="supporting" color="secondary">{item.severity} severity</Text>}
  </VStack>;
}

function PolicyDetails({ item, onClose }: { item: PolicyRow; onClose: () => void }) {
  const event = "reason" in item;
  function reference(value: PolicyRow["resource"], label: string) {
    if (!value) return "Not reported";
    const name = referenceLabel(value);
    return <VStack gap={1}><HStack className="cv-copy-target" gap={1} vAlign="center"><Text hasCapsize={false}>{name}</Text><CopyButton value={name} label={label} /></HStack>
      {value.apiVersion && <Text type="supporting" color="secondary">{value.apiVersion}</Text>}</VStack>;
  }
  return <Dialog isOpen onOpenChange={open => { if (!open) onClose(); }} width="min(38rem, calc(100vw - 2rem))" maxHeight="90dvh">
    <VStack gap={5} data-policy-details="">
      <DialogHeader title={event ? "Violation event details" : "Violation details"} onOpenChange={open => { if (!open) onClose(); }} />
      <VStack gap={2}><Heading level={3}>Message</Heading><Text hasCapsize={false}>{item.message || "Not reported"}</Text></VStack>
      <MetadataList label={{ position: "top" }} columns="multi">
        <MetadataListItem label="Policy">{reference(item.policy, "Policy")}</MetadataListItem>
        <MetadataListItem label="Resource">{reference(item.resource, "Resource")}</MetadataListItem>
        <MetadataListItem label="Engine">{item.engine || "Not reported"}</MetadataListItem>
        <MetadataListItem label="Enforcement">{item.enforcement || "Unknown"}</MetadataListItem>
        {event ? <>
          <MetadataListItem label="Reason">{item.reason || "Not reported"}</MetadataListItem><MetadataListItem label="Action">{item.action || "Not reported"}</MetadataListItem>
          <MetadataListItem label="Occurrences">{item.count || "Not reported"}</MetadataListItem>
        </> : <>
          <MetadataListItem label="Status">{item.result || "Not reported"}</MetadataListItem><MetadataListItem label="Severity">{item.severity || "Not reported"}</MetadataListItem>
          <MetadataListItem label="Rule">{item.rule || "Not reported"}</MetadataListItem>
        </>}
        <MetadataListItem label="Last observed">{utcTime(event ? item.lastObservedAt : item.observedAt)}</MetadataListItem>
      </MetadataList>
    </VStack>
  </Dialog>;
}

function PolicyTable({ items, events, offset, total }: { items: PolicyRow[]; events: boolean; offset: number; total: number }) {
  const container = useRef<HTMLElement>(null);
  const [compact, setCompact] = useState(true);
  const [selected, setSelected] = useState<PolicyRow>();
  useEffect(() => {
    const element = container.current;
    if (!element) return;
    const observer = new ResizeObserver(([entry]) => setCompact(entry.contentRect.width < 640));
    observer.observe(element);
    return () => observer.disconnect();
  }, []);
  const resource = (item: PolicyRow) => <VStack gap={1} className="cv-policy-cell">
    <Text maxLines={1} hasTruncateTooltip hasCapsize={false}>{item.resource?.name || "Not reported"}</Text>
    <Text type="supporting" color="secondary" maxLines={1} hasTruncateTooltip>{[item.resource?.kind, item.resource?.namespace].filter(Boolean).join(" · ")}</Text>
  </VStack>;
  const finding: TableColumn<PolicyRow> = { key: "policy", header: events ? "Event" : "Finding", width: proportional(3, { minWidth: 0 }), resizable: false, renderCell: item => {
    const name = item.policy?.name || ("reason" in item && item.reason) || "Policy details";
    return <VStack gap={2} className="cv-policy-cell" data-policy-finding="">
      <Link weight="semibold" tooltip={name} maxLines={1} onClick={() => setSelected(item)}>{name}</Link>
      {compact && <HStack className="cv-policy-resource-outcome" gap={3} vAlign="start">{resource(item)}<Outcome item={item} /></HStack>}
      <Text type="supporting" hasCapsize={false} maxLines={2} hasTruncateTooltip>{item.message || "No message reported"}</Text>
    </VStack>;
  } };
  const columns: TableColumn<PolicyRow>[] = compact ? [finding] : [
    finding,
    { key: "resource", header: "Resource", width: proportional(2, { minWidth: 0 }), resizable: false, renderCell: resource },
    { key: "outcome", header: events ? "Reason" : "Status", width: proportional(1.2, { minWidth: 0 }), resizable: false, renderCell: item => <Outcome item={item} /> },
    { key: "observed", header: "Observed (UTC)", width: proportional(1.2, { minWidth: 0 }), resizable: false, renderCell: item => {
      const stamp = "result" in item ? item.observedAt : item.lastObservedAt;
      if (!stamp) return <Text type="supporting">Not reported</Text>;
      const [date, time] = utcTime(stamp).split(" ");
      return <VStack gap={1}><Text>{date}</Text><Text type="supporting" color="secondary">{time?.split(".")[0]}</Text></VStack>;
    } },
  ];
  return <VStack ref={container} width="100%" className="cv-policy-cell">
    <ConsoleTable aria-label={events ? "Cluster violation events" : "Cluster violations"} data={items} columns={columns} idKey="id" rowIndexStart={offset + 1} rowCount={total} hasFullDetails />
    {selected && <PolicyDetails item={selected} onClose={() => setSelected(undefined)} />}
  </VStack>;
}

export function ClusterViolationsPanel({ clusterId, events = false }: { clusterId: string; events?: boolean }) {
  const { scopeKey } = usePlatform();
  return <ViolationsInventory key={`${scopeKey}:${clusterId}`} clusterId={clusterId} events={events} />;
}

function ViolationsInventory({ clusterId, events }: { clusterId: string; events: boolean }) {
  const { api, tenantId } = usePlatform();
  const [filters, setFilters] = useState({ ...emptyPolicyFilters });
  const [filtersOpen, setFiltersOpen] = useState(false);
  const filterId = useId();
  const [page, setPage] = useState(1);
  const title = events ? "Violation events" : "Current violations";
  const noun = events ? "violation events" : "violations";
  const load = useCallback(async (signal: AbortSignal) => {
    if (!tenantId || !clusterId) throw new Error("Select a tenant and cluster to read policy results.");
    return collectPolicySnapshot<PolicyRow>(async pageToken => {
      const request = { tenantId, clusterId, pageToken, pageSize: 200 };
      if (events) {
        const response = await api.cluster.listClusterViolationEvents(request, { signal });
        return { ...response, items: response.events };
      }
      const response = await api.cluster.listClusterViolations(request, { signal });
      return { ...response, items: response.violations };
    }, signal);
  }, [api, tenantId, clusterId, events]);
  const query = useAsyncQuery(load);
  const rows = query.data?.rows ?? [];
  const summary = query.data?.summary;
  const filtered = filterPolicyRows(rows, filters);
  const currentPage = Math.min(page, Math.max(1, Math.ceil(filtered.length / 25)));
  const visible = filtered.slice((currentPage - 1) * 25, currentPage * 25);
  const fields: PolicyFilter[] = events ? ["namespace", "kind", "policy", "engine"] : ["namespace", "kind", "status", "severity", "policy", "engine"];
  const activeFilterCount = fields.filter(field => filters[field] !== "all").length;
  const hasFilters = Object.entries(filters).some(([key, value]) => key === "search" ? !!value.trim() : value !== "all");
  const rowNoun = events ? "event" : "finding";
  const resultCount = `${hasFilters ? `${filtered.length} of ` : ""}${rows.length} retained ${rowNoun}${rows.length === 1 ? "" : "s"}`;
  const available = !!summary?.collectedAt;
  const staleLabel = staleSnapshotLabel(summary, utcTime);
  return <VStack className="cv-policy-panel" role="group" aria-label={title} gap={3} aria-busy={query.refreshing || query.loading}>
    <HStack gap={3} wrap="wrap" hAlign="end" vAlign="center" width="100%">
      <StackItem size="fill"><Heading level={2}>{title}</Heading></StackItem>
      <RefreshButton label={`Refresh ${noun}`} isLoading={query.refreshing} onClick={query.refresh} />
    </HStack>
    <Text type="supporting" color="secondary">{events ? query.data?.retentionNotice || "Kubernetes retains events for a limited time and can combine repeated occurrences." : "Policy reports show findings at collection time. Admission events are listed below."}</Text>
    {query.error && <Banner status="error" title={`Could not load ${noun}`} description={query.error} />}
    {query.error && query.data && <Text>Showing the last fully loaded snapshot. The failed refresh did not add partial pages.</Text>}
    {query.loading && <Text role="status">Loading all stored {noun} pages…</Text>}
    {query.data && <>
      <HStack className="cv-policy-summary" gap={4} wrap="wrap">
        <Text type="supporting">Last collected · {available ? utcTime(summary?.collectedAt).replace(".000 UTC", " UTC") : "Not available"}</Text>
        <Text type="supporting">Coverage · {!available ? "Unknown" : summary?.incomplete || summary?.truncated ? "Limited" : summary?.stale ? "Recorded snapshot (stale)" : "Recorded snapshot"}</Text>
      </HStack>
      {!available && <Banner status="warning" title="Collection unavailable" description="No collection timestamp was returned. An empty result is not evidence of zero violations or events." />}
      {staleLabel && <Banner status="warning" title={staleLabel} description="The controller has not collected recently, so these rows may no longer match the cluster. Coverage of the last collection is reported separately." />}
      {summary?.incomplete && <Banner status="warning" title="Incomplete policy coverage" description={`Some policy sources ${summary.collectedAt ? "could not be read during the last collection" : "have not been read yet"}. These rows do not represent complete cluster coverage.`} />}
      {summary?.truncated && <Banner status="warning" title="Stored results are truncated" description="The controller retained a bounded subset. Loading every API page does not recover omitted rows." />}
      {summary?.notices.map((notice, index) => <Banner key={index} status="info" title="Collection notice" description={notice} />)}
    </>}
    <HStack as="search" className="cv-policy-filters" aria-label={events ? "Violation event filters" : "Violation filters"} gap={2} vAlign="end">
      <TextInput label={`Search ${noun}`} type="search" value={filters.search} placeholder="Policy, resource, or message" onChange={search => { setFilters(previous => ({ ...previous, search })); setPage(1); }} hasClear autoComplete="off" />
      <Button className="cv-policy-filter-toggle" label={`Filters${activeFilterCount ? ` (${activeFilterCount})` : ""}`} variant="secondary" aria-expanded={filtersOpen} aria-controls={filterId} onClick={() => setFiltersOpen(open => !open)} />
      <HStack id={filterId} className="cv-policy-selectors" data-open={filtersOpen} gap={2}>
        {fields.map(field => <Selector key={field} label={field[0].toUpperCase() + field.slice(1)} hasSearch value={filters[field]} options={policyFilterOptions(rows, field, filters[field])} onChange={value => { setFilters(previous => ({ ...previous, [field]: value })); setPage(1); }} />)}
        {hasFilters && <Button label="Clear" aria-label={`Clear ${events ? "violation event" : "violation"} filters`} variant="ghost" onClick={() => { setFilters({ ...emptyPolicyFilters }); setPage(1); }} />}
      </HStack>
    </HStack>
    {query.data && <>
      {(available || rows.length > 0) && <Text type="supporting" role="status">{resultCount}{filtered.length > 25 && " · 25 per page"}</Text>}
      {visible.length > 0 ? <PolicyTable items={visible} events={events} offset={(currentPage - 1) * 25} total={filtered.length} /> : <Text>{hasFilters ? "No retained rows match your search and filters." : !available ? "Results will appear after collection becomes available." : summary?.incomplete || summary?.truncated ? "No rows were retained in this limited snapshot. This does not establish that no violations occurred." : `No ${noun} were retained in this snapshot. This is limited to the reported sources and retention coverage.`}</Text>}
      {filtered.length > 25 && <Pagination label={`${title} pagination`} page={currentPage} onChange={setPage} pageSize={25} totalItems={filtered.length} variant="count" />}
    </>}
  </VStack>;
}
