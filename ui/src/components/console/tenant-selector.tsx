"use client";

import { useCallback, useEffect, useId, useRef, useState, type FormEvent, type ReactNode } from "react";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Dialog, DialogHeader } from "@astryxdesign/core/Dialog";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { Selector } from "@astryxdesign/core/Selector";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { usePlatform } from "@/components/platform-provider";
import { errorMessage, useAsyncQuery } from "@/lib/api/use-query";
import { acceptedTenantId, canCreateTenant, validTenantId } from "@/lib/workflows/tenants";

/** Remounted with the connection scope so directory and drafts cannot cross credentials. */
export function TenantSelector({ onSwitch, children }: { onSwitch: (tenantId: string) => void; children?: ReactNode }) {
  const { api, tenantId, identity, confirmedTenants, rememberCreatedTenant } = usePlatform();
  const load = useCallback((signal: AbortSignal) => api.tenant.listTenants({}, { signal }), [api]);
  const directory = useAsyncQuery(load);
  const [open, setOpen] = useState(false);
  const [draft, setDraft] = useState("");
  const [review, setReview] = useState(false);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const request = useRef<AbortController | null>(null);
  const input = useRef<HTMLInputElement>(null);
  const formId = useId();
  useEffect(() => () => { request.current?.abort(); }, []);
  const ids = [...new Set([...(directory.data?.tenants.map((tenant) => tenant.id) ?? []), ...confirmedTenants.map((tenant) => tenant.id)])].sort();
  const capability = !directory.error && !!directory.data?.canCreateTenants;
  const canOfferCreate = canCreateTenant(identity, capability);
  const target = draft.trim();
  const canSubmit = validTenantId(target) && canCreateTenant(identity, capability);

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (request.current) return;
    if (!validTenantId(target)) {
      setError("Enter a tenant ID using lowercase letters, numbers and hyphens (1–56 characters), starting and ending with a letter or number.");
      input.current?.focus();
      return;
    }
    if (!canCreateTenant(identity, capability)) {
      setError("Tenant creation is unavailable. Sign in and refresh the tenant directory.");
      return;
    }
    setError("");
    if (!review) { setReview(true); return; }
    const controller = new AbortController();
    request.current = controller;
    setBusy(true);
    try {
      const response = await api.tenant.createTenant({ tenantId: target }, { signal: controller.signal });
      if (controller.signal.aborted) return;
      const canonical = acceptedTenantId(target, response.tenant?.id);
      if (response.tenant) rememberCreatedTenant(response.tenant);
      directory.refresh();
      setOpen(false);
      onSwitch(canonical);
    } catch (cause) {
      if (!controller.signal.aborted) setError(errorMessage(cause));
    } finally {
      if (!controller.signal.aborted) { request.current = null; setBusy(false); }
    }
  }

  return <VStack gap={2} width="max-content" maxWidth="min(100%, calc(100dvw - 6rem))">
    <HStack gap={2} wrap="wrap" vAlign="center" width="100%">
      <HStack gap={2} vAlign="center">
      <Text type="label">Tenant</Text>
      <Selector label="Tenant" isLabelHidden hasSearch searchPlaceholder="Search tenants" value={tenantId || undefined} width="9rem"
        isLoading={directory.loading} placeholder="Select tenant" emptyText="No allowed tenants were returned."
        options={[
          ...(tenantId && !ids.includes(tenantId) ? [{ value: tenantId, label: tenantId, description: "Current tenant; unavailable in the directory", disabled: true }] : []),
          ...ids.map((id) => ({ value: id, label: id })),
        ]}
        onChange={(id) => { if (id !== tenantId && ids.includes(id) && !directory.error) onSwitch(id); }}
        status={directory.error ? { type: "error", message: `Tenant directory unavailable: ${directory.error}` } : undefined} />
      </HStack>
      {canOfferCreate && <Button label="Create tenant" variant="secondary" onClick={() => { setOpen(true); setReview(false); setError(""); }} />}
      {children}
      {directory.error && <Button label="Retry tenant directory" variant="ghost" onClick={directory.refresh} isLoading={directory.refreshing} />}
    </HStack>
    {!directory.loading && !directory.error && ids.length === 0 && <Text type="supporting" role="status">No allowed tenants were returned. The current connection is unchanged.</Text>}
    {!identity && <Text type="supporting">Tenant creation is unavailable until you are signed in.</Text>}
    <Dialog isOpen={open} onOpenChange={(next) => { if (!busy) setOpen(next); }} purpose="form" width="min(40rem, calc(100vw - 2rem))" maxHeight="90dvh" padding={0}>
      <VStack gap={5} padding={6}>
        <DialogHeader title={review ? "Review tenant" : "Create tenant"} subtitle="Create a cluster-scoped Tenant CR and its namespace. Cloud accounts are connected separately." onOpenChange={(next) => { if (!busy) setOpen(next); }} />
        <form id={formId} method="post" onSubmit={submit} aria-label="Create tenant" noValidate>
          <VStack gap={4}>
            {error && <Banner status="error" role="alert" title="Could not create tenant" description={error} />}
            {review ? <VStack gap={2}>
              <Text>Tenant ID: {target}</Text>
              <Text>This creates the cluster-scoped Tenant CR and its namespace, then switches your workspace to it. No cloud accounts or cloud resources will be created.</Text>
              <Text>Connect a cloud account before provisioning networks or clusters in the new tenant.</Text>
            </VStack> : <TextInput ref={input} label="Tenant ID" htmlName="tenantId" value={draft} onChange={(value) => { setDraft(value); setError(""); }}
              description="1–56 lowercase letters, numbers and hyphens; start and end with a letter or number."
              isRequired autoComplete="off" autoCapitalize="none" hasAutoCorrect={false} hasSpellCheck={false} width="100%" />}
            {!review && validTenantId(target) && !canSubmit && <Text role="status">Tenant creation is unavailable until you are signed in.</Text>}
            <HStack gap={2} wrap="wrap" hAlign="end">
              <Button label="Cancel" type="button" variant="ghost" isDisabled={busy} onClick={() => setOpen(false)} />
              {review && <Button label="Back" type="button" isDisabled={busy} onClick={() => { setReview(false); setError(""); }} />}
              <Button label={review ? "Confirm create tenant" : "Review tenant"} type="submit" variant="primary" isLoading={busy} isDisabled={review && !canSubmit} />
            </HStack>
          </VStack>
        </form>
      </VStack>
    </Dialog>
  </VStack>;
}
