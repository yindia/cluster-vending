"use client";

import { useEffect, useRef, useState } from "react";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { DropdownMenu } from "@astryxdesign/core/DropdownMenu";
import { Heading } from "@astryxdesign/core/Heading";
import { Icon } from "@astryxdesign/core/Icon";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { Pagination } from "@astryxdesign/core/Pagination";
import { Selector } from "@astryxdesign/core/Selector";
import { pixel, proportional } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { Tooltip } from "@astryxdesign/core/Tooltip";
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
  protected: boolean;
  hasEtag: boolean;
  allocationName: string;
}

export function SubTenantsView({ rows, loading, refreshing, error, canWrite, onRefresh, onCreate, onView, onEdit, onDelete, onDisableProtection }: {
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
  onDisableProtection: (id: string) => void;
}) {
  const [search, setSearch] = useState("");
  const [kind, setKind] = useState("all");
  const [state, setState] = useState("all");
  const [page, setPage] = useState(1);
  const container = useRef<HTMLElement>(null);
  const [width, setWidth] = useState(0);
  useEffect(() => {
    const element = container.current;
    if (!element) return;
    const measure = () => setWidth(Math.floor(element.getBoundingClientRect().width));
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(element);
    return () => observer.disconnect();
  }, []);
  const terms = search.trim().toLowerCase().split(/\s+/).filter(Boolean);
  const filtered = rows.filter((row) => (kind === "all" || kind === "SubTenant") && (state === "all" || row.state === state) && terms.every((term) => [row.name, row.allocationName, row.id, row.protected ? "Protected" : "Protection off", "SubTenant", row.state, row.message].join(" ").toLowerCase().includes(term)));
  const currentPage = Math.min(page, Math.max(1, Math.ceil(filtered.length / 25)));
  return <VStack ref={container} gap={4} as="section" aria-label="Cluster Sub Tenants" width="100%">
    <HStack gap={3} hAlign="between" vAlign="center" wrap="wrap" width="100%">
      <VStack gap={1}><Heading level={3}>Sub Tenants</Heading><Text type="supporting">Manage team namespaces, quotas and access within this cluster.</Text></VStack>
      <HStack gap={2} wrap="wrap" vAlign="center">{canWrite && <Button label="Create Sub Tenant" variant="primary" onClick={onCreate} />}<RefreshButton label="Refresh Sub Tenants" onClick={onRefresh} isLoading={refreshing} /></HStack>
    </HStack>
    {error && <Banner status="error" title="Sub Tenants unavailable" description={error} />}
    {loading && <Text role="status">Loading all Sub Tenants…</Text>}
    {!canWrite && <Text type="supporting">Read-only access. Tenant administrator permission is required to create, edit or delete Sub Tenants.</Text>}
    <HStack gap={3} vAlign="end" wrap="wrap">
      <TextInput label="Search Sub Tenants" type="search" value={search} onChange={(value) => { setSearch(value); setPage(1); }} autoComplete="off" hasClear width="min(100%, 24rem)" />
      <Selector label="Sub Tenant kind" value={kind} onChange={(value) => { setKind(value); setPage(1); }} options={[{ value: "all", label: "All kinds" }, { value: "SubTenant", label: "Sub Tenant" }]} />
      <Selector label="Sub Tenant status" value={state} onChange={(value) => { setState(value); setPage(1); }} options={[{ value: "all", label: "All statuses" }, ...[...new Set(rows.map((row) => row.state))].sort().map((value) => ({ value, label: value }))]} />
      {(search || kind !== "all" || state !== "all") && <Button label="Clear Sub Tenant filters" variant="ghost" onClick={() => { setSearch(""); setKind("all"); setState("all"); setPage(1); }} />}
    </HStack>
    {!loading && <Text type="supporting" role="status">{filtered.length} of {rows.length} Sub Tenants</Text>}
    {!!filtered.length && <ConsoleTable aria-label="Cluster Sub Tenants" data={filtered.slice((currentPage - 1) * 25, currentPage * 25)} idKey="id" rowIndexStart={(currentPage - 1) * 25 + 1} rowCount={filtered.length} columns={[
      { key: "name", header: "Sub Tenant", width: proportional(2, { minWidth: 100 }), renderCell: (row) => <Button label={row.name} tooltip={row.name} width="100%" variant="ghost" onClick={() => onView(row.id)} /> },
      ...(width >= 700 ? [{ key: "id", header: "ID", width: proportional(2, { minWidth: 130 }), renderCell: (row: SubTenantRow) => <CopyValue value={row.id} label="Sub Tenant ID" /> }] : []),
      ...(width >= 520 ? [{ key: "namespaces", header: "Namespaces", width: proportional(1, { minWidth: 82 }), renderCell: (row: SubTenantRow) => <Text>{row.namespaceCount}</Text> }] : []),
      ...(width >= 900 ? [{ key: "protection", header: "Protection", width: proportional(1, { minWidth: 86 }), renderCell: (row: SubTenantRow) => <Text>{row.protected ? "Protected" : "Off"}</Text> }] : []),
      { key: "state", header: "Status", width: proportional(1.5, { minWidth: 100 }), renderCell: (row) => <Tooltip content={row.message || row.state}><StatusValue label={row.state} tone={row.tone} /></Tooltip> },
      ...(width >= 1050 ? [{ key: "message", header: "Message", width: proportional(2, { minWidth: 150 }), renderCell: (row: SubTenantRow) => <Text maxLines={1} hasTruncateTooltip hasCapsize={false}>{row.message || "No message reported"}</Text> }] : []),
      { key: "actions", header: "Actions", width: pixel(76), resizable: false, renderCell: (row) => <DropdownMenu presentation="adaptive" alignment="end" aria-label="Actions" renderTrigger={(props) => <Button {...props} label={`Actions for Sub Tenant ${row.name} (${row.id})`} icon={<Icon icon="moreHorizontal" />} isIconOnly size="sm" variant="ghost" />} items={[
        { id: "view", label: "View", onClick: () => onView(row.id) },
        ...(canWrite ? [
          { id: "edit", label: "Edit", isDisabled: !row.mutable || loading || refreshing || !!error, onClick: () => onEdit(row.id) },
          ...(row.protected ? [{ id: "unprotect", label: "Disable deletion protection", isDisabled: !row.hasEtag || loading || refreshing || !!error, onClick: () => onDisableProtection(row.id) }] : []),
          { id: "delete", label: "Delete", description: row.protected ? "Disable deletion protection before deleting this Sub Tenant." : undefined, variant: "destructive" as const, isDisabled: row.protected || !row.mutable || loading || refreshing || !!error, onClick: () => onDelete(row.id) },
        ] : []),
      ]} /> },
    ]} />}
    {!loading && !filtered.length && <Text>{rows.length ? "No Sub Tenants match these filters." : error ? "No Sub Tenants can be confirmed. Refresh to retry." : "No Sub Tenants have been created in this cluster."}</Text>}
    {filtered.length > 25 && <Pagination label="Sub Tenant pagination" page={currentPage} onChange={setPage} pageSize={25} totalItems={filtered.length} variant="count" />}
  </VStack>;
}
