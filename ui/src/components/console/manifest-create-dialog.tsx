"use client";

import { useEffect, useLayoutEffect, useId, useRef, useState, type ReactNode } from "react";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Collapsible } from "@astryxdesign/core/Collapsible";
import { Dialog, DialogHeader } from "@astryxdesign/core/Dialog";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { Heading } from "@astryxdesign/core/Heading";
import { SegmentedControl, SegmentedControlItem } from "@astryxdesign/core/SegmentedControl";
import { Stepper, Step } from "@astryxdesign/core/Stepper";
import { Text } from "@astryxdesign/core/Text";
import { usePlatform } from "@/components/platform-provider";
import type { CloudAccount } from "@/gen/cloud/v1/tenant_service_pb";
import type { Network } from "@/gen/cloud/v1/network_pb";
import type { Cluster } from "@/gen/cloud/v1/cluster_pb";
import { errorMessage } from "@/lib/api/use-query";
import type { ManifestKind, ManifestReview } from "@/lib/workflows/manifests";
import { MetadataList, MetadataListItem } from "@astryxdesign/core/MetadataList";
import { CopyValue } from "./copy-value";
import { ValuesEditor, type ValuesEditorHandle } from "./values-editor";
import { WizardDialog, type WizardDialogProps } from "./wizard-dialog";
import { KubernetesIcon } from "./provider-icons";

/** Guided and YAML drafts share a dialog while keeping independent reviews. An `inline` method
 * keeps the wizard open in YAML mode: the body renders the YAML of the same draft itself. */
export function ResourceWorkflowDialog({ mark, method, yamlContent, placement, busy = false, ...props }: WizardDialogProps & {
  mark?: ReactNode;
  method?: { label: string; value: "guided" | "yaml"; onChange: (mode: "guided" | "yaml") => void; labels?: { guided: string; yaml: string }; inline?: boolean };
  yamlContent?: ReactNode;
  placement?: ReactNode;
}) {
  const changeOpen = (open: boolean) => { if (!busy) props.onOpenChange(open); };
  return <Dialog isOpen={props.isOpen} onOpenChange={changeOpen} purpose={busy ? "required" : "form"} width="min(60rem, calc(100vw - 1.5rem))" maxHeight="92dvh" padding={0}>
    <VStack data-resource-workflow={method ? "with-method" : "guided-only"}>
      <VStack data-workflow-header="">
        <DialogHeader title={props.title} subtitle={props.description} onOpenChange={busy ? undefined : changeOpen}
          startContent={<HStack data-workflow-mark="" padding={2}>{mark ?? <KubernetesIcon size="lg" />}</HStack>} />
      </VStack>
      {method && <VStack data-workflow-method="" gap={2}>
        <Text type="label">{method.label}</Text>
        <SegmentedControl label={method.label} value={method.value} onChange={(value) => { if (value === "guided" || value === "yaml") method.onChange(value); }} isDisabled={busy} layout="hug" size="md">
          <SegmentedControlItem value="guided" label={method.labels?.guided ?? "Guided form"} />
          <SegmentedControlItem value="yaml" label={method.labels?.yaml ?? "Paste YAML"} />
        </SegmentedControl>
      </VStack>}
      <ResourceWorkflowBody {...props} placement={placement} isOpen={props.isOpen && (!!method?.inline || method?.value !== "yaml")} />
      {yamlContent}
    </VStack>
  </Dialog>;
}

function ResourceWorkflowBody({ isOpen, title, steps, step, children, footer, placement }: WizardDialogProps & { placement?: ReactNode }) {
  const heading = useRef<HTMLHeadingElement>(null);
  const previousStep = useRef(step);
  useEffect(() => {
    if (isOpen && previousStep.current !== step) heading.current?.focus();
    previousStep.current = step;
  }, [isOpen, step]);
  if (!isOpen) return null;
  return <>
    <VStack as="aside" data-workflow-rail="" gap={5}>
      <Stepper activeStep={step} label={`${title} progress`} orientation="vertical" indicatorPosition="on-track">
        {steps.map((label, index) => <Step key={label} step={index} label={label} />)}
      </Stepper>
      {placement}
    </VStack>
    <VStack data-workflow-body="" gap={5} key={step}>
      <VStack gap={1}>
        <Text type="supporting" color="secondary">Step {step + 1} of {steps.length}</Text>
        <Heading level={2} accessibilityLevel={3} ref={heading} tabIndex={-1}>{steps[step]}</Heading>
      </VStack>
      {children}
    </VStack>
    {footer && <HStack as="footer" data-workflow-footer="">{footer}</HStack>}
  </>;
}

function ManifestDialogFrame({ embedded, ...props }: WizardDialogProps & { embedded: boolean }) {
  return embedded
    ? <ResourceWorkflowBody {...props} />
    : <WizardDialog {...props} />;
}

export type ManifestCreateDialogProps = {
  isOpen: boolean;
  onOpenChange: (open: boolean) => void;
  /** The resource workflow owns one modal and embeds this draft's reviewed workflow. */
  embedded?: boolean;
  onBusyChange?: (busy: boolean) => void;
} & (
  | { kind: "CloudAccount"; onCreated: (resource: CloudAccount) => void }
  | { kind: "Network"; onCreated: (resource: Network) => void }
  | { kind: "NetworkImport"; onCreated: (resource: Network) => void }
  | { kind: "Cluster"; onCreated: (resource: Cluster) => void }
  | { kind: "ClusterImport"; onCreated: (resource: Cluster) => void }
);

type ReviewedDraft = { review: ManifestReview; text: string; scopeKey: string };
type Operation = { controller: AbortController; mode: "example" | "review" | "write" };

function ErrorNotice({ error }: { error: string }) {
  return error ? <VStack role="alert"><Banner status="error" title="Request could not be completed" description={error} /></VStack> : null;
}

export function ManifestCreateDialog(props: ManifestCreateDialogProps) {
  const { isOpen, kind, onOpenChange, embedded = false, onBusyChange } = props;
  const { api, tenantId, scopeKey } = usePlatform();
  const formId = useId();
  const editor = useRef<ValuesEditorHandle>(null);
  const [text, setText] = useState("");
  const [reviewed, setReviewed] = useState<ReviewedDraft>();
  const [step, setStep] = useState(0);
  const [busy, setBusy] = useState<Operation["mode"]>();
  const [error, setError] = useState("");
  const [validationError, setValidationError] = useState("");
  const operation = useRef<Operation | null>(null);
  const requestId = useRef<{ text: string; kind: ManifestKind; tenantId: string; scopeKey: string; id: string } | null>(null);
  const latest = useRef({ props, api, tenantId, scopeKey });
  useLayoutEffect(() => { latest.current = { props, api, tenantId, scopeKey }; }, [props, api, tenantId, scopeKey]);
  const previousScope = useRef({ scopeKey, tenantId, kind });
  useEffect(() => { onBusyChange?.(!!busy); }, [busy, onBusyChange]);

  function cancel() {
    operation.current?.controller.abort();
    operation.current = null;
    setBusy(undefined);
  }

  useEffect(() => {
    let disposed = false;
    const previous = previousScope.current;
    previousScope.current = { scopeKey, tenantId, kind };
    if (previous.scopeKey !== scopeKey || previous.tenantId !== tenantId || previous.kind !== kind) {
      operation.current?.controller.abort();
      operation.current = null;
      requestId.current = null;
      queueMicrotask(() => {
        if (disposed) return;
        setBusy(undefined); setReviewed(undefined); setStep(0); setError(""); setValidationError("");
        latest.current.props.onOpenChange(false);
      });
    }
    return () => {
      disposed = true;
      operation.current?.controller.abort();
      operation.current = null;
    };
  }, [scopeKey, tenantId, kind]);

  useEffect(() => {
    let disposed = false;
    if (!isOpen) {
      operation.current?.controller.abort();
      operation.current = null;
      queueMicrotask(() => {
        if (disposed) return;
        setBusy(undefined); setReviewed(undefined); setStep(0); setError(""); setValidationError("");
      });
    }
    return () => { disposed = true; };
  }, [isOpen]);

  // Check scope again after every await, including the lazy module import.
  function isCurrent(active: Operation) {
    const current = latest.current;
    return operation.current === active && !active.controller.signal.aborted
      && current.props.isOpen && current.props.kind === kind
      && current.scopeKey === scopeKey && current.tenantId === tenantId && current.api === api;
  }

  function start(mode: Operation["mode"]) {
    if (operation.current || !isOpen || !tenantId) return;
    const active: Operation = { controller: new AbortController(), mode };
    operation.current = active; // Synchronous guard against double clicks and submits.
    setBusy(mode);
    setError("");
    setValidationError("");
    return active;
  }

  function finish(active: Operation) {
    if (operation.current === active) {
      operation.current = null;
      setBusy(undefined);
    }
  }

  function changeText(value: string) {
    if (value === text) return;
    setText(value);
    setReviewed(undefined);
    requestId.current = null;
    setError("");
    setValidationError("");
    setStep(0);
  }

  async function loadExample() {
    const active = start("example");
    if (!active) return;
    try {
      const { manifestTemplate } = await import("@/lib/workflows/manifests");
      if (isCurrent(active)) changeText(manifestTemplate(kind, tenantId));
    } catch (cause) {
      if (isCurrent(active)) setError(errorMessage(cause));
    } finally { finish(active); }
  }

  async function reviewDraft() {
    const active = start("review");
    if (!active) return;
    try {
      const saved = requestId.current;
      if (!saved || saved.text !== text || saved.kind !== kind || saved.tenantId !== tenantId || saved.scopeKey !== scopeKey) {
        requestId.current = { text, kind, tenantId, scopeKey, id: crypto.randomUUID() };
      }
      const id = requestId.current!.id;
      const { reviewManifest } = await import("@/lib/workflows/manifests");
      if (!isCurrent(active)) return;
      const review = await reviewManifest(api, text, kind, tenantId, id, active.controller.signal);
      if (!isCurrent(active)) return;
      setReviewed({ review, text, scopeKey });
      setStep(1);
    } catch (cause) {
      if (isCurrent(active)) {
        setReviewed(undefined);
        setValidationError(errorMessage(cause));
      }
    } finally { finish(active); }
  }

  // Focus after the editor has received its error and has been re-enabled.
  useEffect(() => {
    if (isOpen && validationError && !busy) editor.current?.focus();
  }, [isOpen, validationError, busy]);

  async function createReviewed() {
    if (step !== 1 || !reviewed || reviewed.text !== text || reviewed.scopeKey !== scopeKey
      || reviewed.review.kind !== kind || reviewed.review.tenantId !== tenantId) return;
    const active = start("write");
    if (!active) return;
    let resource: CloudAccount | Network | Cluster;
    try {
      const { applyManifest } = await import("@/lib/workflows/manifests");
      if (!isCurrent(active)) return;
      resource = await applyManifest(api, reviewed.review, active.controller.signal);
      if (!isCurrent(active)) return;
    } catch (cause) {
      if (isCurrent(active)) setError(errorMessage(cause));
      return;
    } finally { finish(active); }
    setReviewed(undefined);
    setStep(0);
    requestId.current = null;
    onOpenChange(false);
    // The reviewed kind fixes the generated resource type returned by applyManifest.
    if (props.kind === "CloudAccount") props.onCreated(resource as CloudAccount);
    else if (props.kind === "Network" || props.kind === "NetworkImport") props.onCreated(resource as Network);
    else props.onCreated(resource as Cluster);
  }

  function changeOpen(open: boolean) {
    if (operation.current?.mode === "write") return;
    if (!open) {
      cancel();
      setReviewed(undefined);
      setStep(0);
    }
    onOpenChange(open);
  }

  const account = kind === "CloudAccount";
  const importing = kind === "NetworkImport" || kind === "ClusterImport";
  const resourceLabel = account ? "cloud account" : kind.replace("Import", "").toLowerCase();
  const locked = !!busy || !tenantId;
  return <ManifestDialogFrame
    embedded={embedded}
    isOpen={isOpen} onOpenChange={changeOpen}
    title={account ? "Connect cloud account from YAML" : `${importing ? "Import" : "Create"} ${resourceLabel} from YAML`}
    description={`Active tenant: ${tenantId || "None selected"}`}
    steps={["Paste YAML", importing ? "Review import" : "Review creation"]} step={step} busy={busy === "write"}
    footer={<HStack gap={2} wrap="wrap">
      <Button type="button" label="Cancel" isDisabled={busy === "write"} onClick={() => changeOpen(false)} />
      {step === 1 && <Button type="button" label="Back" isDisabled={!!busy} onClick={() => { setStep(0); setError(""); }} />}
      {step === 0
        ? <Button type="submit" form={formId} label="Review" variant="primary" isDisabled={locked} isLoading={busy === "review"} />
        : <Button type="button" label={account ? "Connect account" : `${importing ? "Import" : "Create"} ${resourceLabel}`} variant="primary" isDisabled={locked || !reviewed} isLoading={busy === "write"} onClick={() => { void createReviewed(); }} />}
    </HStack>}
  >
    <form id={formId} method="post" aria-label={`${kind} YAML manifest`} noValidate onSubmit={(event) => { event.preventDefault(); if (step === 0) void reviewDraft(); }}>
      <VStack gap={5}>
        {!tenantId && <Banner status="warning" title="Select a tenant before continuing" />}
        {step === 0 && <>
          {!embedded && <Text color="secondary">Paste one {kind} manifest. Review checks the manifest and its referenced accounts and networks. Nothing is submitted until you confirm.</Text>}
          <HStack gap={2} wrap="wrap" hAlign={embedded ? "between" : "start"} vAlign="center">
            {embedded && <Text color="secondary">Use the same manifest as the CLI.</Text>}
            <Button type="button" label={text ? "Replace with example" : "Load example"} isDisabled={locked} isLoading={busy === "example"} onClick={() => { void loadExample(); }} />
          </HStack>
          <ValuesEditor ref={editor} label={`${kind} manifest`} language="yaml" value={text} onChange={changeText} disabled={locked} error={validationError || undefined} description={embedded ? "Maximum 64 KiB. Replace example resource IDs before review." : "Required. Paste YAML using apiVersion: cluster-vending.io/v1 and the selected kind. Maximum size: 64 KiB. Example reference IDs must be replaced."} />
        </>}
        {step === 1 && reviewed && <>
          <MetadataList columns="multi">{reviewed.review.summary.map(item => <MetadataListItem key={item.label} label={item.label}>{item.label.endsWith("ID") ? <CopyValue value={item.value} label={item.label} /> : item.value}</MetadataListItem>)}</MetadataList>
          <Banner status="info" title={importing ? "Import stays read-only" : "Ready for review"} description={importing ? "This records discovery of existing cloud resources. It does not create or take ownership of cloud infrastructure. Review discovery on the detail page, then explicitly take over when ready." : "Check the configuration before submitting. The platform performs final validation when you confirm. Provisioning and access verification continue after the request is accepted."} />
          {account && <Banner status="info" title="Connects an existing cloud account" description="This updates the connection if the cloud account identifier already exists. It does not create a cloud account." />}
          <Collapsible trigger="Reviewed YAML manifest" defaultIsOpen={false}>
            <ValuesEditor label="Reviewed YAML manifest" language="yaml" value={reviewed.text} readOnly />
          </Collapsible>
          <Text type="supporting">Only the exact request captured by this review will be submitted. Go Back to edit and review again.</Text>
        </>}
        <ErrorNotice error={error} />
      </VStack>
    </form>
  </ManifestDialogFrame>;
}
