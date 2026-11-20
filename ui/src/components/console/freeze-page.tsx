"use client";

import { StackItem } from "@astryxdesign/core/Stack";

import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Dialog, DialogHeader } from "@astryxdesign/core/Dialog";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack } from "@astryxdesign/core/HStack";
import { Selector } from "@astryxdesign/core/Selector";
import { ConsoleTable as Table } from "./console-table";
import { DetailNavigation, useDetailSection } from "./detail-navigation";
import { useRouter } from "next/navigation";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { VStack } from "@astryxdesign/core/VStack";
import { usePlatform } from "@/components/platform-provider";
import { RefreshButton } from "./refresh-button";
import { errorMessage, useAsyncQuery } from "@/lib/api/use-query";
import { FreezePhase, FreezeTargetSchema, type FreezeTarget, type FreezeStatus } from "@/gen/cloud/v1/admin_freeze_pb";
import { CopyButton, CopyValue } from "./copy-value";
import { DetailHeader, DetailSummary, StatusValue } from "./detail-layout";

type Scope = "tenant" | "network" | "cluster";
export type FreezeSelection = { scope: Scope; id: string };
type Action = "freeze" | "unfreeze";
type Review = { action: Action; reason: string; until?: string; revision: string };
type Pending = { action: Action; revision: string; startedAt: number };
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const tenant = /^[a-z0-9]([a-z0-9-]{0,54}[a-z0-9])?$/;
const waitLimit = 15 * 60_000;

function expiryDate(value: string): Date | undefined {
  if (!value) return undefined;
  const parsed = new Date(value);
  if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/.test(value) || !Number.isFinite(parsed.getTime()) || parsed.toISOString().replace(".000Z", "Z") !== value || parsed.getTime() <= Date.now()) {
    throw new Error("Expiry must be a future UTC timestamp: YYYY-MM-DDTHH:mm:ssZ.");
  }
  return parsed;
}

function targetFor(scope: Scope, id: string): FreezeTarget {
  return create(FreezeTargetSchema, { scope: scope === "tenant" ? { case: "tenantId", value: id }
    : scope === "network" ? { case: "networkId", value: id }
    : { case: "clusterId", value: id } });
}

function selectionLabel(scope: Scope, id: string) {
  return `${scope} ${id}`;
}

export function FreezePage({ initialSelection }: { initialSelection?: FreezeSelection } = {}) {
  const { scopeKey } = usePlatform();
  return <FreezeScopes key={`${scopeKey}:${initialSelection?.scope ?? ""}:${initialSelection?.id ?? ""}`} initialSelection={initialSelection} />;
}

function FreezeScopes({ initialSelection }: { initialSelection?: FreezeSelection }) {
  const router = useRouter();
  const { tenantId } = usePlatform();
  const [scope, setScope] = useState<Scope>(initialSelection?.scope ?? "tenant");
  const selectionMatches = !initialSelection || initialSelection.scope !== "tenant" || initialSelection.id === tenantId;
  const [id, setId] = useState(initialSelection?.scope === "tenant" ? tenantId : initialSelection?.id ?? tenantId);
  const [error, setError] = useState(selectionMatches ? "" : "This link names another tenant. Only the connected tenant can be selected.");
  const [active, setActive] = useState<{ target: FreezeTarget; label: string; key: number } | undefined>(() => initialSelection && selectionMatches ? { target: targetFor(initialSelection.scope, initialSelection.id), label: selectionLabel(initialSelection.scope, initialSelection.id), key: 0 } : undefined);
  const errorRef = useRef<HTMLInputElement>(null);
  function inspect(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const value = scope === "tenant" ? tenantId : id.trim();
    if (scope === "tenant" && !tenant.test(value) || (scope === "network" || scope === "cluster") && !uuid.test(value)) {
      setError(scope === "tenant" ? "Enter a tenant DNS label (up to 56 characters)." : "Enter a valid network or cluster UUID.");
      errorRef.current?.focus();
      return;
    }
    setError("");
    if (initialSelection?.scope !== scope || initialSelection.id !== value) {
      router.push(`/freeze?scope=${scope}&id=${encodeURIComponent(value)}`, { scroll: false });
      return;
    }
    setActive({ target: targetFor(scope, value), label: selectionLabel(scope, value), key: (active?.key ?? 0) + 1 });
  }
  return <VStack gap={6}>
    <DetailHeader title="Freeze controls" description={`Pause and resume resources belonging to tenant ${tenantId}.`} />
    <form method="post" onSubmit={inspect} noValidate aria-label="Select freeze scope">
      <HStack gap={4} wrap="wrap" vAlign="end">
        <Selector label="Scope" htmlName="scope" options={[{ value: "tenant", label: "Entire tenant" }, { value: "network", label: "Network" }, { value: "cluster", label: "Cluster" }]} value={scope} onChange={value => {
          setScope(value as Scope); setId(value === "tenant" ? tenantId : ""); setActive(undefined); setError("");
        }} />
        <TextInput label="Tenant ID" value={tenantId} isReadOnly description="Bound to the connected tenant." />
        {scope !== "tenant" && <TextInput ref={errorRef} label={`${scope === "network" ? "Network" : "Cluster"} UUID`} htmlName="target" value={id} isRequired autoCapitalize="none" hasSpellCheck={false} onChange={value => { setId(value); setActive(undefined); setError(""); }} />}
        {error && <Banner status="error" title="Check scope" description={error} />}
        <Button label="Inspect selected scope" type="submit" />
      </HStack>
    </form>
    {active && <ScopedFreeze key={`${scope}:${id}:${active.key}`} target={active.target} scopeLabel={active.label} />}
  </VStack>;
}

function ScopedFreeze({ target, scopeLabel }: { target: FreezeTarget; scopeLabel: string }) {
  const { api, tenantId, canWrite } = usePlatform();
  const [action, setAction] = useState<Action>("freeze");
  const [reason, setReason] = useState("");
  const [until, setUntil] = useState("");
  const [review, setReview] = useState<Review>();
  const [confirmation, setConfirmation] = useState("");
  const [actionError, setActionError] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [pending, setPending] = useState<Pending>();
  const [confirmedOperation, setConfirmedOperation] = useState<Pending>();
  const [expired, setExpired] = useState<Pending>();
  const controller = useRef<AbortController | null>(null);
  const reasonRef = useRef<HTMLInputElement>(null);
  const confirmRef = useRef<HTMLInputElement>(null);
  const cancelRef = useRef<HTMLButtonElement>(null);
  useEffect(() => () => controller.current?.abort(), []);
  useEffect(() => {
    if (review) cancelRef.current?.focus();
  }, [review]);
  useEffect(() => {
    if (!pending) return;
    const timer = setTimeout(() => setExpired(pending), waitLimit);
    return () => clearTimeout(timer);
  }, [pending]);
  const operationComplete = useCallback((result: { response: { status?: FreezeStatus } }) => {
    const current = result.response.status;
    return !!pending && current?.revision === pending.revision
      && current?.target?.scope.case === target.scope.case && current?.target?.scope.value === target.scope.value
      && current.phase === (pending.action === "freeze" ? FreezePhase.FROZEN : FreezePhase.UNFROZEN);
  }, [pending, target]);
  const load = useCallback(async (signal: AbortSignal) => {
    const response = await api.tenant.getFreeze({ tenantId, target }, { signal });
    // Keep historical confirmation when later live observations report expiry or another policy revision.
    if (!signal.aborted && pending && operationComplete({ response })) setConfirmedOperation(pending);
    return { response, operation: pending, observedAt: Date.now() };
  }, [api, tenantId, target, pending, operationComplete]);
  const query = useAsyncQuery(load, { pollMs: 5000, observeOperation: !!pending && confirmedOperation !== pending, isComplete: operationComplete });
  // A prior query may still be visible while the loader changes. It cannot confirm a new operation.
  const observation = query.data?.operation === pending ? query.data : undefined;
  const status = observation?.response.status;
  const targetMatches = status?.target?.scope.case === target.scope.case && status?.target?.scope.value === target.scope.value;
  const revisionMatches = !!pending && status?.revision === pending.revision;
  const complete = !!pending && (confirmedOperation === pending || !!observation && operationComplete(observation));
  const timedOut = !!pending && !complete && (expired === pending || !!observation && observation.observedAt - pending.startedAt >= waitLimit);
  const observingPending = !!pending && !complete && !timedOut;
  // Resume must stay available while a freeze is converging: waiting for the
  // pause to finish would lock operators out when a controller is unavailable.
  const actionBusy = (requestedAction: Action) => submitting || observingPending && (requestedAction === "freeze" || pending?.action === "unfreeze");
  const busy = actionBusy(action) || action === "unfreeze" && !!status?.platformFrozen;
  const reviewBusy = actionBusy(review?.action ?? action) || review?.action === "unfreeze" && !!status?.platformFrozen;
  function invalidateReview() { setReview(undefined); setConfirmation(""); setActionError(""); }
  function closeConfirmation(open: boolean) {
    if (!open && !submitting && !controller.current) invalidateReview();
  }
  function prepare(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    prepareAction(action);
  }
  function prepareAction(requestedAction: Action) {
    if (!canWrite || actionBusy(requestedAction)) return;
    try {
      if (!status || !targetMatches || query.error) throw new Error("Load canonical status for this scope before reviewing an action.");
      if (requestedAction === "unfreeze" && status.platformFrozen) throw new Error("An additional platform pause applies. Tenant unfreeze cannot override it.");
      if (requestedAction === "freeze" && (!reason.trim() || reason.trim().length > 512)) {
        reasonRef.current?.focus();
        throw new Error("Enter a freeze reason (1–512 characters).");
      }
      if (requestedAction === "freeze") expiryDate(until.trim());
      setActionError(""); setConfirmation("");
      setAction(requestedAction);
      setReview({ action: requestedAction, reason: reason.trim(), until: requestedAction === "freeze" ? until.trim() : undefined, revision: status.revision });
    } catch (error) { setActionError(errorMessage(error)); }
  }
  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!review || !canWrite || actionBusy(review.action) || controller.current) return;
    if (review.action === "unfreeze" && status?.platformFrozen) { setActionError("An additional platform pause applies. Tenant unfreeze cannot override it."); return; }
    if (!status || !targetMatches || query.error || review.revision !== status.revision) {
      setReview(undefined); setConfirmation("");
      setActionError("The scope revision changed or status is unavailable. Review the action again before confirming.");
      return;
    }
    if (confirmation !== scopeLabel) {
      setActionError("Type the full scope shown in the confirmation to continue."); confirmRef.current?.focus(); return;
    }
    const request = new AbortController();
    controller.current = request;
    setSubmitting(true); setActionError("");
    try {
      const expires = review.action === "freeze" ? expiryDate(review.until ?? "") : undefined;
      const response = review.action === "freeze"
        ? await api.tenant.freeze({ tenantId, target, reason: review.reason, until: expires ? timestampFromDate(expires) : undefined }, { signal: request.signal })
        : await api.tenant.unfreeze({ tenantId, target }, { signal: request.signal });
      if (request.signal.aborted) return;
      setPending({ action: review.action, revision: response.revision, startedAt: Date.now() });
      setReview(undefined); setConfirmation("");
    } catch (error) {
      if (!request.signal.aborted) setActionError(`${errorMessage(error)} The request outcome may be unknown; inspect current status before retrying.`);
    } finally {
      if (!request.signal.aborted) { setSubmitting(false); controller.current = null; }
    }
  }
  const scopeId = String(target.scope.value ?? "");
  const tabs = ["policies", ...(status ? ["resources"] : []), ...(pending || status?.message || status?.freeze?.since ? ["activity"] : [])];
  const pages = tabs.map(value => ({ value, label: value[0].toUpperCase() + value.slice(1) }));
  const navigation = useDetailSection(pages);
  return <VStack gap={6}>
    <HStack gap={4} hAlign="end" vAlign="center" wrap="wrap" width="100%">
      <StackItem size="fill"><HStack gap={2} vAlign="center" wrap="wrap"><Heading level={2}>{scopeLabel}</Heading><CopyButton value={scopeId} label="scope ID" /></HStack></StackItem>
      <HStack gap={2} wrap="wrap" vAlign="center">
        {canWrite && <Button label="Unfreeze scope" variant="primary" aria-haspopup="dialog" isDisabled={actionBusy("unfreeze") || !status || !targetMatches || !!query.error || !!status.platformFrozen} tooltip={status?.platformFrozen ? "An additional platform pause applies and cannot be overridden by tenant controls." : undefined} onClick={() => prepareAction("unfreeze")} />}
        <RefreshButton label="Refresh freeze status" isLoading={query.refreshing} onClick={query.refresh} />
      </HStack>
    </HStack>
    {!canWrite && <Text color="secondary">Read-only access. Tenant administrator permission is required to freeze or unfreeze resources.</Text>}
    {status?.platformFrozen && <Banner status="info" title="An additional platform pause applies" description="These resources are also paused by the platform. Tenant controls cannot remove that pause." />}
    {query.loading && <Text role="status">Loading freeze status…</Text>}
    {query.error && <Banner status="error" title="Freeze status unavailable" description={query.error} />}
    {observation && !status && <Banner status="warning" title="Canonical status missing" description="The server did not return canonical freeze status. Convergence cannot be confirmed." />}
    {status && <HStack gap={3} wrap="wrap" vAlign="center"><Text type="supporting">Policy revision</Text><CopyValue value={status.revision} label="policy revision" isSecondary /></HStack>}
    {pending && <Banner status={complete ? "success" : "warning"} title={complete ? "Convergence confirmed" : timedOut ? "Observation timed out" : "Request accepted; awaiting convergence"} description={complete
      ? `The requested ${pending.action} was confirmed for this scope at revision ${pending.revision}. Current policy is shown above.`
      : timedOut ? "Convergence was not confirmed within 15 minutes. Refresh status and inspect the scope before another action."
      : `Waiting for the canonical phase at revision ${pending.revision || "(empty)"}.${status && !revisionMatches ? " The observed revision differs; this does not confirm this request." : ""}`} />}
    {pending && !complete && <Button label="Stop operation observation" onClick={() => { setPending(undefined); invalidateReview(); }} />}
    <DetailNavigation resource="Freeze" pages={pages} selection={navigation} />
    <VStack id={navigation.panelId(navigation.active)} role="tabpanel" aria-label={navigation.label} tabIndex={0} gap={5}>
    {navigation.active === "policies" && <>
    {status && <DetailSummary items={[
      { label: "Phase", value: <StatusValue label={FreezePhase[status.phase]?.toLowerCase().replace(/^./, c => c.toUpperCase()) ?? `Unknown (${status.phase})`} tone={status.phase === FreezePhase.FROZEN || status.phase === FreezePhase.FREEZING ? "warning" : "neutral"} />, note: "Current server observation" },
      { label: "Paused resources", value: `${status.pausedResources} / ${status.totalResources}`, note: `${status.newlyPausedResources} newly paused` },
      { label: "Clusters", value: status.clusters, note: "Within this scope" },
      { label: "Networks", value: status.networks, note: "Within this scope" },
    ]} />}

    {status && <VStack gap={3}>
      <Heading level={3}>Current policy</Heading>
      <Text>{status.freeze?.reason || "No freeze reason reported."}</Text>
      <Text>Expiry (UTC): {status.freeze?.until ? new Date(Number(status.freeze.until.seconds) * 1000).toISOString() : "No expiry reported"}</Text>
      {status.scopes.length ? <Table searchable aria-label="Nested freeze scopes" idKey="id" textOverflow="wrap" data={status.scopes.map((entry, id) => ({ id, entry }))} columns={[
        { key: "scope", header: "Scope", renderCell: ({ entry }) => <CopyValue value={entry.clusterId || entry.networkId || entry.tenantId || tenantId} label="scope ID" display={`${entry.clusterId ? "Cluster" : entry.networkId ? "Network" : "Tenant"} ${entry.clusterId || entry.networkId || entry.tenantId}`} /> },
        { key: "state", header: "State", renderCell: ({ entry }) => <StatusValue label={entry.frozen ? "Frozen" : "Unfrozen / exception"} tone={entry.frozen ? "warning" : "neutral"} /> },
        { key: "reason", header: "Reason", renderCell: ({ entry }) => <Text>{entry.reason || "Not reported"}</Text> },
        { key: "until", header: "Expiry (UTC)", renderCell: ({ entry }) => <Text>{entry.until ? new Date(Number(entry.until.seconds) * 1000).toISOString() : "No expiry"}</Text> },
      ]} /> : <Text>No nested freeze scopes returned.</Text>}
      {status.exceptions.length ? <VStack gap={2}><Heading level={3}>Exceptions</Heading>{status.exceptions.map((exception, index) => <CopyValue key={index} value={exception} label="exception resource" />)}</VStack> : <Text>No exceptions reported.</Text>}
    </VStack>}
    {canWrite && <>
    <Heading level={3}>Change policy</Heading>
    <Text>Unfreezing a cluster keeps its shared network’s freeze. Network unfreeze resumes that network and its clusters; tenant unfreeze applies only to this tenant.</Text>
    <form method="post" onSubmit={prepare} noValidate aria-label="Review freeze action">
      <VStack gap={4}>
        <Selector label="Action" htmlName="action" value={action} options={[{ value: "freeze", label: "Freeze" }, { value: "unfreeze", label: "Unfreeze" }]} isDisabled={submitting} onChange={value => { setAction(value as Action); invalidateReview(); }} />
        {action === "freeze" && <>
          <TextInput ref={reasonRef} label="Reason" htmlName="reason" value={reason} isRequired isDisabled={busy} onChange={value => { setReason(value); invalidateReview(); }} />
          <TextInput label="Optional expiry (UTC)" htmlName="until" description="YYYY-MM-DDTHH:mm:ssZ; leave empty for no automatic expiry." value={until} isDisabled={busy} onChange={value => { setUntil(value); invalidateReview(); }} />
        </>}
        <Button label={`Review ${action}`} type="submit" variant={action === "freeze" ? "secondary" : "primary"} aria-haspopup="dialog" isDisabled={busy} isLoading={submitting} />
      </VStack>
    </form>
    </>}
    </>}
    {navigation.active === "resources" && status && <>
      <Heading level={3}>Resource pause progress</Heading>
      <Text>{status.pausedResources} of {status.totalResources} resources paused; {status.newlyPausedResources} newly paused in this observation.</Text>
      <Text>The scope contains {status.clusters} clusters and {status.networks} networks. Individual resource identities are not included in this status.</Text>
    </>}
    {navigation.active === "activity" && <>
      <Heading level={3}>Latest observation</Heading>
      <Text>{status?.message || "No server message reported."}</Text>
      {status?.freeze?.since && <Text>Policy set (UTC): {new Date(Number(status.freeze.since.seconds) * 1000).toISOString()}</Text>}
      {status?.freeze?.by && <CopyValue value={status.freeze.by} label="policy actor" />}
      {pending && <VStack gap={2}><Text>Latest request: {pending.action}. {complete ? "Convergence confirmed." : timedOut ? "Observation timed out." : "Waiting for convergence."}</Text><CopyValue value={pending.revision} label="returned request revision" /></VStack>}
      <Text>Idle status follows Live updates; otherwise refresh manually. Submitted operations are observed every 5 seconds until completion, for up to 15 minutes. This is the latest observation, not an audit history.</Text>
    </>}
    </VStack>
    {review && <Dialog isOpen onOpenChange={closeConfirmation} purpose={submitting ? "required" : "form"} width="min(44rem, calc(100vw - 2rem))" maxHeight="90dvh" padding={6}>
      <VStack gap={5}>
        <DialogHeader title={`Confirm ${review.action}`} subtitle="Review the scope and type its full name before applying." onOpenChange={submitting ? undefined : closeConfirmation} />
        <form method="post" onSubmit={submit} noValidate aria-label="Confirm scoped freeze action">
        <VStack gap={4}>
        <Text>Target: {scopeLabel}</Text>
        <HStack gap={2} vAlign="center"><Text>Reviewed revision</Text><CopyValue value={review.revision} label="reviewed revision" /></HStack>
        {(!status || !targetMatches || query.error || status.revision !== review.revision) && <Banner status="warning" title="Confirmation expired" description="The scope revision changed or current status is unavailable. Cancel and review the action again." />}
        {review.action === "freeze" && <Text>Reason: {review.reason} · Expiry: {review.until || "No automatic expiry"}</Text>}
        <Text>{review.action === "freeze" ? "This requests a pause for the selected scope. Provider resources pause asynchronously after acceptance." : target.scope.case === "clusterId" ? "This resumes only the selected cluster. Its shared network keeps its own freeze." : target.scope.case === "networkId" ? "This resumes the network and its clusters, clearing nested freezes." : "This requests an unfreeze for the selected tenant scope."}</Text>
        <TextInput ref={confirmRef} label={`Type exactly: ${scopeLabel}`} htmlName="confirmation" value={confirmation} autoCapitalize="none" hasAutoCorrect={false} hasSpellCheck={false} isDisabled={reviewBusy} onChange={value => { setConfirmation(value); setActionError(""); }} />
        {actionError && <Banner role="alert" status="error" title="Action could not be completed" description={actionError} />}
        {submitting && <Text role="status">Submitting the request. Keep this dialog open until the API responds.</Text>}
        <HStack gap={3} wrap="wrap" hAlign="end">
          <Button ref={cancelRef} label="Cancel confirmation" type="button" isDisabled={submitting} onClick={() => closeConfirmation(false)} />
          <Button label={`Confirm ${review.action} for ${scopeLabel}`} type="submit" variant={review.action === "freeze" ? "secondary" : "primary"} isLoading={submitting} isDisabled={reviewBusy} />
        </HStack>
        </VStack>
        </form>
      </VStack>
    </Dialog>}
    {!review && actionError && <Banner role="alert" status="error" title="Action could not be completed" description={actionError} />}
  </VStack>;
}
