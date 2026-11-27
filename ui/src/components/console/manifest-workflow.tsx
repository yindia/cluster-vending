"use client";

import { useEffect, useId, useRef, useState, type ReactNode } from "react";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Collapsible } from "@astryxdesign/core/Collapsible";
import { Dialog, DialogHeader } from "@astryxdesign/core/Dialog";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { Stepper, Step } from "@astryxdesign/core/Stepper";
import { Text } from "@astryxdesign/core/Text";
import { usePlatform } from "@/components/platform-provider";
import { errorMessage } from "@/lib/api/use-query";
import { errorLine, type ManifestKind } from "@/lib/workflows/manifest-yaml";
import { ManifestHelp, type ManifestMode } from "./manifest-help";
import { KubernetesIcon } from "./provider-icons";
import { ValuesEditor, type ValuesEditorHandle } from "./values-editor";

export interface ManifestWorkflowProps<R> {
  kind: ManifestKind;
  mode: ManifestMode;
  title: string;
  description: string;
  mark?: ReactNode;
  /** The manifest the editor opens with: a starter template, or the object as it is. */
  initialText: string;
  /** Puts the opening manifest back, such as "Reset to example". */
  resetLabel: string;
  reviewLabel: string;
  submitLabel: string;
  /** Banners above the editor, such as a changed object. */
  notice?: ReactNode;
  /** Blocks review, with the reason in `notice`. */
  isBlocked?: boolean;
  review: (text: string, signal: AbortSignal) => Promise<R>;
  canSubmit?: (review: R) => boolean;
  submit: (review: R, signal: AbortSignal) => Promise<void>;
  renderReview: (review: R) => ReactNode;
  onClose: () => void;
}

type Busy = "review" | "submit";
const steps = ["Edit YAML", "Review"] as const;

/**
 * One YAML flow for create, import and update: edit a manifest, review what the server will
 * be asked (nothing is written), then submit exactly that review. Errors name their line in
 * the editor; a refusal on submit returns to the editor with the server's message.
 */
export function ManifestWorkflow<R>({ kind, mode, title, description, mark, initialText, resetLabel, reviewLabel, submitLabel, notice, isBlocked = false, review, canSubmit, submit, renderReview, onClose }: ManifestWorkflowProps<R>) {
  const { tenantId, scopeKey } = usePlatform();
  const formId = useId();
  const editor = useRef<ValuesEditorHandle>(null);
  const heading = useRef<HTMLHeadingElement>(null);
  const operation = useRef<AbortController | null>(null);
  const [text, setText] = useState(initialText);
  const [reviewed, setReviewed] = useState<{ review: R; text: string }>();
  const [busy, setBusy] = useState<Busy>();
  const [problem, setProblem] = useState<{ message: string; line?: number }>();
  const step = reviewed ? 1 : 0;
  const opened = useRef(scopeKey);

  // The scope (tenant or sign-in) changed under the dialog: its draft belongs to the old one.
  useEffect(() => { if (opened.current !== scopeKey) onClose(); }, [scopeKey, onClose]);
  useEffect(() => () => operation.current?.abort(), []);
  const previousStep = useRef(step);
  useEffect(() => {
    if (previousStep.current !== step) heading.current?.focus();
    previousStep.current = step;
  }, [step]);
  useEffect(() => { if (problem && !busy && step === 0) editor.current?.focus(); }, [problem, busy, step]);

  async function run(mode: Busy, action: (signal: AbortSignal) => Promise<void>) {
    if (operation.current) return;
    const controller = new AbortController();
    operation.current = controller;
    setBusy(mode); setProblem(undefined);
    try { await action(controller.signal); }
    catch (cause) {
      if (controller.signal.aborted) return;
      // A refusal returns to the editor: the manifest is what changes it.
      setReviewed(undefined);
      setProblem({ message: errorMessage(cause), line: errorLine(cause, text) });
    } finally {
      if (operation.current === controller) { operation.current = null; setBusy(undefined); }
    }
  }
  const startReview = () => run("review", async (signal) => {
    const result = await review(text, signal);
    if (!signal.aborted) setReviewed({ review: result, text });
  });
  const startSubmit = () => {
    if (!reviewed || reviewed.text !== text) return;
    void run("submit", (signal) => submit(reviewed.review, signal));
  };
  function edit(next: string) {
    if (next === text) return;
    setText(next); setReviewed(undefined); setProblem(undefined);
  }
  function close(open: boolean) {
    if (open || busy === "submit") return;
    operation.current?.abort();
    onClose();
  }
  const submittable = !!reviewed && (canSubmit?.(reviewed.review) ?? true);
  const locked = !!busy || !tenantId;
  return <Dialog isOpen onOpenChange={close} purpose={busy === "submit" ? "required" : "form"} width="min(64rem, calc(100vw - 1.5rem))" maxHeight="92dvh" padding={0}>
    <VStack data-resource-workflow="" data-manifest-workflow={mode}>
      <VStack data-workflow-header="">
        <DialogHeader title={title} subtitle={description} onOpenChange={busy === "submit" ? undefined : close}
          startContent={<HStack data-workflow-mark="" padding={2}>{mark ?? <KubernetesIcon size="lg" />}</HStack>} />
      </VStack>
      <VStack as="aside" data-workflow-rail="" gap={5} aria-label={`${title} guidance`}>
        <Stepper activeStep={step} label={`${title} progress`} orientation="vertical" indicatorPosition="on-track">
          {steps.map((label, index) => <Step key={label} step={index} label={label} />)}
        </Stepper>
        <ManifestHelp kind={kind} mode={mode} />
      </VStack>
      <VStack data-workflow-body="" gap={5}>
        <VStack gap={1}>
          <Text type="supporting" color="secondary">Step {step + 1} of {steps.length} · {kind} manifest · tenant {tenantId || "none selected"}</Text>
          <Heading level={2} accessibilityLevel={3} ref={heading} tabIndex={-1}>{step === 0 ? steps[0] : steps[1]}</Heading>
        </VStack>
        {!tenantId && <Banner status="warning" title="Select a tenant before continuing" />}
        {notice}
        <form id={formId} method="post" aria-label={`${kind} manifest`} noValidate onSubmit={(event) => { event.preventDefault(); if (step === 0) void startReview(); else startSubmit(); }}>
          {step === 0 && <VStack gap={3}>
            <HStack gap={2} hAlign="between" vAlign="center" wrap="wrap">
              <Text color="secondary">{mode === "update" ? "The object as the server stores it. Change what you need; nothing is sent until you review and confirm." : "The same manifest the CLI takes. Nothing is sent until you review and confirm."}</Text>
              <Button type="button" label={resetLabel} variant="ghost" size="sm" isDisabled={locked || text === initialText} onClick={() => edit(initialText)} />
            </HStack>
            <ValuesEditor ref={editor} label={`${kind} manifest`} language="yaml" value={text} onChange={edit} disabled={locked} height="26rem"
              error={problem?.message} errorLine={problem?.line}
              description="At most 64 KiB. Unknown fields, custom tags and recursive aliases are refused; errors name their line. Tab moves to the next control." />
          </VStack>}
          {step === 1 && reviewed && <VStack gap={5} data-manifest-review="">
            {renderReview(reviewed.review)}
            <Collapsible trigger="Reviewed YAML manifest" defaultIsOpen={false}>
              <ValuesEditor label="Reviewed YAML manifest" language="yaml" value={reviewed.text} readOnly />
            </Collapsible>
            <Text type="supporting">Only the request this review captured is submitted. Go back to change the manifest and review again.</Text>
          </VStack>}
        </form>
      </VStack>
      <HStack as="footer" data-workflow-footer="">
        <HStack gap={2} wrap="wrap">
          <Button type="button" label="Cancel" variant="secondary" isDisabled={busy === "submit"} onClick={() => close(false)} />
          {step === 1 && <Button type="button" label="Back to YAML" variant="secondary" isDisabled={!!busy} onClick={() => { setReviewed(undefined); setProblem(undefined); }} />}
          {step === 0
            ? <Button type="submit" form={formId} label={reviewLabel} variant="primary" isDisabled={locked || isBlocked} isLoading={busy === "review"} />
            : <Button type="submit" form={formId} label={submitLabel} variant="primary" isDisabled={locked || !submittable || isBlocked} isLoading={busy === "submit"} />}
        </HStack>
      </HStack>
    </VStack>
  </Dialog>;
}

/** Warnings review found that do not block the request, such as a cluster without administrators. */
export function ReviewNotes({ notes }: { notes: readonly string[] }) {
  return notes.length ? <VStack gap={2}>{notes.map((note) => <Banner key={note} status="warning" title={note} />)}</VStack> : null;
}
