"use client";

import { useEffect, useRef, type ReactNode } from "react";
import { Dialog, DialogHeader } from "@astryxdesign/core/Dialog";
import { Layout, LayoutContent, LayoutFooter, VStack, HStack } from "@astryxdesign/core/Layout";
import { Heading } from "@astryxdesign/core/Text";
import { Stepper, Step } from "@astryxdesign/core/Stepper";

export type WizardDialogProps = {
  isOpen: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  description?: string;
  steps: readonly string[];
  step: number;
  children: ReactNode;
  footer?: ReactNode;
  busy?: boolean;
};

/** The caller owns the draft and validates each step before advancing. */
export function WizardDialog({ isOpen, onOpenChange, title, description, steps, step, children, footer, busy = false }: WizardDialogProps) {
  const heading = useRef<HTMLHeadingElement>(null);
  const previous = useRef(step);
  useEffect(() => {
    if (isOpen && previous.current !== step) heading.current?.focus();
    previous.current = step;
  }, [isOpen, step]);
  const changeOpen = (open: boolean) => { if (!busy) onOpenChange(open); };
  return <Dialog isOpen={isOpen} onOpenChange={changeOpen} purpose={busy ? "required" : "form"} width="min(48rem, calc(100vw - 2rem))" maxHeight="90dvh" padding={0}>
    <Layout padding={6}
      header={<DialogHeader title={title} subtitle={description} onOpenChange={busy ? undefined : changeOpen} />}
      content={<LayoutContent padding={6}><VStack gap={5} minHeight={steps.length > 1 ? "min(24rem, 50dvh)" : undefined}>
        <Stepper activeStep={step} label={`${title} progress`} density="compact">
          {steps.map((label, index) => <Step key={label} step={index} label={label} />)}
        </Stepper>
        <Heading level={3} ref={heading} tabIndex={-1}>{steps[step]}</Heading>
        {children}
      </VStack></LayoutContent>}
      footer={footer ? <LayoutFooter padding={5} hasDivider><HStack hAlign="end">{footer}</HStack></LayoutFooter> : undefined}
    />
  </Dialog>;
}
