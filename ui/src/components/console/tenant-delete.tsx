"use client";

import { useCallback, useEffect, useId, useRef, useState, type FormEvent } from "react";
import { Divider } from "@astryxdesign/core/Divider";
import { Heading } from "@astryxdesign/core/Heading";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Dialog, DialogHeader } from "@astryxdesign/core/Dialog";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { usePlatform } from "@/components/platform-provider";
import { useAsyncQuery } from "@/lib/api/use-query";
import { canCreateTenant, tenantDeleteConfirmed, tenantDeleteFailure } from "@/lib/workflows/tenants";

/** Deletes the tenant's registration after its ID is typed. The server refuses while the tenant still has resources. */
export function DeleteTenantDialog({ tenantId, onClose, onDeleted }: { tenantId: string; onClose: () => void; onDeleted: (tenantId: string) => void }) {
  const { api } = usePlatform();
  const [confirmation, setConfirmation] = useState("");
  const [failure, setFailure] = useState<ReturnType<typeof tenantDeleteFailure>>();
  const [busy, setBusy] = useState(false);
  const request = useRef<AbortController | null>(null);
  const formId = useId();
  useEffect(() => () => { request.current?.abort(); }, []);
  const confirmed = tenantDeleteConfirmed(tenantId, confirmation);
  const close = (open: boolean) => { if (!open && !busy) onClose(); };

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (request.current) return;
    if (!confirmed) { setFailure({ status: "error", title: "Confirmation does not match", description: `Type ${tenantId} exactly to confirm deletion.` }); return; }
    const controller = new AbortController();
    request.current = controller;
    setBusy(true); setFailure(undefined);
    try {
      await api.tenant.deleteTenant({ tenantId }, { signal: controller.signal });
      if (!controller.signal.aborted) onDeleted(tenantId);
    } catch (cause) {
      if (!controller.signal.aborted) setFailure(tenantDeleteFailure(cause));
    } finally {
      if (!controller.signal.aborted) { request.current = null; setBusy(false); }
    }
  }

  return <Dialog isOpen onOpenChange={close} purpose={busy ? "required" : "form"} width="min(40rem, calc(100vw - 2rem))" maxHeight="90dvh" padding={0}>
    <VStack gap={5} padding={6}>
      <DialogHeader title="Delete tenant" subtitle={tenantId} onOpenChange={busy ? undefined : close} />
      <form id={formId} method="post" onSubmit={submit} aria-label="Delete tenant" noValidate>
        <VStack gap={4}>
          {failure && <VStack role="alert"><Banner status={failure.status} title={failure.title} description={failure.description} /></VStack>}
          <Text>This removes the tenant&apos;s registration. The server refuses while the tenant still has cloud accounts, networks, clusters or components: delete those first.</Text>
          <Text type="supporting">The namespace tenant-{tenantId} and its contents are kept, so creating the same tenant ID again takes them back.</Text>
          <TextInput label={`Type ${tenantId} to confirm`} htmlName="confirmation" value={confirmation} onChange={(next) => { setConfirmation(next); setFailure(undefined); }} isDisabled={busy}
            autoComplete="off" autoCapitalize="none" hasAutoCorrect={false} hasSpellCheck={false} width="100%" />
          <HStack gap={2} wrap="wrap" hAlign="end">
            <Button label="Cancel deletion" type="button" variant="ghost" isDisabled={busy} onClick={onClose} />
            <Button label="Delete tenant" type="submit" variant="destructive" isLoading={busy} isDisabled={!confirmed} />
          </HStack>
        </VStack>
      </form>
    </VStack>
  </Dialog>;
}

/**
 * The Settings section that deletes the current tenant. Offered to the sessions that may manage tenants (the
 * directory's capability, signed in); the API decides. After a delete the workspace moves to another tenant.
 */
export function TenantDeleteSection({ onSwitch }: { onSwitch: (tenantId: string) => void }) {
  const { api, tenantId, identity } = usePlatform();
  const headingId = useId();
  const load = useCallback((signal: AbortSignal) => api.tenant.listTenants({}, { signal }), [api]);
  const directory = useAsyncQuery(load);
  const [deleting, setDeleting] = useState(false);
  const [deleted, setDeleted] = useState("");
  if (!tenantId || !canCreateTenant(identity, !directory.error && !!directory.data?.canCreateTenants)) return null;
  const others = (directory.data?.tenants ?? []).map((tenant) => tenant.id).filter((id) => id !== tenantId).sort();
  return <><Divider /><VStack as="section" gap={3} width="100%" aria-labelledby={headingId}>
    <Heading level={2} id={headingId}>Tenant</Heading>
    {deleted === tenantId
      ? <Text role="status">Tenant {tenantId} was deleted. Choose another tenant, or create one.</Text>
      : <>
        <Text color="secondary">Delete tenant {tenantId} once it has no cloud accounts, networks, clusters or components.</Text>
        <HStack><Button label={`Delete tenant ${tenantId}`} variant="destructive" onClick={() => setDeleting(true)} /></HStack>
      </>}
    {deleting && <DeleteTenantDialog tenantId={tenantId} onClose={() => setDeleting(false)} onDeleted={(id) => {
      setDeleting(false); setDeleted(id);
      if (others[0]) onSwitch(others[0]);
    }} />}
  </VStack></>;
}
