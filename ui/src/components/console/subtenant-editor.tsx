"use client";

import { useEffect, useId, useRef, useState } from "react";
import { toJsonString } from "@bufbuild/protobuf";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { CheckboxInput } from "@astryxdesign/core/CheckboxInput";
import { Collapsible } from "@astryxdesign/core/Collapsible";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { MetadataList, MetadataListItem } from "@astryxdesign/core/MetadataList";
import { SegmentedControl, SegmentedControlItem } from "@astryxdesign/core/SegmentedControl";
import { Text } from "@astryxdesign/core/Text";
import { TextArea } from "@astryxdesign/core/TextArea";
import { TextInput } from "@astryxdesign/core/TextInput";
import { SubTenantSpecSchema, type SubTenant } from "@/gen/cloud/v1/subtenant_pb";
import { usePlatform } from "@/components/platform-provider";
import { errorMessage } from "@/lib/api/use-query";
import { emptySubTenantDraft, SUBTENANT_POLICY, SUBTENANT_QUOTA_KEYS, validateSubTenantDraft, type SubTenantDraft } from "@/lib/workflows/subtenant-drafts";
import { applySubTenantReview, draftSubTenantSpec, reviewSubTenant, subTenantDraft, subTenantYaml, yamlSubTenantSpec, type SubTenantReview, type SubTenantScope } from "@/lib/workflows/subtenants";
import { ConsoleTable } from "./console-table";
import { CopyValue } from "./copy-value";
import { ValuesEditor } from "./values-editor";
import { WizardDialog } from "./wizard-dialog";

export function SubTenantEditor({ scope, baseline, onClose, onAccepted }: { scope: SubTenantScope; baseline?: SubTenant; onClose: () => void; onAccepted: (resource: SubTenant, action: "create" | "update") => void }) {
  const { api } = usePlatform();
  const [mode, setMode] = useState("guided");
  const [draft, setDraft] = useState(() => baseline ? subTenantDraft(baseline) : { ...emptySubTenantDraft });
  const [yaml, setYaml] = useState(() => subTenantYaml(scope, baseline));
  const [step, setStep] = useState(0);
  const [review, setReview] = useState<SubTenantReview>();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const pending = useRef<AbortController | null>(null);
  const requestId = useRef<{ fingerprint: string; id: string } | undefined>(undefined);
  const errorRef = useRef<HTMLElement>(null);
  const formId = useId();
  useEffect(() => () => { pending.current?.abort(); }, []);
  const steps = mode === "guided" ? ["Namespaces", "Quota and access", "Review"] : ["Paste YAML", "Review"];
  const last = steps.length - 1;
  function invalidate() { setReview(undefined); setError(""); }
  function change<K extends keyof SubTenantDraft>(field: K, value: SubTenantDraft[K]) { invalidate(); setDraft((previous) => ({ ...previous, [field]: value })); }
  function prepare() {
    if (pending.current) return;
    try {
      if (mode === "guided" && step === 0) { validateSubTenantDraft(draft, 0, baseline?.spec?.namespaceKeys, baseline ? baseline.spec?.name ?? "" : undefined); setStep(1); setError(""); return; }
      const spec = mode === "guided" ? draftSubTenantSpec(draft, baseline) : yamlSubTenantSpec(yaml, scope, baseline);
      const fingerprint = toJsonString(SubTenantSpecSchema, spec);
      if (!requestId.current || requestId.current.fingerprint !== fingerprint) requestId.current = { fingerprint, id: crypto.randomUUID() };
      setReview(reviewSubTenant(scope, spec, requestId.current.id, baseline));
      setStep(last); setError("");
    } catch (cause) { setError(errorMessage(cause)); requestAnimationFrame(() => errorRef.current?.focus()); }
  }
  async function apply() {
    if (!review || pending.current) return;
    const controller = new AbortController(); pending.current = controller;
    setBusy(true); setError("");
    try {
      const resource = await applySubTenantReview(api, review, scope, controller.signal);
      if (!controller.signal.aborted) onAccepted(resource, review.action);
    } catch (cause) { if (!controller.signal.aborted) setError(errorMessage(cause)); }
    finally { if (!controller.signal.aborted) { pending.current = null; setBusy(false); } }
  }
  const spec = review?.resource.spec;
  const removedGroups = [...(baseline?.spec?.access?.adminGroups.filter((group) => !spec?.access?.adminGroups.includes(group)) ?? []), ...(baseline?.spec?.access?.viewerGroups.filter((group) => !spec?.access?.viewerGroups.includes(group)) ?? [])];
  const removedAccess = baseline?.spec?.access?.bindings.filter((binding) => !spec?.access?.bindings.some((next) => next.identityRef === binding.identityRef)) ?? [];
  return <WizardDialog isOpen title={baseline ? "Edit Sub Tenant" : "Create Sub Tenant"} description={`Tenant ${scope.tenantId} · Cluster ${scope.clusterId}`} steps={steps} step={step} busy={busy} onOpenChange={(open) => { if (!open && !pending.current) onClose(); }}
    headerContent={<SegmentedControl label="Sub Tenant input method" value={mode} onChange={(value) => { if (pending.current || (value !== "guided" && value !== "yaml")) return; setMode(value); setStep(0); setReview(undefined); setError(""); }} isDisabled={busy} layout="hug"><SegmentedControlItem value="guided" label="Guided form" /><SegmentedControlItem value="yaml" label="Paste YAML" /></SegmentedControl>}
    footer={<HStack gap={2} wrap="wrap"><Button label="Cancel" isDisabled={busy} onClick={onClose} />{step > 0 && <Button label="Back" isDisabled={busy} onClick={() => { setStep(step - 1); setReview(undefined); setError(""); }} />}<Button label={step === last ? baseline ? "Apply Sub Tenant update" : "Confirm create Sub Tenant" : mode === "guided" && step === 0 ? "Next" : "Review Sub Tenant"} variant="primary" type="submit" form={formId} isLoading={busy} isDisabled={(step === last && !review)} /></HStack>}>
    <form id={formId} method="post" aria-label={baseline ? "Edit Sub Tenant configuration" : "Create Sub Tenant configuration"} noValidate onSubmit={(event) => { event.preventDefault(); if (step === last) void apply(); else prepare(); }}>
      <VStack gap={4}>
        {error && <VStack ref={errorRef} tabIndex={-1} role="alert"><Banner status="error" title="Sub Tenant request could not be completed" description={error} /></VStack>}
        {mode === "guided" && step === 0 && <>
          <TextInput label="Name" htmlName="name" value={draft.name} onChange={(value) => change("name", value)} isRequired={!baseline} isReadOnly={!!baseline} isDisabled={busy} autoComplete="off" autoCapitalize="none" hasAutoCorrect={false} hasSpellCheck={false} description={baseline ? "Permanent name. Existing allocations keep their original remote names." : "Unique within this cluster. Use 1–35 lowercase DNS characters; for example payments creates Capsule tenant payments and namespace payments-app for key app. This name cannot be changed."} />
          <CheckboxInput label="Deletion protection" htmlName="deletionProtection" value={draft.deletionProtection} onChange={(value) => change("deletionProtection", value)} isDisabled={busy} description="On by default. Turn this off and save before deleting the allocation. Turning it off does not delete anything." />
          <TextInput label="Display name" htmlName="displayName" value={draft.displayName} onChange={(value) => change("displayName", value)} isDisabled={busy} autoComplete="off" description="Optional friendly name. The server assigns the allocation ID." />
          <TextArea label="Namespace keys" htmlName="namespaceKeys" value={draft.namespaceKeys} onChange={(value) => change("namespaceKeys", value)} isRequired isDisabled={busy} autoComplete="off" hasSpellCheck={false} description="1–64 unique lowercase DNS keys, at most 27 characters each; separate with commas or newlines. Physical namespace names are assigned by the platform. Existing keys cannot be removed or renamed." />
        </>}
        {mode === "guided" && step === 1 && <>
          <TextInput label="Namespace quota" htmlName="namespaceCount" value={draft.namespaceCount} onChange={(value) => change("namespaceCount", value)} isRequired isDisabled={busy} autoComplete="off" description="1–64 namespaces; must cover every namespace key. This is a budget for the whole Sub Tenant." />
          <TextArea label="Resource quotas" htmlName="quotas" value={draft.quotas} onChange={(value) => change("quotas", value)} isRequired isDisabled={busy} autoComplete="off" hasSpellCheck={false} description="One resource=quantity per line, for example requests.cpu=2 or requests.memory=4Gi. Enter at least one quota; the API validates supported quantities." />
          <Collapsible trigger="Supported quota resources" defaultIsOpen={false}><Text>{SUBTENANT_QUOTA_KEYS.join(", ")}</Text></Collapsible>
          <TextInput label="Policy profile" value={SUBTENANT_POLICY} isReadOnly description="The supported restricted namespace policy is fixed." />
          <TextArea label="Admin groups" htmlName="adminGroups" value={draft.adminGroups} onChange={(value) => change("adminGroups", value)} isDisabled={busy} autoComplete="off" hasSpellCheck={false} description="Up to 32 exact identity-provider group names, separated by commas or newlines. No system: names or wildcards. These groups receive namespace admin access after setup and policy checks finish." />
          <TextArea label="Viewer groups" htmlName="viewerGroups" value={draft.viewerGroups} onChange={(value) => change("viewerGroups", value)} isDisabled={busy} autoComplete="off" hasSpellCheck={false} description="Up to 32 exact identity-provider group names, separated by commas or newlines. No system: names or wildcards. These groups receive read-only namespace access after setup and policy checks finish." />
          <TextArea label="Approved identity references" htmlName="identityRefs" value={draft.identityRefs} onChange={(value) => change("identityRefs", value)} isDisabled isReadOnly autoComplete="off" hasSpellCheck={false} description="Legacy identity references require a configured resolver and cannot be changed here. Existing references are preserved read-only. Use Admin groups or Viewer groups for new access." />
        </>}
        {mode === "yaml" && step === 0 && <ValuesEditor label="Sub Tenant manifest" language="yaml" value={yaml} onChange={(value) => { invalidate(); setYaml(value); }} disabled={busy} description="One Sub Tenant manifest in the current tenant and cluster. Output identity and status cannot be supplied. Maximum 64 KiB." />}
        {step === last && review && spec && <>
          <MetadataList><MetadataListItem label="Name">{spec.name || "Legacy allocation"}</MetadataListItem><MetadataListItem label="Deletion protection">{spec.deletionProtection !== false ? "Protected" : "Off"}</MetadataListItem><MetadataListItem label="Tenant"><CopyValue value={scope.tenantId} label="Tenant" /></MetadataListItem><MetadataListItem label="Cluster"><CopyValue value={scope.clusterId} label="Cluster ID" /></MetadataListItem><MetadataListItem label="Display name">{spec.displayName || "Not set"}</MetadataListItem><MetadataListItem label="Namespace keys">{spec.namespaceKeys.join(", ")}</MetadataListItem><MetadataListItem label="Namespace quota">{spec.quota?.namespaceCount}</MetadataListItem><MetadataListItem label="Policy">{spec.policyProfile}</MetadataListItem></MetadataList>
          <ConsoleTable aria-label="Reviewed Sub Tenant quotas" idKey="name" data={Object.entries(spec.quota?.hard ?? {}).map(([name, quantity]) => ({ name, quantity }))} columns={[{ key: "name", header: "Resource" }, { key: "quantity", header: "Sub Tenant budget" }]} />
          <Text>Admin groups: {spec.access?.adminGroups.join(", ") || "None configured"}.</Text>
          <Text>Viewer groups: {spec.access?.viewerGroups.join(", ") || "None configured"}.</Text>
          <Text>Legacy identity references: {spec.access?.bindings.map((binding) => `${binding.identityRef} · ${binding.roleProfile}`).join(", ") || "None requested"}.</Text>
          {!!removedGroups.length && <Banner status="warning" title="Group access will be removed or changed" description={removedGroups.join(", ")} />}
          {!!removedAccess.length && <Banner status="warning" title="Developer access will be removed" description={removedAccess.map((binding) => binding.identityRef).join(", ")} />}
          {baseline && <Banner status="warning" title="Review quota and access changes" description="Changing budgets or access can affect teams already using this allocation. Namespace keys are append-only." />}
          {!baseline && spec.name && <Text>Planned namespaces: {spec.namespaceKeys.map((key) => `${spec.name}-${key}`).join(", ")}</Text>}
          {spec.deletionProtection === false && <Banner status="warning" title="Deletion protection will be off" description="The allocation can be deleted after this change is saved and all cleanup requirements are met." />}
          <Text>Changes are saved first. Namespaces become available after setup and policy checks finish.</Text>
          <Collapsible trigger="Reviewed Sub Tenant YAML" defaultIsOpen={false}><ValuesEditor label="Reviewed Sub Tenant YAML" language="yaml" value={subTenantYaml(scope, review.resource)} readOnly /></Collapsible>
        </>}
      </VStack>
    </form>
  </WizardDialog>;
}
