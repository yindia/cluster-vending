"use client";

import { useCallback, useEffect, useId, useMemo, useRef, useState } from "react";
import { toJsonString } from "@bufbuild/protobuf";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { CheckboxInput } from "@astryxdesign/core/CheckboxInput";
import { Collapsible } from "@astryxdesign/core/Collapsible";
import { DropdownMenu } from "@astryxdesign/core/DropdownMenu";
import { FormLayout } from "@astryxdesign/core/FormLayout";
import { Heading } from "@astryxdesign/core/Heading";
import { Icon } from "@astryxdesign/core/Icon";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { Link } from "@astryxdesign/core/Link";
import { MetadataList, MetadataListItem } from "@astryxdesign/core/MetadataList";
import { pixel, proportional } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { usePlatform } from "@/components/platform-provider";
import { PlanReview } from "@/components/resources/plan-review";
import type { Cluster } from "@/gen/cloud/v1/cluster_pb";
import { UpdateClusterRequestSchema } from "@/gen/cloud/v1/cluster_service_pb";
import { Architecture, type NodeGroupSpec } from "@/gen/cloud/v1/node_group_pb";
import { CapacityType, TaintEffect, TaintEffectSchema } from "@/gen/cloud/v1/node_pool_pb";
import { errorMessage, useAsyncQuery } from "@/lib/api/use-query";
import { applyReviewedClusterPlan, bindClusterPlan, clusterPlanApplicable, type ReviewedClusterPlan } from "@/lib/workflows/clusters";
import {
  applyNodeGroupChange, buildNodeGroupSpec, buildNodeGroupsUpdate, emptyNodeGroupForm, MAX_NODE_GROUPS, nodeGroupForm, nodeGroupIssues,
  nodeGroupAllowlist, nodeGroupRemoveSteps, nodeGroupRows, nodeGroupSteps, summarizeNodeGroup, validateNodeGroupStep, type NodeGroupAllowlist, type NodeGroupField, type NodeGroupForm, type NodeGroupRow,
} from "@/lib/workflows/node-groups";
import { ConsoleTable } from "./console-table";
import { StatusValue } from "./detail-layout";
import { FormEntryRows, type FormEntry } from "./form-entry-rows";
import { NodeGroupYamlButton } from "./node-group-yaml-button";
import { taintEffectOptions } from "./cluster-form-fields";
import { RefreshButton } from "./refresh-button";
import { TokenListInput } from "./token-list-input";
import { WizardDialog } from "./wizard-dialog";

// The platform administrator's allow-list comes from GetNodeGroupOptions: it feeds the suggestions and the early checks,
// and the server stays the authority (it refuses a group outside the list when the plan is previewed).
const allowListNote = "Only families your platform administrator allows are accepted.";

type Dialog = { mode: "add" } | { mode: "edit" | "remove"; name: string };

/** The platform node group is read-only; user groups are added, edited and removed as one reviewed list. */
export function ClusterNodeGroups({ cluster, onApplied, onRefresh, refreshing }: { cluster: Cluster; onApplied: (message: string) => void; onRefresh: () => void; refreshing: boolean }) {
  const { api, canWrite, tenantId } = usePlatform();
  const [dialog, setDialog] = useState<Dialog>();
  // The exact Karpenter objects the controller applies, rendered from the stored spec; they change with the cluster's etag.
  const clusterId = cluster.id, etag = cluster.etag;
  const loadManifests = useCallback(async (signal: AbortSignal) => {
    void etag;
    return (await api.cluster.listClusterNodeGroupManifests({ tenantId, clusterId }, { signal })).nodeGroups;
  }, [api, tenantId, clusterId, etag]);
  const manifests = useAsyncQuery(loadManifests);
  const specGroups = useMemo(() => cluster.spec?.nodeGroups ?? [], [cluster.spec?.nodeGroups]);
  const statusGroups = useMemo(() => cluster.status?.nodeGroups ?? [], [cluster.status?.nodeGroups]);
  const rows = useMemo(() => nodeGroupRows(specGroups, statusGroups, manifests.data), [specGroups, statusGroups, manifests.data]);
  const full = specGroups.length >= MAX_NODE_GROUPS;
  const target = dialog && dialog.mode !== "add" ? specGroups.find(group => group.name === dialog.name) : undefined;
  return <VStack gap={4} as="section" aria-label="Node groups" width="100%">
    <HStack gap={3} hAlign="between" vAlign="center" wrap="wrap" width="100%">
      <VStack gap={1}>
        <Heading level={3}>Node groups</Heading>
        <Text type="supporting">Karpenter provisions workload nodes for each group. The platform group runs platform components and cannot be changed.</Text>
      </VStack>
      <HStack gap={2} wrap="wrap" vAlign="center">
        {canWrite && <Button label="Add node group" variant="primary" onClick={() => setDialog({ mode: "add" })} isDisabled={full} tooltip={full ? `A cluster allows at most ${MAX_NODE_GROUPS} node groups.` : undefined} />}
        <RefreshButton label="Refresh node groups" onClick={() => { onRefresh(); manifests.refresh(); }} isLoading={refreshing || manifests.refreshing} />
      </HStack>
    </HStack>
    {manifests.error && <Banner status="warning" title="YAML is unavailable" description={manifests.error} />}
    {!canWrite && <Text type="supporting">Read-only access. Tenant administrator permission is required to change node groups.</Text>}
    <NodeGroupTable clusterId={clusterId} rows={rows} canWrite={canWrite} onEdit={name => setDialog({ mode: "edit", name })} onRemove={name => setDialog({ mode: "remove", name })} />
    {!!rows.length && !specGroups.length && <Text type="supporting">No node groups of your own yet. Add one to run workloads on Karpenter-managed nodes.</Text>}
    {dialog && canWrite && (dialog.mode === "add" || target) && <NodeGroupDialog key={`${dialog.mode}:${"name" in dialog ? dialog.name : ""}`} cluster={cluster} dialog={dialog} target={target} onClose={() => setDialog(undefined)}
      onApplied={message => { setDialog(undefined); onApplied(message); }} />}
  </VStack>;
}

function NodeGroupTable({ clusterId, rows, canWrite, onEdit, onRemove }: { clusterId: string; rows: NodeGroupRow[]; canWrite: boolean; onEdit: (name: string) => void; onRemove: (name: string) => void }) {
  const container = useRef<HTMLElement>(null);
  const [width, setWidth] = useState(0);
  // Optional columns follow the container width, not the viewport.
  useEffect(() => {
    const element = container.current;
    if (!element) return;
    const measure = () => setWidth(Math.floor(element.getBoundingClientRect().width));
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(element);
    return () => observer.disconnect();
  }, []);
  return <VStack ref={container} width="100%">
    <ConsoleTable aria-label="Cluster node groups" hasFullDetails idKey="name" data={rows} emptyState="Node groups will appear after the controller reports them." columns={[
      { key: "name", header: "Node group", width: proportional(2, { minWidth: 110 }), renderCell: row => <VStack gap={1}>
        <Link href={`/clusters/${encodeURIComponent(clusterId)}/node-groups/${encodeURIComponent(row.name)}`} aria-label={`Open node group ${row.name}`}>{row.name}</Link>
        {row.platformManaged && <Text type="supporting">Managed by platform</Text>}
      </VStack> },
      ...(width >= 560 ? [{ key: "capacity", header: "Capacity", width: proportional(1.5, { minWidth: 90 }), renderCell: (row: NodeGroupRow) => row.spec ? <VStack gap={1}><Text>{summarizeNodeGroup(row.spec).capacity}</Text><Text type="supporting">{summarizeNodeGroup(row.spec).architectures}</Text></VStack> : <Text color="secondary">Platform</Text> }] : []),
      ...(width >= 760 ? [{ key: "instances", header: "Instances", width: proportional(2, { minWidth: 120 }), renderCell: (row: NodeGroupRow) => row.spec ? <Text>{summarizeNodeGroup(row.spec).instances}</Text> : <Text color="secondary">Managed by platform</Text> }] : []),
      ...(width >= 960 ? [{ key: "scheduling", header: "Labels and taints", width: proportional(2, { minWidth: 140 }), renderCell: (row: NodeGroupRow) => row.spec ? <VStack gap={1}><Text type="supporting">Labels: {summarizeNodeGroup(row.spec).labels}</Text><Text type="supporting">Taints: {summarizeNodeGroup(row.spec).taints}</Text></VStack> : <Text color="secondary">Not applicable</Text> }] : []),
      ...(width >= 1100 ? [{ key: "limits", header: "Limits", width: proportional(1, { minWidth: 100 }), renderCell: (row: NodeGroupRow) => row.spec ? <Text>{summarizeNodeGroup(row.spec).limits}</Text> : <Text color="secondary">Not applicable</Text> }] : []),
      { key: "status", header: "Status", width: proportional(2, { minWidth: 120 }), renderCell: row => <VStack gap={1}>
        <StatusValue label={row.phaseLabel} tone={row.tone} />
        {row.message && <Text type="supporting">{row.message}</Text>}
      </VStack> },
      { key: "nodes", header: "Nodes", align: "end", width: proportional(0.7, { minWidth: 64 }), renderCell: row => <Text>{row.nodes === undefined ? "—" : row.nodes}</Text> },
      { key: "yaml", header: "YAML", align: "center" as const, width: pixel(120), resizable: false, renderCell: (row: NodeGroupRow) => row.manifest ? <NodeGroupYamlButton manifest={row.manifest} /> : <Text type="supporting" color="secondary">—</Text> },
      ...(canWrite ? [{ key: "actions", header: "Actions", align: "center" as const, width: pixel(76), resizable: false, renderCell: (row: NodeGroupRow) => row.editable
        ? <DropdownMenu presentation="adaptive" alignment="end" aria-label="Actions" renderTrigger={props => <Button {...props} label={`Actions for node group ${row.name}`} icon={<Icon icon="moreHorizontal" />} isIconOnly size="sm" variant="ghost" />} items={[
          { id: "edit", label: "Edit", onClick: () => onEdit(row.name) },
          { id: "remove", label: "Remove", variant: "destructive" as const, onClick: () => onRemove(row.name) },
        ]} />
        : <Text type="supporting" color="secondary">{row.platformManaged ? "Locked" : "—"}</Text> }] : []),
    ]} />
  </VStack>;
}

const effectValue = (name: string) => TaintEffectSchema.values.find(entry => entry.name === name)?.number ?? TaintEffect.UNSPECIFIED;
const effectName = (effect: TaintEffect) => TaintEffectSchema.values.find(entry => entry.number === effect)?.name ?? "TAINT_EFFECT_NO_SCHEDULE";
const toEntries = (form: NodeGroupForm) => ({
  labels: form.labels.map(({ key, value }): FormEntry => ({ id: crypto.randomUUID(), key, value, choice: "" })),
  taints: form.taints.map(({ key, value, effect }): FormEntry => ({ id: crypto.randomUUID(), key, value, choice: effectName(effect) })),
});
/** "Label 2: reason" messages from the shared validation highlight their row. */
function rowIssues(message: string | undefined, rows: FormEntry[], noun: string) {
  const match = message && new RegExp(`^${noun} (\\d+): (.*)$`).exec(message);
  const row = match ? rows[Number(match[1]) - 1] : undefined;
  return new Map(row && match ? [[row.id, match[2]]] : []);
}

function NodeGroupDialog({ cluster, dialog, target, onClose, onApplied }: { cluster: Cluster; dialog: Dialog; target?: NodeGroupSpec; onClose: () => void; onApplied: (message: string) => void }) {
  const { api, tenantId, canWrite } = usePlatform();
  // The cluster as it was when the dialog opened: the etag and list that the reviewed plan is bound to.
  const [baseline] = useState(cluster);
  const loadOptions = useCallback(async (signal: AbortSignal) => nodeGroupAllowlist(await api.cluster.getNodeGroupOptions({ tenantId }, { signal })), [api, tenantId]);
  // Unknown (still loading, or failed) means no client-side check and no suggestions: the server decides.
  const allow: NodeGroupAllowlist | undefined = useAsyncQuery(loadOptions).data;
  const removing = dialog.mode === "remove";
  const editing = dialog.mode === "edit";
  const steps = removing ? nodeGroupRemoveSteps : nodeGroupSteps;
  const reviewStep = steps.length - 1;
  const [form, setForm] = useState<NodeGroupForm>(() => target ? nodeGroupForm(target) : emptyNodeGroupForm());
  const [entries, setEntries] = useState(() => toEntries(form));
  const [step, setStep] = useState(0);
  const [showIssues, setShowIssues] = useState(false);
  const [review, setReview] = useState<ReviewedClusterPlan>();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const locked = useRef(false);
  const revision = useRef(0);
  const errorRef = useRef<HTMLElement>(null);
  const formId = useId();
  const others = (baseline.spec?.nodeGroups ?? []).filter(group => group.name !== target?.name).map(group => group.name);
  const issues = showIssues ? nodeGroupIssues(form, others, allow) : [];
  const issueFor = (field: NodeGroupField) => issues.find(issue => issue.field === field)?.message;
  const stale = baseline.etag !== cluster.etag;
  const title = removing ? `Remove node group ${dialog.name}` : editing ? `Edit node group ${dialog.name}` : "Add node group";

  function change(patch: Partial<NodeGroupForm>) {
    revision.current++;
    setReview(undefined); setError("");
    setForm(previous => ({ ...previous, ...patch }));
  }
  function changeEntries(kind: "labels" | "taints", next: FormEntry[]) {
    setEntries(previous => ({ ...previous, [kind]: next }));
    change(kind === "labels" ? { labels: next.map(({ key, value }) => ({ key, value })) } : { taints: next.map(({ key, value, choice }) => ({ key, value, effect: effectValue(choice) })) });
  }
  function toggle<T extends number>(values: readonly T[], value: T, on: boolean): T[] { return on ? [...values, value] : values.filter(item => item !== value); }
  function fail(cause: unknown) { setError(errorMessage(cause)); requestAnimationFrame(() => errorRef.current?.focus()); }
  function nextList() {
    const change = removing ? { kind: "remove" as const, name: dialog.name }
      : editing ? { kind: "update" as const, name: dialog.name, group: buildNodeGroupSpec(form, others, allow) }
        : { kind: "add" as const, group: buildNodeGroupSpec(form, others, allow) };
    return applyNodeGroupChange(baseline.spec?.nodeGroups ?? [], change);
  }
  async function submit(apply: boolean) {
    if (!canWrite || locked.current) return;
    if (!apply && !removing && step === 0) {
      try { validateNodeGroupStep(form, others, 0, allow); setShowIssues(false); setStep(1); setError(""); }
      catch (cause) { setShowIssues(true); fail(cause); }
      return;
    }
    if (!apply && removing && step === reviewStep) return;
    locked.current = true; setBusy(true); setError("");
    const inputRevision = revision.current;
    try {
      if (stale) throw new Error("The cluster changed. Close this dialog and review a new plan.");
      if (!removing && !apply) validateNodeGroupStep(form, others, 1, allow);
      const request = buildNodeGroupsUpdate(tenantId, baseline, nextList());
      if (apply) {
        if (!review) throw new Error("Review a plan first.");
        const response = await api.cluster.updateCluster(applyReviewedClusterPlan(review, request));
        if (!response.updateRecorded) throw new Error("The server did not acknowledge the update. Close this dialog and review again.");
        onApplied(removing ? `Removal of node group ${dialog.name} recorded. Karpenter drains its nodes asynchronously.` : `Node group ${form.name} ${editing ? "update" : "creation"} recorded. The controller is converging the desired state.`);
      } else {
        setReview(undefined);
        const response = await api.cluster.updateCluster(request);
        if (!response.plan) throw new Error("The server returned no plan. Apply is blocked.");
        if (revision.current === inputRevision) { setReview(bindClusterPlan(request, response.plan)); setStep(reviewStep); }
      }
    } catch (cause) { setReview(undefined); setShowIssues(true); fail(cause); }
    finally { locked.current = false; setBusy(false); }
  }
  const finalLabel = removing ? "Remove node group" : editing ? "Apply node group changes" : "Add node group";
  const spec = removing ? target : (() => { try { return buildNodeGroupSpec(form, others, allow); } catch { return undefined; } })();
  const summary = spec && summarizeNodeGroup(spec);
  const nextLabel = step === reviewStep ? finalLabel : removing || step === 1 ? "Preview plan" : "Next";
  return <WizardDialog isOpen onOpenChange={open => { if (!open && !locked.current) onClose(); }} title={title} steps={steps} step={step} busy={busy}
    description={`Review changes to ${baseline.name} in tenant ${tenantId} before applying.`}
    footer={<HStack gap={3} wrap="wrap">
      <Button label="Cancel" variant="secondary" onClick={onClose} isDisabled={busy} />
      {step > 0 && <Button label="Back" variant="secondary" onClick={() => { setStep(step - 1); setError(""); }} isDisabled={busy} />}
      <Button label={nextLabel} type="submit" form={formId} variant={removing && step === reviewStep ? "destructive" : "primary"} isLoading={busy}
        isDisabled={!canWrite || stale || (step === reviewStep && (!review || !clusterPlanApplicable(review.plan)))} />
    </HStack>}>
    {stale && <Banner status="warning" title="Cluster changed" description="Close this dialog and reopen it to review the latest node groups." />}
    {error && <VStack ref={errorRef} tabIndex={-1} role="alert"><Banner status="error" title="Request failed" description={error} /></VStack>}
    <form id={formId} method="post" aria-label="Node group settings" onSubmit={event => { event.preventDefault(); void submit(step === reviewStep && !!review); }}>
      <VStack gap={5}>
        {!removing && step === 0 && <>
          <TextInput label="Name" value={form.name} onChange={name => { if (!editing) change({ name }); }} isReadOnly={editing} isDisabled={busy} isRequired placeholder="batch"
            description={editing ? "A node group's name cannot be changed. Remove it and add a new group instead." : "1–40 lowercase letters, digits and hyphens, starting with a letter. “platform”, “default” and “system” are reserved."}
            status={issueFor("name") ? { type: "error", message: issueFor("name")! } : undefined} autoComplete="off" autoCapitalize="none" hasAutoCorrect={false} hasSpellCheck={false} />
          <TokenListInput label="Instance types" values={form.instanceTypes} onChange={instanceTypes => change({ instanceTypes })} disabled={busy} suggestions={allow?.instanceTypes}
            placeholder="m7i.large" description={`Exact instance types, for example m7i.large. Type a value and press Enter.${allow?.instanceTypes.length ? ` Allowed besides the allowed families' types: ${allow.instanceTypes.join(", ")}.` : ""}`} error={issueFor("instanceTypes")} maxEntries={20} />
          <TokenListInput label="Instance families" values={form.instanceFamilies} onChange={instanceFamilies => change({ instanceFamilies })} disabled={busy} suggestions={allow?.instanceFamilies}
            placeholder="m7i" description={`Karpenter picks any size from these families. Use families, types or both.${allow?.instanceFamilies.length ? ` ${allowListNote} Allowed: ${allow.instanceFamilies.join(", ")}.` : ""}`} error={issueFor("instanceFamilies")} maxEntries={20} />
          <VStack gap={2} role="group" aria-label="Architectures">
            <Heading level={4}>Architectures</Heading>
            <HStack gap={4} wrap="wrap">
              {([[Architecture.AMD64, "AMD64"], [Architecture.ARM64, "ARM64"]] as const).map(([value, label]) => <CheckboxInput key={value} label={label} value={form.architectures.includes(value)} isDisabled={busy}
                onChange={on => change({ architectures: toggle(form.architectures, value, on) })} />)}
            </HStack>
          </VStack>
          <VStack gap={2} role="group" aria-label="Capacity types">
            <Heading level={4}>Capacity types</Heading>
            <HStack gap={4} wrap="wrap">
              {([[CapacityType.ON_DEMAND, "On-demand"], [CapacityType.SPOT, "Spot"]] as const).map(([value, label]) => <CheckboxInput key={value} label={label} value={form.capacityTypes.includes(value)} isDisabled={busy}
                onChange={on => change({ capacityTypes: toggle(form.capacityTypes, value, on) })} />)}
            </HStack>
            {form.capacityTypes.includes(CapacityType.SPOT) && <Text type="supporting" data-spot-note="">Spot nodes use the cluster’s interruption queue, so Karpenter drains them before AWS reclaims the capacity.</Text>}
          </VStack>
        </>}
        {!removing && step === 1 && <>
          <FormEntryRows label="Node labels" rowLabel="node label" rows={entries.labels} onChange={next => changeEntries("labels", next)} disabled={busy} limit={64}
            issues={rowIssues(issueFor("labels"), entries.labels, "Label")} />
          <FormEntryRows label="Node taints" rowLabel="node taint" rows={entries.taints} onChange={next => changeEntries("taints", next)} disabled={busy} limit={50}
            select={{ label: "Effect", initial: "TAINT_EFFECT_NO_SCHEDULE", options: taintEffectOptions() }} issues={rowIssues(issueFor("taints"), entries.taints, "Taint")} />
          <VStack gap={2}>
            <Heading level={4}>Limits</Heading>
            <FormLayout direction="horizontal" data-workflow-fields="">
              <TextInput label="CPU limit (vCPU)" value={form.cpu} onChange={cpu => change({ cpu })} isDisabled={busy} inputMode="numeric" placeholder="No limit" autoComplete="off"
                status={issueFor("cpu") ? { type: "error", message: issueFor("cpu")! } : undefined} />
              <TextInput label="Memory limit (GiB)" value={form.memoryGib} onChange={memoryGib => change({ memoryGib })} isDisabled={busy} inputMode="numeric" placeholder="No limit" autoComplete="off"
                status={issueFor("memoryGib") ? { type: "error", message: issueFor("memoryGib")! } : undefined} />
            </FormLayout>
            <Text type="supporting">Optional. Karpenter stops adding nodes once the group reaches these totals.</Text>
          </VStack>
        </>}
        {removing && step === 0 && <VStack gap={3}>
          <Banner status="warning" title={`Remove ${dialog.name}?`} description="Karpenter drains and deletes every node in this group. Pods that only fit on these nodes become pending until capacity is available elsewhere." />
          {summary && <MetadataList><MetadataListItem label="Capacity">{summary.capacity}</MetadataListItem><MetadataListItem label="Instances">{summary.instances}</MetadataListItem></MetadataList>}
        </VStack>}
        {step === reviewStep && review && <VStack gap={4}>
          <PlanReview plan={review.plan} />
          {summary && <MetadataList title={<Heading level={4}>{removing ? "Node group being removed" : "Node group"}</Heading>}>
            <MetadataListItem label="Name">{removing ? dialog.name : form.name}</MetadataListItem>
            <MetadataListItem label="Capacity">{summary.capacity}</MetadataListItem>
            <MetadataListItem label="Instances">{summary.instances}</MetadataListItem>
            <MetadataListItem label="Architectures">{summary.architectures}</MetadataListItem>
            <MetadataListItem label="Labels">{summary.labels}</MetadataListItem>
            <MetadataListItem label="Taints">{summary.taints}</MetadataListItem>
            <MetadataListItem label="Limits">{summary.limits}</MetadataListItem>
          </MetadataList>}
          <MetadataList><MetadataListItem label="Update mask">spec.node_groups ({review.request.cluster?.spec?.nodeGroups.length ?? 0} groups in the new list)</MetadataListItem><MetadataListItem label="Concurrency token">{baseline.etag}</MetadataListItem></MetadataList>
          <Collapsible trigger="Advanced · exact reviewed request" defaultIsOpen={false}><VStack as="pre" isScrollable><Text type="code">{toJsonString(UpdateClusterRequestSchema, review.request, { prettySpaces: 2 })}</Text></VStack></Collapsible>
        </VStack>}
      </VStack>
    </form>
  </WizardDialog>;
}
