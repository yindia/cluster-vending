"use client";

import { useCallback, useId, useMemo, useRef, useState } from "react";
import { toJsonString } from "@bufbuild/protobuf";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Collapsible } from "@astryxdesign/core/Collapsible";
import { HStack } from "@astryxdesign/core/HStack";
import { MetadataList, MetadataListItem } from "@astryxdesign/core/MetadataList";
import { Text } from "@astryxdesign/core/Text";
import { VStack } from "@astryxdesign/core/VStack";
import { usePlatform } from "@/components/platform-provider";
import { PlanReview } from "@/components/resources/plan-review";
import type { Cluster } from "@/gen/cloud/v1/cluster_pb";
import { UpdateClusterRequestSchema } from "@/gen/cloud/v1/cluster_service_pb";
import type { Network } from "@/gen/cloud/v1/network_pb";
import { errorMessage, useAsyncQuery } from "@/lib/api/use-query";
import {
  applyReviewedClusterPlan, bindClusterPlan, buildClusterUpdate, clusterFieldLabel, clusterForm, clusterPlanApplicable,
  clusterReviewStep, clusterUpdateFields, clusterWizardSteps, createOnlyClusterFields,
  validateClusterForm, validateClusterFormStep, type ClusterForm, type ReviewedClusterPlan,
} from "@/lib/workflows/clusters";
import { clusterFormYaml, parseClusterFormYaml } from "@/lib/workflows/cluster-yaml";
import { ClusterFields, useClusterFieldRows } from "./cluster-form-fields";
import { ClusterExtraReview } from "./cluster-form-review";
import { ErrorNotice, NetworkPlacement } from "./cluster-network-picker";
import { ClusterYamlPanel, clusterModeLabels, useClusterYamlMode } from "./cluster-yaml-panel";
import { ResourceWorkflowDialog } from "./manifest-create-dialog";

const lockedFields: readonly string[] = createOnlyClusterFields;

/** `current` is the cluster as the detail page shows it (resolved): the editor displays what the cluster runs from it. */
export function EditCluster({ clusterId, current, network, onClose, onApplied }: { network?: Network; clusterId: string; current: Cluster; onClose: () => void; onApplied: () => void }) {
  const { api, tenantId } = usePlatform();
  const load = useCallback((signal: AbortSignal) => api.cluster.getCluster({ tenantId, clusterId, asRequested: true }, { signal }), [api, tenantId, clusterId]);
  const requested = useAsyncQuery(load);
  if (requested.data?.cluster) return <ClusterEditor network={network} key={requested.data.cluster.etag} baseline={requested.data.cluster} current={current} asRequested={requested.data.asRequested} onApplied={onApplied} onClose={onClose} onReload={requested.refresh} />;
  return <ResourceWorkflowDialog isOpen onOpenChange={(open) => { if (!open) onClose(); }} title="Edit cluster" steps={clusterWizardSteps} step={0}
    footer={<HStack gap={3}><Button label="Cancel editing" variant="secondary" onClick={onClose} />{!requested.loading && <Button label="Reload editable cluster" onClick={requested.refresh} />}</HStack>}>
    <ErrorNotice message={requested.error} />
    {requested.loading ? <Text role="status">Loading cluster settings…</Text> : !requested.error && <Text>The server returned no cluster settings to edit.</Text>}
  </ResourceWorkflowDialog>;
}

function ClusterEditor({ baseline, current, network, asRequested, onApplied, onClose, onReload }: { baseline: Cluster; current: Cluster; network?: Network; asRequested: boolean; onApplied: () => void; onClose: () => void; onReload: () => void }) {
  const { api, tenantId } = usePlatform();
  const loadSubTenants = useCallback(async (signal: AbortSignal) => baseline.spec?.naas ? api.subtenant.listSubTenants({ tenantId, clusterId: baseline.id, pageSize: 1 }, { signal }) : undefined, [api, tenantId, baseline.id, baseline.spec?.naas]);
  const allocations = useAsyncQuery(loadSubTenants);
  const naasLockReason = !baseline.spec?.naas ? undefined : allocations.loading ? "Checking existing Sub Tenants before allowing NaaS to be disabled…" : allocations.error ? "Sub Tenants could not be checked. Reload before disabling NaaS." : allocations.data?.subtenants.length || allocations.data?.nextPageToken ? "Delete all Sub Tenants, including those still deleting, before disabling NaaS." : undefined;
  // The form as it opened is the diff baseline: the update mask is exactly what differs from it.
  const [opened] = useState(() => clusterForm(baseline, current));
  const [form, setForm] = useState(opened);
  const [revision, setRevision] = useState(0);
  const rows = useClusterFieldRows(form);
  const [step, setStep] = useState(0);
  const formId = useId();
  const [review, setReview] = useState<ReviewedClusterPlan>();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const locked = useRef(false);
  const inputs = useRef(0);
  const errorRef = useRef<HTMLElement>(null);
  const changed = useMemo(() => new Set<string>(clusterUpdateFields(opened, form)), [opened, form]);
  const stale = baseline.etag !== current.etag;
  function fail(cause: unknown) { setReview(undefined); setError(errorMessage(cause)); requestAnimationFrame(() => errorRef.current?.focus()); }
  function invalidate() { inputs.current++; setReview(undefined); setError(""); }
  const placement = { region: baseline.spec?.region ?? "", networkId: baseline.spec?.network?.networkId ?? "" };
  const yaml = useClusterYamlMode({
    toYaml: () => clusterFormYaml(form, placement),
    fromYaml: (text) => parseClusterFormYaml(text, { tenantId, baseline }),
    onApplied: (next) => { setForm(next); rows.reset(next); setRevision((value) => value + 1); },
    onEdit: invalidate, onError: fail,
  });
  function change<K extends keyof ClusterForm>(field: K, value: ClusterForm[K]) {
    if (lockedFields.includes(field)) return;
    if (field === "naas" && value === false && naasLockReason) return;
    invalidate();
    setForm((old) => ({ ...old, [field]: value }));
  }
  async function submit(apply: boolean) {
    if (locked.current) return;
    if (!apply && step < clusterReviewStep - 1 && yaml.mode === "guided") {
      try { rows.validate(step); validateClusterFormStep(form, step, opened); setStep(step + 1); setError(""); }
      catch (cause) { fail(cause); }
      return;
    }
    let draft = form;
    try {
      if (!apply && yaml.mode === "yaml") draft = yaml.apply();
      else if (!apply) rows.validate(0);
      if (!apply) validateClusterForm(draft, opened);
    } catch (cause) { fail(cause); return; }
    locked.current = true; setBusy(true); setError("");
    const inputRevision = inputs.current;
    try {
      if (stale) throw new Error("The cluster changed. Reload editable cluster and review a new plan.");
      if (baseline.spec?.naas && !draft.naas && naasLockReason) throw new Error(naasLockReason);
      const request = buildClusterUpdate(tenantId, baseline, draft, opened);
      if (apply) {
        if (!review) throw new Error("Review a plan first.");
        const response = await api.cluster.updateCluster(applyReviewedClusterPlan(review, request));
        if (!response.updateRecorded) throw new Error("The server did not acknowledge the update. Reload and review again.");
        onApplied();
      } else {
        setReview(undefined);
        const response = await api.cluster.updateCluster(request);
        if (!response.plan) throw new Error("The server returned no plan. Apply is blocked.");
        if (inputs.current === inputRevision) { setReview(bindClusterPlan(request, response.plan)); setStep(clusterReviewStep); }
      }
    } catch (cause) { fail(cause); }
    finally { locked.current = false; setBusy(false); }
  }
  const reviewing = step === clusterReviewStep;
  const previewing = !reviewing && (yaml.mode === "yaml" || step === clusterReviewStep - 1);
  return <ResourceWorkflowDialog isOpen onOpenChange={(open) => { if (!open && !locked.current) onClose(); }} title="Edit cluster" steps={clusterWizardSteps} step={step} busy={busy}
    method={{ label: "Edit as", value: yaml.mode, onChange: yaml.switchMode, labels: clusterModeLabels, inline: true }}
    description={`Review changes to ${baseline.name} in tenant ${tenantId} before applying.`}
    placement={<NetworkPlacement network={network} cluster={baseline} />}
    footer={<HStack gap={3} wrap="wrap">
      <Button label="Cancel editing" variant="secondary" onClick={onClose} isDisabled={busy} />
      {step > 0 && (reviewing || yaml.mode === "guided") && <Button label="Back" variant="secondary" onClick={() => { setStep(yaml.mode === "yaml" ? clusterReviewStep - 1 : step - 1); setError(""); }} isDisabled={busy} />}
      <Button label={reviewing ? "Apply reviewed plan" : previewing ? "Preview update plan" : "Next"} type="submit" form={formId} variant="primary" isLoading={busy}
        isDisabled={reviewing && (!review || !clusterPlanApplicable(review.plan) || stale)} />
    </HStack>}>
    {!asRequested && <Banner status="info" title="Original input unavailable" description="The server returned resolved defaults. Only fields you change are included in the update mask." />}
    {stale && <Banner status="warning" title="Cluster changed" description="Reload editable cluster before previewing or applying changes." />}
    {stale && <Button label="Reload editable cluster" onClick={onReload} isDisabled={busy} />}
    {error && <VStack ref={errorRef} tabIndex={-1} role="alert"><ErrorNotice message={error} /></VStack>}
    <form id={formId} method="post" aria-label="Cluster update settings" noValidate onSubmit={(event) => { event.preventDefault(); void submit(reviewing); }}>
      <VStack gap={6}>
        {!reviewing && yaml.mode === "yaml" && <ClusterYamlPanel value={yaml.text} onChange={yaml.edit} error={yaml.error} disabled={busy} editing />}
        {!reviewing && yaml.mode === "guided" && <ClusterFields key={revision} step={step} value={form} onChange={change} rows={rows} baseline={baseline} current={current} naasLockReason={naasLockReason} disabled={busy}
          networkName={network?.displayName || network?.name} region={baseline.spec?.region ?? ""} tier={network?.spec?.tier || baseline.status?.tier} />}
        {!reviewing && <Text type="supporting" data-pending-changes="">{changed.size ? `Changed: ${[...changed].map(clusterFieldLabel).join(", ")}.` : "No changes yet."}</Text>}
        {reviewing && review && <VStack gap={4}>
          <PlanReview plan={review.plan} />
          <ClusterExtraReview form={form} changed={changed} />
          {changed.has("naas") && <Banner status={form.naas ? "info" : "warning"} title={form.naas ? "Enable Namespace as a Service" : "Disable Namespace as a Service"} description={form.naas ? "Enable support for team namespaces with separate access, quotas, and policies. Setup continues after you apply this change." : "Turn off namespace tenancy controls for this cluster. Existing namespaces, workloads, and data will remain. Review the impact on teams using these controls before applying."} />}
          <MetadataList>
            <MetadataListItem label="Update mask">{review.request.updateMask?.paths.join(", ")}</MetadataListItem>
            <MetadataListItem label="Concurrency token">{baseline.etag}</MetadataListItem>
          </MetadataList>
          <Collapsible trigger="Advanced · exact reviewed request" defaultIsOpen={false}><VStack as="pre" isScrollable><Text type="code">{toJsonString(UpdateClusterRequestSchema, review.request, { prettySpaces: 2 })}</Text></VStack></Collapsible>
        </VStack>}
      </VStack>
    </form>
  </ResourceWorkflowDialog>;
}
