"use client";

import { StackItem } from "@astryxdesign/core/Stack";

import { RefreshButton } from "./refresh-button";

import { useCallback, useState } from "react";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { Link } from "@astryxdesign/core/Link";
import { Pagination } from "@astryxdesign/core/Pagination";
import { Selector } from "@astryxdesign/core/Selector";
import type { TableColumn } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { usePlatform } from "@/components/platform-provider";
import {
  ComponentManagement, ComponentPhase, type Component, type ComponentDefinition,
} from "@/gen/cloud/v1/component_pb";
import { useAsyncQuery } from "@/lib/api/use-query";
import { componentReady, loadClusterComponents, type ComponentAction } from "@/lib/workflows/components";
import { ComponentMutationForm, ComponentObservation } from "./components-page";
import { ConsoleTable } from "./console-table";
import { CopyValue } from "./copy-value";
import { StatusValue } from "./detail-layout";

type TenantComponentRow = {
  key: string;
  name: string;
  definition?: ComponentDefinition;
  current?: Component;
  ambiguous: boolean;
};
const PAGE_SIZE = 25;

function phaseLabel(phase: ComponentPhase) {
  return ComponentPhase[phase]?.toLowerCase().replaceAll("_", " ").replace(/^./, (letter) => letter.toUpperCase()) ?? `Unknown (${phase})`;
}

/** Keep table drafts and operation observation isolated when the selected scope changes. */
export function ClusterTenantComponents({ clusterId }: { clusterId: string }) {
  const { scopeKey } = usePlatform();
  return <TenantComponentsContent key={`${scopeKey}:${clusterId}`} clusterId={clusterId} />;
}

function TenantComponentsContent({ clusterId }: { clusterId: string }) {
  const { api, tenantId, canWrite } = usePlatform();
  const [search, setSearch] = useState("");
  const [availability, setAvailability] = useState("all");
  const [page, setPage] = useState(1);
  const [accepted, setAccepted] = useState<{ action: ComponentAction; component: Component; revision: number }>();
  const loadCatalog = useCallback(async (signal: AbortSignal) => {
    if (!tenantId || !clusterId) return undefined;
    return api.component.listComponentDefinitions({ tenantId, clusterId }, { signal });
  }, [api, tenantId, clusterId]);
  const loadInventory = useCallback(async (signal: AbortSignal) => {
    if (!tenantId || !clusterId) return undefined;
    return loadClusterComponents(api, tenantId, clusterId, signal);
  }, [api, tenantId, clusterId]);
  const catalog = useAsyncQuery(loadCatalog);
  const inventory = useAsyncQuery(loadInventory);
  const refreshCatalog = catalog.refresh;
  const refreshInventory = inventory.refresh;
  const refresh = useCallback(() => { refreshCatalog(); refreshInventory(); }, [refreshCatalog, refreshInventory]);
  const onAccepted = useCallback((action: ComponentAction, component: Component) => {
    setAccepted((previous) => ({ action, component, revision: (previous?.revision ?? 0) + 1 }));
    refresh();
  }, [refresh]);
  const inventoryComplete = inventory.data !== undefined && !inventory.error && !inventory.loading;
  const writesAvailable = !!tenantId && !!clusterId && canWrite && catalog.data !== undefined && inventoryComplete &&
    !catalog.loading && !catalog.error && !catalog.refreshing && !inventory.refreshing;

  const installedByName = new Map<string, Component[]>();
  for (const component of inventory.data ?? []) {
    const existing = installedByName.get(component.component) ?? [];
    installedByName.set(component.component, [...existing, component]);
  }
  const tenantDefinitions = new Map((catalog.data?.definitions ?? [])
    .filter((definition) => definition.management === ComponentManagement.TENANT)
    .map((definition) => [definition.name, definition]));
  const rows: TenantComponentRow[] = [];
  for (const definition of tenantDefinitions.values()) {
    const installed = installedByName.get(definition.name) ?? [];
    if (!installed.length) rows.push({ key: `catalog:${definition.name}`, name: definition.name, definition, ambiguous: false });
    for (const [index, current] of installed.entries()) rows.push({
      key: `catalog:${definition.name}:${current.id || index}`, name: definition.name,
      definition, current, ambiguous: installed.length > 1,
    });
  }
  // Platform components remain in the owning cluster's separate read-only section.
  for (const [index, current] of (inventory.data ?? []).entries()) {
    if (current.management === ComponentManagement.PLATFORM || tenantDefinitions.has(current.component)) continue;
    rows.push({ key: `inventory:${current.id || index}`, name: current.component, current, ambiguous: false });
  }
  rows.sort((left, right) => left.name.localeCompare(right.name) || left.key.localeCompare(right.key));

  function readOnlyReason(row: TenantComponentRow) {
    const { definition, current } = row;
    if (!definition) return "Not in the tenant catalog; read-only.";
    if (row.ambiguous) return "Multiple inventory records share this name. Refresh before changing it.";
    if (!current) return "";
    if (current.management !== ComponentManagement.TENANT) return "Platform-managed or unknown management; read-only.";
    if (!current.id) return "No component ID was returned; read-only.";
    if (!current.etag) return "No etag was returned. Refresh before changing it.";
    if (current.tenantId !== tenantId || current.clusterId !== clusterId) return "Inventory scope does not match this cluster; read-only.";
    if (current.status?.phase === ComponentPhase.DELETING) return "Removal is in progress.";
    return "";
  }
  const terms = search.trim().toLowerCase().split(/\s+/).filter(Boolean);
  const filtered = rows.filter((row) => {
    const text = [row.name, row.definition?.description, row.definition?.disabledReason, row.current?.id,
      row.current?.status?.installedVersion, row.current?.status?.targetVersion, row.current?.version].filter(Boolean).join(" ").toLowerCase();
    if (!terms.every((term) => text.includes(term))) return false;
    switch (availability) {
      case "available": return !!row.definition && !row.definition.disabled;
      case "enabled": return !!row.current;
      case "not-enabled": return inventoryComplete && !!row.definition && !row.current;
      case "unavailable": return !!row.definition?.disabled;
      case "read-only": return !!readOnlyReason(row);
      default: return true;
    }
  });
  const currentPage = Math.min(Math.max(1, page), Math.max(1, Math.ceil(filtered.length / PAGE_SIZE)));
  const visible = filtered.slice((currentPage - 1) * PAGE_SIZE, currentPage * PAGE_SIZE);
  const columns: TableColumn<TenantComponentRow>[] = [
    { key: "name", header: "Component", renderCell: (row) => row.name ? <Link href={`/components/${encodeURIComponent(row.name)}?cluster=${encodeURIComponent(clusterId)}`}>{row.name}</Link> : <Text>Unnamed inventory entry</Text> },
    { key: "status", header: "Status", renderCell: ({ current }) => {
      if (!inventoryComplete) return <StatusValue label={inventory.error ? "Inventory unavailable" : "Loading inventory"} />;
      if (!current) return <StatusValue label="Not enabled" />;
      const ready = componentReady(current);
      const phase = current.status?.phase;
      const label = phase === undefined ? "Not reported" : phase === ComponentPhase.READY && !ready ? "Reconciling" : phaseLabel(phase);
      return <StatusValue label={label} tone={ready ? "success" : phase === ComponentPhase.FAILED ? "error" : phase === ComponentPhase.DEGRADED ? "warning" : "neutral"} />;
    } },
    { key: "installed", header: "Installed", renderCell: ({ current }) => current?.status?.installedVersion ? <CopyValue value={current.status.installedVersion} label="Installed component version" /> : <Text>{current ? "Not reported" : "—"}</Text> },
    { key: "target", header: "Target", renderCell: ({ current }) => current?.status?.targetVersion || current?.version ? <CopyValue value={current.status?.targetVersion || current.version} label="Target component version" /> : <Text>Selected by release</Text> },
    { key: "configuration", header: "Configurability", renderCell: (row) => <Text maxLines={1}>{!row.definition ? "Unknown · read-only" : row.definition.configurable ? "Tenant values" : "No tenant values"}</Text> },
    { key: "availability", header: "Availability", renderCell: (row) => <Text maxLines={1}>{readOnlyReason(row) || (row.definition?.disabled ? row.definition.disabledReason || "Disabled by the platform" : "Available")}</Text> },
    { key: "actions", header: "Actions", renderCell: (row) => {
      if (!canWrite) return <Text color="secondary">Read-only</Text>;
      const reason = readOnlyReason(row);
      if (reason || !row.definition) return <Text color="secondary">Read-only</Text>;
      if (!writesAvailable || !row.current && row.definition.disabled) return <Button label={row.current ? "Manage component" : "Enable component"} isDisabled tooltip={row.definition.disabled && !row.current ? row.definition.disabledReason || "Disabled by the platform" : "Load and refresh the complete catalog and inventory before changing components."} />;
      return <ComponentMutationForm key={`${row.definition.name}:${row.current?.id ?? "new"}:${row.current?.etag ?? ""}:${row.definition.disabled}:${row.definition.configurable}`}
        context={{ tenantId, clusterId, canWrite, definition: row.definition, current: row.current }} onAccepted={onAccepted} />;
    } },
  ];

  return <VStack as="section" aria-label="Tenant components" gap={4}>
    <HStack hAlign="end" vAlign="center" gap={4} wrap="wrap" width="100%">
      <StackItem size="fill"><VStack gap={1}><Heading level={2}>Tenant components</Heading><Text color="secondary">Enable optional components or manage their tenant settings. Versions are selected by the cluster’s release.</Text></VStack></StackItem>
      <RefreshButton label="Refresh tenant components" isDisabled={!tenantId || !clusterId} isLoading={catalog.refreshing || inventory.refreshing} onClick={refresh} />
    </HStack>
    {(!tenantId || !clusterId) && <Banner status="info" title="Select a tenant and cluster to manage components" />}
    {!canWrite && <Text color="secondary">Your session has read-only access. Tenant write permission is required to enable, update or disable components.</Text>}
    {catalog.error && <Banner status="error" title="Tenant component catalog unavailable" description={catalog.error} />}
    {inventory.error && <Banner status="error" title="Complete component inventory unavailable" description={inventory.error} />}
    {(catalog.loading || inventory.loading) && <Text role="status">Loading the tenant catalog and complete cluster inventory…</Text>}
    {accepted && <ComponentObservation key={accepted.revision} tenantId={tenantId} action={accepted.action} accepted={accepted.component} onSettled={refresh} />}
    <HStack gap={3} wrap="wrap" vAlign="end">
      <TextInput label="Search tenant components" type="search" placeholder="Name, description, ID or version" value={search} onChange={(value) => { setSearch(value); setPage(1); }} autoComplete="off" />
      <Selector label="Component availability" value={availability} onChange={(value) => { setAvailability(value); setPage(1); }} options={[
        { value: "all", label: "All tenant entries" }, { value: "available", label: "Available in catalog" },
        { value: "enabled", label: "In cluster inventory" }, { value: "not-enabled", label: "Not enabled" },
        { value: "unavailable", label: "Platform disabled" }, { value: "read-only", label: "Unknown / read-only" },
      ]} />
      {(search || availability !== "all") && <Button label="Clear tenant component filters" onClick={() => { setSearch(""); setAvailability("all"); setPage(1); }} />}
    </HStack>
    <Text role="status" color="secondary">{filtered.length} of {rows.length} tenant component entries</Text>
    {visible.length ? <ConsoleTable aria-label="Cluster tenant component catalog" data={visible} columns={columns} idKey="key" rowIndexStart={(currentPage - 1) * PAGE_SIZE + 1} rowCount={filtered.length} /> : !catalog.loading && !inventory.loading && <Text>{rows.length ? "No tenant components match the current filters." : catalog.error || inventory.error ? "Tenant entries could not be fully loaded. Refresh before making changes." : "No tenant component definitions or unmanaged inventory entries were returned."}</Text>}
    {filtered.length > PAGE_SIZE && <Pagination label="Tenant component pagination" page={currentPage} onChange={setPage} totalItems={filtered.length} pageSize={PAGE_SIZE} variant="count" />}
  </VStack>;
}
