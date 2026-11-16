"use client";

import { StackItem } from "@astryxdesign/core/Stack";

import { RefreshButton } from "./refresh-button";

import { useCallback, useState } from "react";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Collapsible } from "@astryxdesign/core/Collapsible";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { Selector } from "@astryxdesign/core/Selector";
import { proportional, type TableColumn } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { timestampDate, type Timestamp } from "@bufbuild/protobuf/wkt";
import {
  DeprecationSource,
  type DeprecationFinding,
  type ListClusterDeprecationsResponse,
} from "@/gen/cloud/v1/deprecation_pb";
import { usePlatform } from "@/components/platform-provider";
import { collectPages } from "@/lib/api/pagination";
import { useAsyncQuery } from "@/lib/api/use-query";
import { ConsoleTable } from "./console-table";
import { CopyValue } from "./copy-value";
import { DetailSummary, StatusValue } from "./detail-layout";

interface FindingRow extends Record<string, unknown> {
  id: number;
  finding: DeprecationFinding;
}

function sourceLabel(source: DeprecationSource): string {
  switch (source) {
    case DeprecationSource.MANIFEST: return "Manifest";
    case DeprecationSource.HELM_RELEASE: return "Helm release";
    case DeprecationSource.UNSPECIFIED: return "Unspecified source";
    default: return `Unknown source (${source})`;
  }
}

function scanKey(scan: ListClusterDeprecationsResponse): string {
  return JSON.stringify([
    scan.targetVersion, scan.scannedAt ? `${scan.scannedAt.seconds}:${scan.scannedAt.nanos}` : null,
    scan.total, scan.removed, scan.truncated, scan.error,
  ]);
}

function utcTime(timestamp: Timestamp | undefined): string {
  if (!timestamp) return "Never scanned";
  try { return `${timestampDate(timestamp).toISOString().replace("T", " ").replace("Z", "")} UTC`; }
  catch { return "Invalid scan timestamp"; }
}

const columns: TableColumn<FindingRow>[] = [
  { key: "name", header: "Name", width: proportional(2, { minWidth: 120 }), renderCell: ({ finding }) => <CopyValue value={finding.name} label="Finding name" /> },
  { key: "namespace", header: "Namespace", width: proportional(1.5, { minWidth: 100 }), renderCell: ({ finding }) => <Text maxLines={1}>{finding.namespace || "Cluster scoped"}</Text> },
  { key: "kind", header: "Kind", width: proportional(1, { minWidth: 80 }), renderCell: ({ finding }) => <Text maxLines={1}>{finding.kind || "Not reported"}</Text> },
  { key: "apiVersion", header: "API version", width: proportional(2, { minWidth: 120 }), renderCell: ({ finding }) => <CopyValue value={finding.apiVersion} label="Deprecated API version" /> },
  { key: "impact", header: "Impact", width: proportional(1, { minWidth: 80 }), renderCell: ({ finding }) => <StatusValue label={finding.removed ? "Removed" : "Deprecated"} tone={finding.removed ? "error" : "warning"} /> },
  { key: "removedIn", header: "Removed in", width: proportional(1, { minWidth: 80 }), renderCell: ({ finding }) => <Text maxLines={1}>{finding.removedIn || "Not announced"}</Text> },
  { key: "replacement", header: "Replacement", width: proportional(2, { minWidth: 120 }), renderCell: ({ finding }) => finding.replacementApi ? <CopyValue value={finding.replacementApi} label="Replacement API version" /> : <Text maxLines={1}>None reported</Text> },
  { key: "source", header: "Source", width: proportional(1.5, { minWidth: 100 }), renderCell: ({ finding }) => <Text maxLines={1}>{sourceLabel(finding.source)}</Text> },
];

const metadataColumns: TableColumn<FindingRow>[] = [
  { key: "name", header: "Name", width: proportional(2, { minWidth: 120 }), renderCell: ({ finding }) => <CopyValue value={finding.name} label="Finding metadata name" /> },
  { key: "namespace", header: "Namespace", width: proportional(1, { minWidth: 80 }), renderCell: ({ finding }) => <Text maxLines={1}>{finding.namespace || "Cluster scoped"}</Text> },
  { key: "component", header: "Component", width: proportional(1, { minWidth: 80 }), renderCell: ({ finding }) => <Text maxLines={1}>{finding.component || "Not reported"}</Text> },
  { key: "deprecatedIn", header: "Deprecated in", width: proportional(1, { minWidth: 80 }), renderCell: ({ finding }) => <Text maxLines={1}>{finding.deprecatedIn || "Not reported"}</Text> },
  { key: "available", header: "Replacement available from", width: proportional(1.5, { minWidth: 100 }), renderCell: ({ finding }) => <Text maxLines={1}>{finding.replacementAvailableIn || "Not reported"}</Text> },
  { key: "helm", header: "Helm release", width: proportional(2, { minWidth: 120 }), renderCell: ({ finding }) => finding.helmRelease ? <CopyValue value={finding.helmRelease} label="Helm release" /> : <Text maxLines={1}>Not reported</Text> },
  { key: "source", header: "Source", width: proportional(1, { minWidth: 80 }), renderCell: ({ finding }) => <Text maxLines={1}>{sourceLabel(finding.source)}</Text> },
];

/** Reads the controller's stored scan; viewers never need a write or scan RPC. */
export function ClusterDeprecationsPanel({ clusterId }: { clusterId: string }) {
  const { api, tenantId } = usePlatform();
  const [search, setSearch] = useState("");
  const [namespace, setNamespace] = useState("scope:all");
  const [kind, setKind] = useState("scope:all");
  const [severity, setSeverity] = useState("all");
  const [source, setSource] = useState("all");
  const [page, setPage] = useState(0);
  const load = useCallback(async (signal: AbortSignal) => {
    if (!tenantId || !clusterId) throw new Error("Select a tenant and cluster to read deprecation findings.");
    let scan: ListClusterDeprecationsResponse | undefined;
    let expectedScan: string | undefined;
    const findings = await collectPages(async (pageToken) => {
      const response = await api.cluster.listClusterDeprecations({ tenantId, clusterId, pageSize: 200, pageToken, onlyRemoved: false }, { signal });
      const key = scanKey(response);
      if (expectedScan !== undefined && key !== expectedScan) throw new Error("The controller's scan changed while findings were being loaded. Refresh findings to read a consistent result.");
      if (!scan) { scan = response; expectedScan = key; }
      return { items: response.findings, nextPageToken: response.nextPageToken };
    }, signal);
    if (!scan) throw new Error("The API returned no deprecation scan information.");
    return { scan, findings };
  }, [api, tenantId, clusterId]);
  const query = useAsyncQuery(load);
  const scan = query.data?.scan;
  const findings = query.data?.findings ?? [];
  const namespaces = [...new Set(findings.map((finding) => finding.namespace).filter(Boolean))].sort();
  const kinds = [...new Set(findings.map((finding) => finding.kind))].sort();
  const sources = [...new Set([DeprecationSource.MANIFEST, DeprecationSource.HELM_RELEASE, ...findings.map((finding) => finding.source)])].sort((a, b) => a - b);
  const terms = search.trim().toLowerCase().split(/\s+/).filter(Boolean);
  const filtered = findings.map((finding, id) => ({ id, finding })).filter(({ finding }) => {
    const text = [finding.kind, finding.name, finding.namespace, finding.apiVersion, finding.replacementApi].join(" ").toLowerCase();
    return terms.every((term) => text.includes(term)) &&
      (namespace === "scope:all" || (namespace === "scope:cluster" ? !finding.namespace : `namespace:${finding.namespace}` === namespace)) &&
      (kind === "scope:all" || `kind:${finding.kind}` === kind) &&
      (severity === "all" || finding.removed === (severity === "removed")) &&
      (source === "all" || String(finding.source) === source);
  });
  const pageCount = Math.max(1, Math.ceil(filtered.length / 25));
  const currentPage = Math.min(page, pageCount - 1);
  const rows = filtered.slice(currentPage * 25, (currentPage + 1) * 25);
  const hasFilters = !!search.trim() || namespace !== "scope:all" || kind !== "scope:all" || severity !== "all" || source !== "all";
  const clear = () => { setSearch(""); setNamespace("scope:all"); setKind("scope:all"); setSeverity("all"); setSource("all"); setPage(0); };

  return <VStack gap={5}>
    <HStack hAlign="end" vAlign="center" gap={3} wrap="wrap" width="100%">
      <StackItem size="fill"><VStack gap={1}><Heading level={3}>Deprecated APIs</Heading><Text type="supporting">Findings from the controller’s last recorded scan.</Text></VStack></StackItem>
      <RefreshButton label="Refresh findings" onClick={query.refresh} isLoading={query.refreshing} />
    </HStack>
    <Text type="supporting">Refresh reloads saved findings, never starts a scan. Coverage is limited to retained manifests and Helm releases.</Text>
    {query.error && <Banner status="error" title="Could not load findings" description={query.error} />}
    {query.error && query.data && <Text type="supporting">Showing the last fully loaded scan. The failed refresh did not add partial pages.</Text>}
    {query.loading && <Text role="status">Loading all deprecation finding pages…</Text>}
    {scan && <>
      <DetailSummary items={[
        { label: "Target Kubernetes version", value: scan.targetVersion || "Not reported", note: "Version assessed by the controller" },
        { label: "Last successful scan (UTC)", value: <Text maxLines={1} hasTruncateTooltip>{utcTime(scan.scannedAt)}</Text>, note: "Successful results may precede a failed attempt" },
        { label: "Total findings", value: scan.scannedAt ? String(scan.total) : "Not available", note: "Count from the last successful scan" },
        { label: "Removed at target", value: scan.scannedAt ? String(scan.removed) : "Not available", note: "Cannot be applied again as written" },
      ]} />
      {scan.error && <Banner status="warning" title="Latest scan failed" description={scan.error}><Text>{scan.scannedAt ? "Counts and findings remain from the last successful scan; they do not indicate a successful latest attempt." : "No successful scan has been recorded. This error is not a clean scan result."}</Text></Banner>}
      {scan.truncated && <Banner status="warning" title="Findings are truncated" description="The controller retains at most 200 findings. Summary counts include all detections, but the table contains only the retained findings, even after every API page is loaded." />}
      {!scan.scannedAt && <Banner status="info" title="Never scanned" description="No successful scan has been recorded for this cluster. An empty finding list does not establish API compatibility." />}
      {!!scan.scannedAt && scan.total === 0 && !scan.error && !scan.truncated && <Banner status="success" title="No deprecated API findings" description="The last successful scan reported no deprecated or removed APIs within its manifest and Helm coverage. This does not cover every client or API call." />}
    </>}
    <HStack gap={3} wrap="wrap" vAlign="end">
      <TextInput type="search" label="Search findings" description="Kind, object name, namespace or API version." value={search} onChange={(value) => { setSearch(value); setPage(0); }} autoComplete="off" hasSpellCheck={false} width="min(100%, 24rem)" />
      <Selector label="Namespace" value={namespace} onChange={(value) => { setNamespace(value); setPage(0); }} width="min(100%, 14rem)" options={[{ value: "scope:all", label: "All namespaces" }, { value: "scope:cluster", label: "Cluster scoped" }, ...namespaces.map((value) => ({ value: `namespace:${value}`, label: value }))]} />
      <Selector label="Kind" value={kind} onChange={(value) => { setKind(value); setPage(0); }} width="min(100%, 14rem)" options={[{ value: "scope:all", label: "All kinds" }, ...kinds.map((value) => ({ value: `kind:${value}`, label: value || "Not reported" }))]} />
      <Selector label="Severity" value={severity} onChange={(value) => { setSeverity(value); setPage(0); }} width="min(100%, 12rem)" options={[{ value: "all", label: "All findings" }, { value: "removed", label: "Removed" }, { value: "deprecated", label: "Deprecated" }]} />
      <Selector label="Source" value={source} onChange={(value) => { setSource(value); setPage(0); }} width="min(100%, 12rem)" options={[{ value: "all", label: "All sources" }, ...sources.map((value) => ({ value: String(value), label: sourceLabel(value) }))]} />
      {hasFilters && <Button label="Clear finding filters" variant="ghost" onClick={clear} />}
    </HStack>
    {query.data && <>
      <Text type="supporting" role="status">{filtered.length} matching of {findings.length} retained findings · 25 per page{scan?.scannedAt ? ` · ${scan.total} reported by the last successful scan` : ""}</Text>
      {rows.length ? <ConsoleTable data={rows} columns={columns} idKey="id" aria-label="Cluster deprecated API findings" /> : <Text color="secondary">{hasFilters ? "No retained findings match your search and filters." : !scan?.scannedAt ? "Findings will appear after the first successful controller scan." : scan.error ? "No findings were retained from the last successful scan. The latest scan failed." : scan.total > 0 ? "The scan summary reports findings, but none were returned. Refresh findings to retry." : "No findings were retained from the last successful scan."}</Text>}
      {rows.length > 0 && <Collapsible trigger="Finding metadata · component and migration versions" defaultIsOpen={false}>
        <VStack gap={3}><Text type="supporting">Additional metadata for the same findings on this page. Versions belong to the named component; they are not always Kubernetes versions.</Text><ConsoleTable data={rows} columns={metadataColumns} idKey="id" aria-label="Deprecated API finding metadata" /></VStack>
      </Collapsible>}
      <HStack hAlign="end" vAlign="center" gap={2} wrap="wrap">
        <Text type="supporting">Page {currentPage + 1} of {pageCount}</Text>
        <Button label="Previous findings" onClick={() => setPage(currentPage - 1)} isDisabled={currentPage === 0} />
        <Button label="Next findings" onClick={() => setPage(currentPage + 1)} isDisabled={currentPage + 1 >= pageCount} />
      </HStack>
    </>}
    <Collapsible trigger="Scan coverage and cadence" defaultIsOpen={false}>
      <VStack gap={3}>
        <Text>The controller scans running clusters every six hours and when the target version changes. The API serves the stored result and never scans the workload cluster itself.</Text>
        <Text>Coverage includes original manifests retained by client-side kubectl apply and the newest deployed Helm v3 release manifests stored in Secrets. Objects without retained manifests, server-side apply and client or operator API calls are not covered. Helm releases stored in ConfigMaps are not included.</Text>
        <Text>Some high-volume live object kinds are skipped. A clean result is limited to this coverage; it is not a complete audit of API usage. Empty namespaces identify cluster-scoped objects; cluster-scoped objects from Helm may instead carry the release namespace.</Text>
      </VStack>
    </Collapsible>
  </VStack>;
}
