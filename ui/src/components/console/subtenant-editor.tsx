"use client";

import { useEffect, useId, useRef, useState } from "react";
import { toJsonString } from "@bufbuild/protobuf";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
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
  const { api, canWrite } = usePlatform();
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
    if (!canWrite || pending.current) return;
    try {
      if (mode === "guided" && step === 0) { validateSubTenantDraft(draft, 0, baseline?.spec?.namespaceKeys); setStep(1); setError(""); return; }
      const spec = mode === "guided" ? draftSubTenantSpec(draft, baseline) : yamlSubTenantSpec(yaml, scope, baseline);
      const fingerprint = toJsonString(SubTenantSpecSchema, spec);
      if (!requestId.current || requestId.current.fingerprint !== fingerprint) requestId.current = { fingerprint, id: crypto.randomUUID() };
      setReview(reviewSubTenant(scope, spec, requestId.current.id, baseline));
      setStep(last); setError("");
    } catch (cause) { setError(errorMessage(cause)); requestAnimationFrame(() => errorRef.current?.focus()); }
  }
  async function apply() {
    if (!canWrite || !review || pending.current) return;
    const controller = new AbortController(); pending.current = controller;
    setBusy(true); setError("");
    try {
      const resource = await applySubTenantReview(api, review, scope, controller.signal);
      if (!controller.signal.aborted) onAccepted(resource, review.action);
    } catch (cause) { if (!controller.signal.aborted) setError(errorMessage(cause)); }
    finally { if (!controller.signal.aborted) { pending.current = null; setBusy(false); } }
  }
  const spec = review?.resource.spec;
  const removedAccess = baseline?.spec?.access?.bindings.filter((binding) => !spec?.access?.bindings.some((next) => next.identityRef === binding.identityRef)) ?? [];
  return <WizardDialog isOpen title={baseline ? "Edit SubTenant" : "Create SubTenant"} description={`Tenant ${scope.tenantId} · Cluster ${scope.clusterId}`} steps={steps} step={step} busy={busy} onOpenChange={(open) => { if (!open && !pending.current) onClose(); }}
    headerContent={<SegmentedControl label="SubTenant input method" value={mode} onChange={(value) => { if (pending.current || (value !== "guided" && value !== "yaml")) return; setMode(value); setStep(0); setReview(undefined); setError(""); }} isDisabled={busy} layout="hug"><SegmentedControlItem value="guided" label="Guided form" /><SegmentedControlItem value="yaml" label="Paste YAML" /></SegmentedControl>}
    footer={<HStack gap={2} wrap="wrap"><Button label="Cancel" isDisabled={busy} onClick={onClose} />{step > 0 && <Button label="Back" isDisabled={busy} onClick={() => { setStep(step - 1); setReview(undefined); setError(""); }} />}<Button label={step === last ? baseline ? "Apply SubTenant update" : "Confirm create SubTenant" : mode === "guided" && step === 0 ? "Next" : "Review SubTenant"} variant="primary" type="submit" form={formId} isLoading={busy} isDisabled={!canWrite || (step === last && !review)} /></HStack>}>
    <form id={formId} method="post" aria-label={baseline ? "Edit SubTenant configuration" : "Create SubTenant configuration"} noValidate onSubmit={(event) => { event.preventDefault(); if (step === last) void apply(); else prepare(); }}>
      <VStack gap={4}>
        {error && <VStack ref={errorRef} tabIndex={-1} role="alert"><Banner status="error" title="SubTenant request could not be completed" description={error} /></VStack>}
        {mode === "guided" && step === 0 && <>
          <TextInput label="Display name" htmlName="displayName" value={draft.displayName} onChange={(value) => change("displayName", value)} isDisabled={busy} autoComplete="off" description="Optional friendly name. The server assigns the allocation ID." />
          <TextArea label="Namespace keys" htmlName="namespaceKeys" value={draft.namespaceKeys} onChange={(value) => change("namespaceKeys", value)} isRequired isDisabled={busy} autoComplete="off" hasSpellCheck={false} description="1–64 unique lowercase DNS keys, at most 27 characters each; separate with commas or newlines. Physical namespace names are assigned by the platform. Existing keys cannot be removed or renamed." />
        </>}
        {mode === "guided" && step === 1 && <>
          <TextInput label="Namespace quota" htmlName="namespaceCount" value={draft.namespaceCount} onChange={(value) => change("namespaceCount", value)} isRequired isDisabled={busy} autoComplete="off" description="1–64 namespaces; must cover every namespace key. This is a budget for the whole SubTenant." />
          <TextArea label="Resource quotas" htmlName="quotas" value={draft.quotas} onChange={(value) => change("quotas", value)} isRequired isDisabled={busy} autoComplete="off" hasSpellCheck={false} description="One resource=quantity per line, for example requests.cpu=2 or requests.memory=4Gi. Enter at least one quota; the API validates supported quantities." />
          <Collapsible trigger="Supported quota resources" defaultIsOpen={false}><Text>{SUBTENANT_QUOTA_KEYS.join(", ")}</Text></Collapsible>
          <TextInput label="Policy profile" value={SUBTENANT_POLICY} isReadOnly description="The supported restricted namespace policy is fixed." />
          <TextArea label="Approved identity references" htmlName="identityRefs" value={draft.identityRefs} onChange={(value) => change("identityRefs", value)} isDisabled isReadOnly autoComplete="off" hasSpellCheck={false} description="Team access is not configured yet. Leave this empty to create the namespace allocation. Existing references are preserved read-only." />
        </>}
        {mode === "yaml" && step === 0 && <ValuesEditor label="SubTenant manifest" language="yaml" value={yaml} onChange={(value) => { invalidate(); setYaml(value); }} disabled={busy} description="One SubTenant manifest in the current tenant and cluster. Output identity and status cannot be supplied. Maximum 64 KiB." />}
        {step === last && review && spec && <>
          <MetadataList><MetadataListItem label="Tenant"><CopyValue value={scope.tenantId} label="Tenant" /></MetadataListItem><MetadataListItem label="Cluster"><CopyValue value={scope.clusterId} label="Cluster ID" /></MetadataListItem><MetadataListItem label="Display name">{spec.displayName || "Not set"}</MetadataListItem><MetadataListItem label="Namespace keys">{spec.namespaceKeys.join(", ")}</MetadataListItem><MetadataListItem label="Namespace quota">{spec.quota?.namespaceCount}</MetadataListItem><MetadataListItem label="Policy">{spec.policyProfile}</MetadataListItem></MetadataList>
          <ConsoleTable aria-label="Reviewed SubTenant quotas" idKey="name" data={Object.entries(spec.quota?.hard ?? {}).map(([name, quantity]) => ({ name, quantity }))} columns={[{ key: "name", header: "Resource" }, { key: "quantity", header: "Whole-SubTenant budget" }]} />
          <Text>Developer access: {spec.access?.bindings.map((binding) => `${binding.identityRef} · ${binding.roleProfile}`).join(", ") || "None requested"}.</Text>
          {!!removedAccess.length && <Banner status="warning" title="Developer access will be removed" description={removedAccess.map((binding) => binding.identityRef).join(", ")} />}
          {baseline && <Banner status="warning" title="Review quota and access changes" description="Changing budgets or access can affect teams already using this allocation. Namespace keys are append-only." />}
          <Text>Changes are saved first. Namespaces become available after setup and policy checks finish.</Text>
          <Collapsible trigger="Reviewed SubTenant YAML" defaultIsOpen={false}><ValuesEditor label="Reviewed SubTenant YAML" language="yaml" value={subTenantYaml(scope, review.resource)} readOnly /></Collapsible>
        </>}
      </VStack>
    </form>
  </WizardDialog>;
}
