"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Collapsible } from "@astryxdesign/core/Collapsible";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { MetadataList, MetadataListItem } from "@astryxdesign/core/MetadataList";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { SubTenantState, type SubTenant } from "@/gen/cloud/v1/subtenant_pb";
import { usePlatform } from "@/components/platform-provider";
import { collectPages } from "@/lib/api/pagination";
import { errorMessage, useAsyncQuery } from "@/lib/api/use-query";
import { deleteSubTenantRequest, disableSubTenantProtection, scopedSubTenant, subTenantYaml, type SubTenantScope } from "@/lib/workflows/subtenants";
import { ConsoleTable } from "./console-table";
import { CopyValue } from "./copy-value";
import { RefreshButton } from "./refresh-button";
import { StatusValue } from "./detail-layout";
import { SubTenantEditor } from "./subtenant-editor";
import { SubTenantsView, type SubTenantRow } from "./subtenants-view";
import { ValuesEditor } from "./values-editor";
import { WizardDialog } from "./wizard-dialog";

export function subTenantRow(resource: SubTenant): SubTenantRow {
  const status = resource.status;
  const observed = !!status && status.observedGeneration >= resource.generation;
  const ready = status?.state === SubTenantState.READY && observed;
  const state = ready ? "Ready" : status?.state === SubTenantState.READY ? "Awaiting observation" : ({ [SubTenantState.UNSPECIFIED]: "Pending", [SubTenantState.PENDING]: "Pending", [SubTenantState.PROVISIONING]: "Provisioning", [SubTenantState.DEGRADED]: "Degraded", [SubTenantState.FAILED]: "Failed", [SubTenantState.DELETING]: "Deleting" }[status?.state ?? 0] ?? "Pending");
  return { id: resource.id, name: resource.spec?.displayName || resource.spec?.name || resource.id, allocationName: resource.spec?.name ?? "", protected: resource.spec?.deletionProtection !== false, hasEtag: !!resource.etag, namespaceCount: resource.spec?.namespaceKeys.length ?? 0, state, tone: ready ? "success" : status?.state === SubTenantState.FAILED ? "error" : status?.state === SubTenantState.DEGRADED ? "warning" : "neutral", message: status?.message ?? "", mutable: !!resource.etag && status?.state !== SubTenantState.DELETING };
}

export function ClusterSubTenants({ clusterId }: { clusterId: string }) {
  const { scopeKey } = usePlatform();
  return <ScopedSubTenants key={`${scopeKey}:${clusterId}`} clusterId={clusterId} />;
}
function ScopedSubTenants({ clusterId }: { clusterId: string }) {
  const { api, tenantId, canWrite } = usePlatform();
  const scope = { tenantId, clusterId };
  const [notice, setNotice] = useState("");
  const [creating, setCreating] = useState(false);
  const [selection, setSelection] = useState<{ id: string; mode: "view" | "edit" | "delete" | "unprotect" }>();
  const [accepted, setAccepted] = useState<{ resource: SubTenant; action: "create" | "update" | "delete" }>();
  const load = useCallback((signal: AbortSignal) => collectPages(async (pageToken) => {
    const page = await api.subtenant.listSubTenants({ tenantId, clusterId, pageSize: 200, pageToken }, { signal });
    return { items: page.subtenants.map((resource) => scopedSubTenant(resource, { tenantId, clusterId })), nextPageToken: page.nextPageToken };
  }, signal), [api, tenantId, clusterId]);
  const list = useAsyncQuery(load);
  function onAccepted(resource: SubTenant, action: "create" | "update" | "delete" | "unprotect") { setCreating(false); setSelection(undefined); if (action === "unprotect") { setAccepted(undefined); setNotice("Deletion protection disabled. Pending deletion may resume; otherwise use Delete when the allocation is empty."); } else { setNotice(""); setAccepted({ resource, action }); } list.refresh(); }
  return <VStack gap={4}>
    {notice && <Banner status="info" title="Deletion protection disabled" description={notice} />}
    {accepted && <SubmittedSubTenant key={`${accepted.resource.id}:${accepted.resource.generation}:${accepted.action}`} scope={scope} accepted={accepted} onRefresh={list.refresh} />}
    <SubTenantsView rows={(list.data ?? []).map(subTenantRow)} loading={list.loading} refreshing={list.refreshing} error={list.error} canWrite={canWrite} onRefresh={list.refresh} onCreate={() => setCreating(true)} onView={(id) => setSelection({ id, mode: "view" })} onEdit={(id) => setSelection({ id, mode: "edit" })} onDelete={(id) => setSelection({ id, mode: "delete" })} onDisableProtection={(id) => setSelection({ id, mode: "unprotect" })} />
    {creating && canWrite && <SubTenantEditor scope={scope} onClose={() => setCreating(false)} onAccepted={onAccepted} />}
    {selection && <SelectedSubTenant key={`${selection.id}:${selection.mode}`} scope={scope} {...selection} onClose={() => setSelection(undefined)} onAccepted={onAccepted} />}
  </VStack>;
}
function SubmittedSubTenant({ scope, accepted, onRefresh }: { scope: SubTenantScope; accepted: { resource: SubTenant; action: "create" | "update" | "delete" }; onRefresh: () => void }) {
  const { api } = usePlatform();
  const { tenantId, clusterId } = scope;
  const { resource, action } = accepted;
  const load = useCallback(async (signal: AbortSignal) => {
    try {
      const response = await api.subtenant.getSubTenant({ tenantId, clusterId, subtenantId: resource.id }, { signal });
      return scopedSubTenant(response.subtenant, { tenantId, clusterId }, resource.id);
    } catch (cause) { if (action === "delete" && cause instanceof ConnectError && cause.code === Code.NotFound) return null; throw cause; }
  }, [api, tenantId, clusterId, resource.id, action]);
  const complete = useCallback((current: SubTenant | null) => current === null || (action !== "delete" && !!current.status && current.status.observedGeneration >= resource.generation && [SubTenantState.READY, SubTenantState.FAILED, SubTenantState.DEGRADED].includes(current.status.state)), [action, resource.generation]);
  const observation = useAsyncQuery(load, { observeOperation: true, pollMs: 2000, isComplete: complete });
  useEffect(() => { if (observation.data !== undefined && complete(observation.data)) onRefresh(); }, [observation.data, complete, onRefresh]);
  const row = observation.data ? subTenantRow(observation.data) : undefined;
  return <Banner status={observation.error || row?.tone === "error" ? "error" : row?.tone === "warning" ? "warning" : "info"} title={observation.data === null ? "Sub Tenant deleted" : `Sub Tenant ${action === "create" ? "creation" : action} saved${row ? ` · ${row.state}` : ""}`} description={observation.error || observation.data?.status?.message || (observation.data === null ? "The allocation is no longer reported by the server." : "Changes are saved first. Namespaces become available after setup and policy checks finish.")} />;
}
function SelectedSubTenant({ scope, id, mode, onClose, onAccepted }: { scope: SubTenantScope; id: string; mode: "view" | "edit" | "delete" | "unprotect"; onClose: () => void; onAccepted: (resource: SubTenant, action: "create" | "update" | "delete" | "unprotect") => void }) {
  const { api, canWrite } = usePlatform();
  const { tenantId, clusterId } = scope;
  const load = useCallback(async (signal: AbortSignal) => scopedSubTenant((await api.subtenant.getSubTenant({ tenantId, clusterId, subtenantId: id }, { signal })).subtenant, { tenantId, clusterId }, id), [api, tenantId, clusterId, id]);
  const detail = useAsyncQuery(load);
  if (detail.data && !detail.error && mode === "edit" && canWrite && detail.data.status?.state !== SubTenantState.DELETING) return <SubTenantEditor scope={scope} baseline={detail.data} onClose={onClose} onAccepted={onAccepted} />;
  if (detail.data && !detail.error && mode === "delete" && canWrite) return <DeleteSubTenant scope={scope} resource={detail.data} onClose={onClose} onAccepted={onAccepted} />;
  if (detail.data && !detail.error && mode === "unprotect" && canWrite) return <DisableProtection scope={scope} resource={detail.data} onClose={onClose} onAccepted={onAccepted} />;
  const resource = detail.data;
  const row = resource && subTenantRow(resource);
  return <WizardDialog isOpen onOpenChange={(open) => { if (!open) onClose(); }} title={mode === "view" ? "Sub Tenant details" : `Load Sub Tenant to ${mode}`} steps={["Details"]} step={0} footer={<HStack gap={2}><Button label="Close" onClick={onClose} /><RefreshButton label="Refresh Sub Tenant details" onClick={detail.refresh} isLoading={detail.refreshing} /></HStack>}>
    {detail.loading && <Text role="status">Loading Sub Tenant…</Text>}
    {detail.error && <Banner status="error" title="Sub Tenant unavailable" description={detail.error} />}
    {resource && !detail.error && <VStack gap={4}>
      <MetadataList><MetadataListItem label="Name">{resource.spec?.name ? <CopyValue value={resource.spec.name} label="Sub Tenant name" /> : "Legacy allocation"}</MetadataListItem><MetadataListItem label="Display name">{resource.spec?.displayName || "Not set"}</MetadataListItem><MetadataListItem label="Deletion protection">{resource.spec?.deletionProtection !== false ? "Protected" : "Off"}</MetadataListItem><MetadataListItem label="ID"><CopyValue value={resource.id} label="Sub Tenant ID" /></MetadataListItem><MetadataListItem label="Tenant"><CopyValue value={resource.tenantId} label="Tenant" /></MetadataListItem><MetadataListItem label="Cluster"><CopyValue value={resource.clusterId} label="Cluster ID" /></MetadataListItem><MetadataListItem label="Admin groups">{resource.spec?.access?.adminGroups.join(", ") || "None configured"}</MetadataListItem><MetadataListItem label="Viewer groups">{resource.spec?.access?.viewerGroups.join(", ") || "None configured"}</MetadataListItem><MetadataListItem label="Status"><StatusValue label={row!.state} tone={row!.tone} /></MetadataListItem><MetadataListItem label="Message">{resource.status?.message || "No message reported"}</MetadataListItem><MetadataListItem label="Remote tenant">{resource.status?.remoteCapsuleTenant ? <CopyValue value={resource.status.remoteCapsuleTenant} label="Remote tenant" /> : "Not reported"}</MetadataListItem></MetadataList>
      <ConsoleTable aria-label="Sub Tenant namespaces" idKey="key" data={(resource.status?.namespaces ?? []).map((namespace) => ({ key: namespace.key, name: namespace.name, uid: namespace.uid, template: namespace.templateRevision }))} columns={[{ key: "key", header: "Namespace key" }, { key: "name", header: "Namespace", renderCell: (namespace) => namespace.name ? <CopyValue value={namespace.name} label="Namespace" /> : <Text>Not reported</Text> }, { key: "uid", header: "UID", renderCell: (namespace) => namespace.uid ? <CopyValue value={namespace.uid} label="Namespace UID" /> : <Text>Not reported</Text> }, ...(resource.status?.namespaces.some((namespace) => namespace.templateRevision) ? [{ key: "template", header: "Policy revision" }] : [])]} />
      {!resource.status?.namespaces.length && <Text>No namespaces have been reported yet.</Text>}
      {!!resource.status?.deletionBlockers.length && <Banner status="warning" title="Deletion blockers" description={resource.status.deletionBlockers.join("; ")} />}
      <Collapsible trigger="View Sub Tenant YAML" defaultIsOpen={false}><ValuesEditor label="Sub Tenant YAML" value={subTenantYaml(scope, resource)} readOnly /></Collapsible>
    </VStack>}
  </WizardDialog>;
}
function DeleteSubTenant({ scope, resource, onClose, onAccepted }: { scope: SubTenantScope; resource: SubTenant; onClose: () => void; onAccepted: (resource: SubTenant, action: "delete") => void }) {
  const { api, canWrite } = usePlatform();
  const [confirmation, setConfirmation] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const pending = useRef<AbortController | null>(null);
  useEffect(() => () => pending.current?.abort(), []);
  async function remove() {
    if (!canWrite || pending.current) return;
    const controller = new AbortController();
    try {
      const request = deleteSubTenantRequest(scope, resource, confirmation);
      pending.current = controller; setBusy(true); setError("");
      const response = await api.subtenant.deleteSubTenant(request, { signal: controller.signal });
      if (!controller.signal.aborted) onAccepted(scopedSubTenant(response.subtenant, scope, resource.id), "delete");
    } catch (cause) { if (!controller.signal.aborted) setError(errorMessage(cause)); }
    finally { if (!controller.signal.aborted) { pending.current = null; setBusy(false); } }
  }
  return <WizardDialog isOpen title="Delete Sub Tenant" steps={["Review deletion"]} step={0} busy={busy} onOpenChange={(open) => { if (!open && !pending.current) onClose(); }} footer={<HStack gap={2}><Button label="Cancel" isDisabled={busy} onClick={onClose} /><Button label="Confirm delete Sub Tenant" variant="destructive" isLoading={busy} isDisabled={!canWrite || resource.spec?.deletionProtection !== false || confirmation !== resource.id || resource.status?.state === SubTenantState.DELETING} onClick={() => void remove()} /></HStack>}>
    <VStack gap={4}>{resource.spec?.deletionProtection !== false && <Banner status="warning" title="Deletion protection is on" description="Disable deletion protection and save before deleting this Sub Tenant." />}<Banner status="warning" title="Only empty allocations can be deleted" description="Remove workloads and other blocking contents from every allocated namespace first. Setup and deletion checks must finish before the allocation disappears. Forced deletion is not available." /><CopyValue value={resource.id} label="Sub Tenant ID" />{!!resource.status?.deletionBlockers.length && <Banner status="warning" title="Deletion blockers" description={resource.status.deletionBlockers.join("; ")} />}{error && <Banner status="error" role="alert" title="Sub Tenant could not be deleted" description={error} />}<TextInput label="Confirm Sub Tenant ID" value={confirmation} onChange={(value) => { setConfirmation(value); setError(""); }} isDisabled={busy} autoComplete="off" /></VStack>
  </WizardDialog>;
}

function DisableProtection({ scope, resource, onClose, onAccepted }: { scope: SubTenantScope; resource: SubTenant; onClose: () => void; onAccepted: (resource: SubTenant, action: "unprotect") => void }) {
  const { api, canWrite } = usePlatform();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const pending = useRef<AbortController | null>(null);
  useEffect(() => () => pending.current?.abort(), []);
  async function apply() {
    if (!canWrite || pending.current) return;
    const controller = new AbortController(); pending.current = controller; setBusy(true); setError("");
    try {
      const updated = await disableSubTenantProtection(api, scope, resource.id, controller.signal);
      if (!controller.signal.aborted) onAccepted(updated, "unprotect");
    } catch (cause) { if (!controller.signal.aborted) setError(errorMessage(cause)); }
    finally { if (!controller.signal.aborted) { pending.current = null; setBusy(false); } }
  }
  return <WizardDialog isOpen title="Disable deletion protection" steps={["Review change"]} step={0} busy={busy} onOpenChange={(open) => { if (!open && !pending.current) onClose(); }} footer={<HStack gap={2}><Button label="Cancel" isDisabled={busy} onClick={onClose} /><Button label="Disable deletion protection" variant="destructive" isLoading={busy} isDisabled={!canWrite} onClick={() => void apply()} /></HStack>}>
    <VStack gap={4}><CopyValue value={resource.id} label="Sub Tenant ID" /><Banner status="warning" title="Allow this allocation to be deleted" description={resource.status?.state === SubTenantState.DELETING ? "Deletion is already pending. Turning off protection allows cleanup to resume when all deletion requirements are met." : "Turning off protection does not delete anything. Use Delete separately after this change is saved and the allocation is empty."} /><Text>Only deletion protection changes. Names, namespace keys, quotas, policy and access remain as stored.</Text>{error && <Banner role="alert" status="error" title="Deletion protection could not be disabled" description={error} />}</VStack>
  </WizardDialog>;
}
