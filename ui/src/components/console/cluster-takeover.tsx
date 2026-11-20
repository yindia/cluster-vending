"use client";

import { ResourceActions } from "./resource-actions";
import { FilterRow } from "./filter-row";
import { useEffect, useId, useLayoutEffect, useRef, useState } from "react";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { CheckboxInput } from "@astryxdesign/core/CheckboxInput";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { Selector } from "@astryxdesign/core/Selector";
import { usePlatform } from "@/components/platform-provider";
import { ClusterState, type Cluster } from "@/gen/cloud/v1/cluster_pb";
import { Disposition } from "@/gen/cloud/v1/import_pb";
import { errorMessage } from "@/lib/api/use-query";
import { clusterTakeoverRequest, reviewClusterTakeover, type ClusterTakeoverReview } from "@/lib/workflows/cluster-takeover";
import { ConsoleTable } from "./console-table";
import { DetailSummary } from "./detail-layout";
import { WizardDialog } from "./wizard-dialog";
import { RefreshButton } from "./refresh-button";

type TakeoverProps = { cluster: Cluster; onAccepted: (cluster: Cluster) => void; onDelete: () => void };

export function ClusterActions({ cluster, ...props }: TakeoverProps) {
  const { scopeKey } = usePlatform();
  return <ClusterTakeover key={`${scopeKey}:${cluster.id}`} cluster={cluster} {...props} />;
}

function ClusterTakeover({ cluster, onAccepted, onDelete }: TakeoverProps) {
  const { api, tenantId, canWrite } = usePlatform();
  const [open, setOpen] = useState(false);
  const [step, setStep] = useState(0);
  const [review, setReview] = useState<ClusterTakeoverReview>();
  const [confirmation, setConfirmation] = useState("");
  const [acknowledge, setAcknowledge] = useState(false);
  const [skipPlatform, setSkipPlatform] = useState(false);
  const [error, setError] = useState("");
  const [search, setSearch] = useState("");
  const [kind, setKind] = useState("all");
  const [busy, setBusy] = useState<"read" | "write">();
  const controller = useRef<AbortController | null>(null);
  const writable = useRef(canWrite);
  useLayoutEffect(() => { writable.current = canWrite; }, [canWrite]);
  useEffect(() => () => controller.current?.abort(), []);
  const formId = useId();
  const cancelButton = useRef<HTMLButtonElement>(null);
  useEffect(() => { if (open) cancelButton.current?.focus(); }, [open, step]);
  function close(next: boolean) {
    if (busy === "write" || controller.current && busy !== "read") return;
    if (!next) { controller.current?.abort(); controller.current = null; setBusy(undefined); }
    setOpen(next);
  }
  async function loadReview() {
    if (controller.current || !writable.current) return;
    const request = new AbortController();
    controller.current = request;
    setOpen(true); setBusy("read"); setError(""); setReview(undefined); setStep(0); setConfirmation(""); setAcknowledge(false); setSkipPlatform(false); setSearch(""); setKind("all");
    try {
      const snapshot = await reviewClusterTakeover(api, tenantId, cluster.id, request.signal);
      if (!request.signal.aborted) setReview(snapshot);
    } catch (cause) { if (!request.signal.aborted) setError(errorMessage(cause)); }
    finally { if (controller.current === request) { controller.current = null; setBusy(undefined); } }
  }
  async function submit() {
    if (!review || !writable.current || controller.current) return;
    if (step === 0) { setStep(1); return; }
    const request = new AbortController();
    controller.current = request; setBusy("write"); setError("");
    let accepted: Cluster | undefined;
    try {
      const message = clusterTakeoverRequest(review, confirmation, acknowledge, skipPlatform);
      const current = await reviewClusterTakeover(api, tenantId, cluster.id, request.signal);
      if (current.key !== review.key) {
        setReview(current); setStep(0); setConfirmation(""); setAcknowledge(false); setSkipPlatform(false);
        throw new Error("The cluster or parent network changed. Review the current plan and confirm again.");
      }
      request.signal.throwIfAborted();
      if (!writable.current) throw new Error("Write access is required to take over a cluster.");
      accepted = (await api.cluster.takeOverCluster(message, { signal: request.signal })).cluster;
      request.signal.throwIfAborted();
      if (!accepted?.id || accepted.id !== cluster.id || accepted.tenantId !== tenantId) throw new Error("The response did not identify the accepted cluster. Refresh its status before retrying.");
    } catch (cause) { if (!request.signal.aborted) setError(errorMessage(cause)); return; }
    finally { if (controller.current === request) { controller.current = null; setBusy(undefined); } }
    if (!request.signal.aborted && accepted) { setOpen(false); onAccepted(accepted); }
  }
  const resources = review?.cluster.status?.import?.resources ?? [];
  const kinds = [...new Set([...resources.map(resource => resource.kind), ...(kind === "all" ? [] : [kind])])].sort();
  const terms = search.trim().toLowerCase().split(/\s+/).filter(Boolean);
  const rows = resources.map((resource, id) => ({ id, resource })).filter(({ resource }) => (kind === "all" || resource.kind === kind) && terms.every(term => [resource.kind, resource.name, resource.externalName, resource.detail, Disposition[resource.disposition]].join(" ").toLowerCase().includes(term)));
  const notImported = resources.filter(resource => resource.disposition === Disposition.NOT_IMPORTED);
  return <>
    <ResourceActions resource="Cluster"
      items={[
        { id: "freeze", label: cluster.status?.freeze && !cluster.status.freeze.letThrough ? "Unfreeze cluster" : "Freeze cluster",
          href: `/freeze?scope=cluster&id=${encodeURIComponent(cluster.id)}` },
        ...(cluster.status?.import ? [{ id: "takeover", label: "Take over imported cluster",
          isDisabled: !canWrite || cluster.status.state !== ClusterState.IMPORTED, onClick: () => { void loadReview(); } }] : []),
        { id: "delete", label: "Delete cluster", variant: "destructive", onClick: onDelete },
      ]} />
    <WizardDialog isOpen={open} onOpenChange={close} title="Take over imported cluster" steps={["Review import plan", "Confirm ownership"]} step={step} busy={busy === "write"} footer={<HStack gap={2} wrap="wrap">
      <Button ref={cancelButton} label="Cancel takeover" isDisabled={busy === "write"} onClick={() => close(false)} />
      {step === 1 && <Button label="Back" isDisabled={!!busy} onClick={() => { setStep(0); setError(""); }} />}
      <Button label={step === 0 ? "Next" : "Confirm cluster takeover"} form={formId} type="submit" variant="primary" isLoading={!!busy} isDisabled={!!busy || !canWrite || !review || step === 1 && (confirmation !== `take over ${review.cluster.name}` || !!notImported.length && !acknowledge)} />
    </HStack>}>
      <form id={formId} method="post" noValidate aria-label="Review cluster ownership" onSubmit={event => { event.preventDefault(); void submit(); }}><VStack gap={4}>
        {busy === "read" && <Text role="status">Loading the current cluster import plan and parent network…</Text>}
        {error && <Banner status="error" role="alert" title="Takeover could not be completed" description={error} />}
        {!review && !busy && <HStack hAlign="end" vAlign="center" width="100%"><RefreshButton label="Reload takeover review" onClick={() => { void loadReview(); }} /></HStack>}
        {review && <>
          <DetailSummary items={[{ label: "Cluster", value: review.cluster.name }, { label: "Network", value: review.network.displayName || review.network.name }, { label: "Tenant", value: tenantId }]} />
          {step === 0 ? <>
            <Text>Review every change and replacement. Stop the previous tool from managing these resources before taking ownership. CARRIED_OVER resources become platform-owned; NOT_IMPORTED resources remain outside platform management and need another owner.</Text>
            <FilterRow label="Takeover resource filters">
              <TextInput label="Search takeover resources" type="search" value={search} onChange={setSearch} hasClear />
              <Selector label="Resource kind" value={kind} onChange={setKind} options={[{ value: "all", label: "All kinds" }, ...kinds.map(value => ({ value, label: value || "Unspecified kind" }))]} />
            </FilterRow>
            <Text type="supporting">{rows.length} of {resources.length} resources. Ownership acknowledgment covers the complete plan, including filtered-out resources.</Text>
            <ConsoleTable aria-label="Resources reviewed for cluster takeover" idKey="id" data={rows} emptyState="No resources match these filters." columns={[
              { key: "kind", header: "Kind", renderCell: ({ resource }) => <Text maxLines={1} hasTruncateTooltip>{resource.kind}</Text> },
              { key: "name", header: "Resource", renderCell: ({ resource }) => <Text maxLines={1} hasTruncateTooltip>{resource.externalName || resource.name}</Text> },
              { key: "disposition", header: "Change", renderCell: ({ resource }) => <Text>{Disposition[resource.disposition]}</Text> },
              { key: "detail", header: "Details", renderCell: ({ resource }) => <Text maxLines={1} hasTruncateTooltip tabIndex={0}>{resource.detail || "No details reported"}{resource.requiresReplacement ? " · Requires replacement" : ""}</Text> },
            ]} />
            <Banner status="warning" title="Ownership changes cloud management" description="After acceptance, the controller applies the reviewed plan, updates resources and may create platform components. Readiness and convergence are asynchronous. Server admission checks access and import application again on submit." />
          </> : <>
            <TextInput label={`Type take over ${review.cluster.name} to confirm ownership`} value={confirmation} onChange={setConfirmation} isDisabled={!!busy} autoComplete="off" autoCapitalize="none" hasAutoCorrect={false} hasSpellCheck={false} />
            {!!notImported.length && <CheckboxInput label="I confirm every NOT_IMPORTED resource has another owner or was deleted, and the previous tool no longer manages anything here" value={acknowledge} onChange={setAcknowledge} isDisabled={!!busy} />}
            <CheckboxInput label="Skip platform components" value={skipPlatform} onChange={setSkipPlatform} isDisabled={!!busy} description="Choose this if the cluster already runs its own controllers. The platform will never install or adopt its platform components on this cluster. This is decided at takeover and stays." />
            <Text type="supporting">A fresh read must match this reviewed cluster etag and import plan before the final takeover request is sent.</Text>
          </>}
        </>}
      </VStack></form>
    </WizardDialog>
  </>;
}
