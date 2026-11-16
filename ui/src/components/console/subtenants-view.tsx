"use client";

import { useState } from "react";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { Pagination } from "@astryxdesign/core/Pagination";
import { Selector } from "@astryxdesign/core/Selector";
import { proportional } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { ConsoleTable } from "./console-table";
import { CopyValue } from "./copy-value";
import { StatusValue } from "./detail-layout";
import { RefreshButton } from "./refresh-button";

/** Presentation model only; generated API messages are adapted by the inventory owner. */
export interface SubTenantRow extends Record<string, unknown> {
  id: string;
  name: string;
  namespaceCount: number;
  state: string;
  tone: "neutral" | "success" | "warning" | "error";
  message: string;
  mutable: boolean;
}

export function SubTenantsView({ rows, loading, refreshing, error, canWrite, onRefresh, onCreate, onView, onEdit, onDelete }: {
  rows: SubTenantRow[];
  loading: boolean;
  refreshing: boolean;
  error: string;
  canWrite: boolean;
  onRefresh: () => void;
  onCreate: () => void;
  onView: (id: string) => void;
  onEdit: (id: string) => void;
  onDelete: (id: string) => void;
}) {
  const [search, setSearch] = useState("");
  const [kind, setKind] = useState("all");
  const [state, setState] = useState("all");
  const [page, setPage] = useState(1);
  const terms = search.trim().toLowerCase().split(/\s+/).filter(Boolean);
  const filtered = rows.filter((row) => (kind === "all" || kind === "SubTenant") && (state === "all" || row.state === state) && terms.every((term) => [row.name, row.id, "SubTenant", row.state, row.message].join(" ").toLowerCase().includes(term)));
  const currentPage = Math.min(page, Math.max(1, Math.ceil(filtered.length / 25)));
  return <VStack gap={4} as="section" aria-label="Cluster SubTenants">
    <HStack gap={3} hAlign="between" vAlign="center" wrap="wrap" width="100%">
      <VStack gap={1}><Heading level={3}>SubTenants</Heading><Text type="supporting">Manage team namespaces, quotas and access within this cluster.</Text></VStack>
      <HStack gap={2} wrap="wrap" vAlign="center">{canWrite && <Button label="Create SubTenant" variant="primary" onClick={onCreate} />}<RefreshButton label="Refresh SubTenants" onClick={onRefresh} isLoading={refreshing} /></HStack>
    </HStack>
    {error && <Banner status="error" title="SubTenants unavailable" description={error} />}
    {loading && <Text role="status">Loading all SubTenants…</Text>}
    {!canWrite && <Text type="supporting">Read-only access. Tenant administrator permission is required to create, edit or delete SubTenants.</Text>}
    <HStack gap={3} vAlign="end" wrap="wrap">
      <TextInput label="Search SubTenants" type="search" value={search} onChange={(value) => { setSearch(value); setPage(1); }} autoComplete="off" hasClear width="min(100%, 24rem)" />
      <Selector label="SubTenant kind" value={kind} onChange={(value) => { setKind(value); setPage(1); }} options={[{ value: "all", label: "All kinds" }, { value: "SubTenant", label: "SubTenant" }]} />
      <Selector label="SubTenant status" value={state} onChange={(value) => { setState(value); setPage(1); }} options={[{ value: "all", label: "All statuses" }, ...[...new Set(rows.map((row) => row.state))].sort().map((value) => ({ value, label: value }))]} />
      {(search || kind !== "all" || state !== "all") && <Button label="Clear SubTenant filters" variant="ghost" onClick={() => { setSearch(""); setKind("all"); setState("all"); setPage(1); }} />}
    </HStack>
    {!loading && <Text type="supporting" role="status">{filtered.length} of {rows.length} SubTenants</Text>}
    {!!filtered.length && <ConsoleTable aria-label="Cluster SubTenants" data={filtered.slice((currentPage - 1) * 25, currentPage * 25)} idKey="id" rowIndexStart={(currentPage - 1) * 25 + 1} rowCount={filtered.length} columns={[
      { key: "name", header: "SubTenant", width: proportional(2, { minWidth: 140 }), renderCell: (row) => <Button label={row.name} variant="ghost" onClick={() => onView(row.id)} /> },
      { key: "id", header: "ID", width: proportional(2, { minWidth: 140 }), renderCell: (row) => <CopyValue value={row.id} label="SubTenant ID" /> },
      { key: "kind", header: "Kind", width: proportional(1, { minWidth: 100 }), renderCell: () => <Text>SubTenant</Text> },
      { key: "namespaces", header: "Namespace keys", width: proportional(1, { minWidth: 120 }), renderCell: (row) => <Text>{row.namespaceCount}</Text> },
      { key: "state", header: "Status", width: proportional(1.5, { minWidth: 130 }), renderCell: (row) => <StatusValue label={row.state} tone={row.tone} /> },
      { key: "message", header: "Message", width: proportional(2, { minWidth: 140 }), renderCell: (row) => <Text maxLines={1} hasTruncateTooltip hasCapsize={false}>{row.message || "No message reported"}</Text> },
      { key: "actions", header: "Actions", width: proportional(2, { minWidth: 220 }), renderCell: (row) => <HStack gap={2} vAlign="center"><Button label="View" onClick={() => onView(row.id)} />{canWrite && <><Button label="Edit" isDisabled={!row.mutable || refreshing || !!error} onClick={() => onEdit(row.id)} /><Button label="Delete" variant="destructive" isDisabled={!row.mutable || refreshing || !!error} onClick={() => onDelete(row.id)} /></>}</HStack> },
    ]} />}
    {!loading && !filtered.length && <Text>{rows.length ? "No SubTenants match these filters." : error ? "No SubTenants can be confirmed. Refresh to retry." : "No SubTenants have been created in this cluster."}</Text>}
    {filtered.length > 25 && <Pagination label="SubTenant pagination" page={currentPage} onChange={setPage} pageSize={25} totalItems={filtered.length} variant="count" />}
  </VStack>;
}
