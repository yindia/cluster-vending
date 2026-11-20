"use client";

import { FilterRow } from "./filter-row";
import { StackItem } from "@astryxdesign/core/Stack";

import { useCallback, useEffect, useId, useRef, useState, type ReactNode } from "react";
import Link from "next/link";
import { Link as AstryxLink } from "@astryxdesign/core/Link";
import { create } from "@bufbuild/protobuf";
import type { Timestamp } from "@bufbuild/protobuf/wkt";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Collapsible } from "@astryxdesign/core/Collapsible";
import { Dialog, DialogHeader } from "@astryxdesign/core/Dialog";
import { Divider } from "@astryxdesign/core/Divider";
import { FormLayout } from "@astryxdesign/core/FormLayout";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack } from "@astryxdesign/core/HStack";
import { Pagination } from "@astryxdesign/core/Pagination";
import { Selector } from "@astryxdesign/core/Selector";
import { TabList, Tab } from "@astryxdesign/core/TabList";
import type { TableColumn } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { VStack } from "@astryxdesign/core/VStack";
import { usePlatform } from "@/components/platform-provider";
import { useAsyncQuery, errorMessage } from "@/lib/api/use-query";
import { providerRegistry } from "@/lib/providers/registry";
import { ConditionStatus } from "@/gen/cloud/v1/conditions_pb";
import { ManagedResourceState, type ManagedResource } from "@/gen/cloud/v1/managed_resource_pb";
import { CloudAccountSchema, CloudAccountState, type AccessResult, type CloudAccount } from "@/gen/cloud/v1/tenant_service_pb";
import { NetworkState, type Network } from "@/gen/cloud/v1/network_pb";
import { ClusterState, ClusterTier, ReleaseChannel, type Cluster } from "@/gen/cloud/v1/cluster_pb";
import { Tier } from "@/gen/cloud/v1/tier_pb";
import { WizardDialog } from "./wizard-dialog";
import { ManifestCreateDialog } from "./manifest-create-dialog";
import { ConsoleTable as Table } from "./console-table";
import { DetailHeader, DetailSummary, StatusValue } from "./detail-layout";
import { CopyButton, CopyValue } from "./copy-value";
import { ResourceYamlButton } from "./resource-yaml-button";
import { RefreshButton } from "./refresh-button";
import { ProviderIcon } from "./provider-icons";
import {
  ACCOUNT_PAGE_SIZE, ACCOUNT_POLL_MS, ACCOUNT_POLL_WINDOW_MS,
  accessAttemptKey, accountBootstrapFields, accountPage, accountProgressing, accountStateLabel,
  accountStepErrors, bootstrapAccount, deleteAccount, loadAccountRelations, onboardingRequest,
  parseVerifyRegions, validateAccount, verifyAccount, type AccountBootstrapFields,
} from "@/lib/workflows/accounts";

function useAccountPolling(active: boolean, refresh: () => void, revision: number, observeOperation = false) {
  const { liveUpdates } = usePlatform();
  useEffect(() => {
    if (!active || (!liveUpdates && !observeOperation)) return;
    let ticks = 0;
    const timer = setInterval(() => {
      refresh();
      if (++ticks >= ACCOUNT_POLL_WINDOW_MS / ACCOUNT_POLL_MS) clearInterval(timer);
    }, ACCOUNT_POLL_MS);
    return () => clearInterval(timer);
  }, [active, refresh, revision, liveUpdates, observeOperation]);
}

/** Cancel transport and suppress every completion callback after the scope unmounts. */
function useAccountMutation() {
  const controller = useRef<AbortController | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  useEffect(() => () => controller.current?.abort(), []);
  async function run<T>(action: (signal: AbortSignal) => Promise<T>, accepted: (result: T) => void) {
    if (controller.current && !controller.current.signal.aborted) return;
    const request = new AbortController();
    controller.current = request;
    setBusy(true);
    setError("");
    try {
      const result = await action(request.signal);
      if (!request.signal.aborted) accepted(result);
    } catch (cause) {
      if (!request.signal.aborted) setError(errorMessage(cause));
    } finally {
      if (!request.signal.aborted) { controller.current = null; setBusy(false); }
    }
  }
  return { busy, error, run, clearError: () => setError("") };
}

function ErrorNotice({ error }: { error: string }) {
  return error ? <VStack role="alert"><Banner status="error" title="Request could not be completed" description={error} /></VStack> : null;
}
function Value({ label, children }: { label: string; children: ReactNode }) {
  const content = children === undefined || children === null || children === "" ? "Not reported" : children;
  return <VStack gap={1}><Text type="supporting" color="secondary">{label}</Text>{typeof content === "string" || typeof content === "number" ? <Text>{content}</Text> : content}</VStack>;
}
function timestampText(time: Timestamp | undefined) {
  return time ? new Date(Number(time.seconds) * 1_000 + Math.floor(time.nanos / 1_000_000)).toLocaleString() : "Not reported";
}
function titleCase(value: string) { return value.toLowerCase().replaceAll("_", " ").replace(/^./, (letter) => letter.toUpperCase()); }
function accountName(account: CloudAccount) { return account.accountName || (account.provider.case === "aws" ? `AWS account ${account.provider.value.accountId}` : "Cloud account"); }
function AccessStatus({ account }: { account: CloudAccount }) {
  const access = account.status?.access;
  const label = !access ? "Not checked" : access.stale ? "Check stale" : access.verified ? "Verified" : "Not verified";
  return <StatusValue label={label} tone={!access ? "neutral" : access.stale ? "warning" : access.verified ? "success" : "error"} />;
}

function ConnectAccount({ account, onAccepted }: { account?: CloudAccount; onAccepted: (account: CloudAccount) => void }) {
  const { api, tenantId, canWrite } = usePlatform();
  const id = useId();
  const form = useRef<HTMLFormElement>(null);
  const [open, setOpen] = useState(false);
  const [createMode, setCreateMode] = useState<"guided" | "yaml">("guided");
  const [step, setStep] = useState(0);
  const [fields, setFields] = useState<AccountBootstrapFields>(() => accountBootstrapFields(account));
  const [errors, setErrors] = useState<ReturnType<typeof validateAccount>>({});
  const mutation = useAccountMutation();
  const disabled = !tenantId || !canWrite || mutation.busy;
  function change(field: keyof AccountBootstrapFields, value: string) {
    const next = { ...fields, [field]: value };
    setFields(next);
    if (!validateAccount(next)[field]) setErrors((previous) => ({ ...previous, [field]: undefined }));
  }
  function submit() {
    if (disabled) return;
    const nextErrors = accountStepErrors(fields, step);
    setErrors(nextErrors);
    const firstError = Object.keys(nextErrors)[0];
    if (firstError) {
      const input = form.current?.elements.namedItem(firstError);
      if (input instanceof HTMLElement) input.focus();
      return;
    }
    if (step < 2) { setStep(step + 1); return; }
    void mutation.run((signal) => bootstrapAccount(api, tenantId, fields, signal), (saved) => { setOpen(false); onAccepted(saved); });
  }
  const status = (field: keyof AccountBootstrapFields) => errors[field] ? { type: "error" as const, message: errors[field] } : undefined;
  return <>
    <Button label={account ? "Edit account" : "Connect account"} variant={account ? "secondary" : "primary"} isDisabled={disabled || !!account && account.provider.case !== "aws"} onClick={() => { setFields(accountBootstrapFields(account)); setCreateMode("guided"); setStep(0); setErrors({}); mutation.clearError(); setOpen(true); }} />
    <WizardDialog isOpen={open && createMode === "guided"} onOpenChange={(next) => { if (!mutation.busy) setOpen(next); }} title={account ? "Edit account" : "Connect cloud account"} description="Give the platform access to an existing cloud account. No cloud credentials are collected." steps={["Account identity", "AWS access", "Review connection"]} step={step} busy={mutation.busy}
      footer={<HStack gap={2} justify="between" wrap="wrap">
        <Button label="Cancel" isDisabled={mutation.busy} onClick={() => setOpen(false)} />
        <HStack gap={2}>
          {step > 0 && <Button label="Back" isDisabled={mutation.busy} onClick={() => setStep(step - 1)} />}
          <Button type="submit" form={id} label={step < 2 ? "Next" : account ? "Save account" : "Connect account"} variant="primary" isDisabled={disabled} isLoading={mutation.busy} />
        </HStack>
      </HStack>}>
      <form id={id} ref={form} method="post" aria-label={account ? "Edit account" : "Connect cloud account"} noValidate onSubmit={(event) => { event.preventDefault(); submit(); }}>
        <VStack gap={5}>
          {step === 0 && <>
            {!account && <VStack gap={2}>
              <Text type="label">Connection method</Text>
              <HStack gap={2} wrap="wrap" vAlign="center">
                <Button type="button" label="Use guided form" variant="primary" aria-pressed={true} isDisabled={disabled} onClick={() => setCreateMode("guided")} />
                <Button type="button" label="Paste YAML" isDisabled={disabled} onClick={() => setCreateMode("yaml")} />
              </HStack>
              <Text type="supporting" color="secondary">Use the form or paste the CloudAccount YAML used by the CLI. Both connect an existing cloud account.</Text>
            </VStack>}
            <Text color="secondary">Choose a provider and a name your team will recognize.</Text>
            <FormLayout>
              <TextInput id={`${id}-name`} htmlName="accountName" label="Display name" description="Optional. Up to 128 characters." value={fields.accountName} onChange={(value) => change("accountName", value)} status={status("accountName")} isDisabled={disabled} isOptional hasAutoFocus />
            </FormLayout>
            <VStack gap={2}>
              <Text type="label">Cloud provider</Text>
              <HStack gap={2} wrap="wrap">{Object.values(providerRegistry).map((provider) => <Button key={provider.id} label={`${provider.id === "aws" ? "AWS" : provider.label}${provider.enabled ? " · Selected" : " · Planned"}`} isDisabled={disabled || !provider.enabled || !!account} tooltip={provider.enabled ? "Amazon Web Services" : provider.disabledReason} onClick={() => change("provider", provider.id)} />)}</HStack>
            </VStack>
          </>}
          {step === 1 && <>
            <Text color="secondary">Use the account ID and the IAM role the platform will assume. The standard role works with the onboarding template.</Text>
            <FormLayout>
              <TextInput id={`${id}-aws-id`} htmlName="accountId" label="AWS account ID" description="12 digits. This cloud identity cannot be changed when editing." value={fields.accountId} onChange={(value) => change("accountId", value)} isRequired isReadOnly={!!account} isDisabled={disabled} status={status("accountId")} inputMode="numeric" autoComplete="off" autoCapitalize="none" hasAutoCorrect={false} hasSpellCheck={false} hasAutoFocus />
              <TextInput id={`${id}-role`} htmlName="role" label="IAM role name" description="Role name only, without an ARN or path." value={fields.role} onChange={(value) => change("role", value)} isRequired isDisabled={disabled} status={status("role")} autoComplete="off" autoCapitalize="none" hasAutoCorrect={false} hasSpellCheck={false} />
            </FormLayout>
            <Collapsible trigger="How role access works" defaultIsOpen={false}><Text>The role must trust the platform’s management role. After connecting, the Access tab provides the CloudFormation template and parameters. Create or update that stack in your own AWS session, then request verification.</Text></Collapsible>
          </>}
          {step === 2 && <>
            <Text color="secondary">Review the account connection before saving.</Text>
            <VStack gap={4}>
              <HStack gap={6} wrap="wrap"><Value label="Display name">{fields.accountName.trim() || "Unnamed account"}</Value><Value label="Provider">Amazon Web Services</Value></HStack>
              <Divider />
              <HStack gap={6} wrap="wrap"><Value label="AWS account ID">{fields.accountId.trim()}</Value><Value label="Tenant">{tenantId}</Value></HStack>
              <Value label="IAM role">{fields.role.trim()}</Value>
              {account && <Value label="Platform account ID">{account.id}</Value>}
              <Banner status="info" title={account ? "Updates the existing connection" : "Connects an existing AWS account"} description={account ? "Saves the display name and assumed role, then converges the ProviderConfig. Role changes affect access for dependent networks and clusters. Cloud resources are not created by this request." : "Registers the account and creates its platform ProviderConfig. Reusing an AWS account ID updates its existing connection. Access verification runs asynchronously; this does not create an AWS account."} />
            </VStack>
          </>}
          <ErrorNotice error={mutation.error} />
        </VStack>
      </form>
    </WizardDialog>
    {!account && <ManifestCreateDialog kind="CloudAccount" isOpen={open && createMode === "yaml"} onOpenChange={setOpen} onCreated={(saved: CloudAccount) => { setOpen(false); onAccepted(saved); }} onUseForm={() => { setCreateMode("guided"); setOpen(true); }} />}
  </>;
}

function VerifyAccount({ account, onAccepted }: { account: CloudAccount; onAccepted: (account: CloudAccount) => void }) {
  const { api, tenantId, canWrite } = usePlatform();
  const id = useId();
  const [open, setOpen] = useState(false);
  const [step, setStep] = useState(0);
  const [regions, setRegions] = useState("");
  const [validation, setValidation] = useState("");
  const mutation = useAccountMutation();
  const disabled = !tenantId || !canWrite || mutation.busy;
  function submit() {
    if (disabled) return;
    try { parseVerifyRegions(regions); setValidation(""); } catch (cause) { setValidation(errorMessage(cause)); return; }
    if (step === 0) { setStep(1); return; }
    void mutation.run((signal) => verifyAccount(api, tenantId, account.id, regions, signal), (response) => { setOpen(false); onAccepted(response.account ?? account); });
  }
  return <>
    <Button label="Verify access" isDisabled={disabled} onClick={() => { setStep(0); setValidation(""); mutation.clearError(); setOpen(true); }} />
    <WizardDialog isOpen={open} onOpenChange={(next) => { if (!mutation.busy) setOpen(next); }} title="Verify account access" description="Ask the controller to check the tenant role and required permissions." steps={["Regions", "Review verification"]} step={step} busy={mutation.busy} footer={<HStack justify="between" gap={2} wrap="wrap"><Button label="Cancel" isDisabled={mutation.busy} onClick={() => setOpen(false)} /><HStack gap={2}>{step > 0 && <Button label="Back" isDisabled={mutation.busy} onClick={() => setStep(0)} />}<Button type="submit" form={id} label={step === 0 ? "Next" : "Request verification"} variant="primary" isLoading={mutation.busy} isDisabled={disabled} /></HStack></HStack>}>
      <form id={id} method="post" aria-label="Verify account access" onSubmit={(event) => { event.preventDefault(); submit(); }}>
        <VStack gap={4}>
          {step === 0 ? <TextInput id={`${id}-regions`} htmlName="regions" label="Additional regions" description="Optional comma-separated regions. Network regions are always checked. Empty keeps regions requested previously." value={regions} onChange={(value) => { setRegions(value); setValidation(""); }} isDisabled={disabled} status={validation ? { type: "error", message: validation } : undefined} hasAutoFocus autoComplete="off" autoCapitalize="none" hasAutoCorrect={false} hasSpellCheck={false} /> : <>
            <Value label="Account">{accountName(account)}</Value>
            <Value label="Additional regions">{parseVerifyRegions(regions).join(", ") || "Keep previously requested regions"}</Value>
            <Banner status="info" title="Verification runs asynchronously" description="Acceptance records a request. The last access result stays visible until the controller reports a new attempt." />
          </>}
          <ErrorNotice error={mutation.error} />
        </VStack>
      </form>
    </WizardDialog>
  </>;
}

function DeleteAccount({ account, onAccepted }: { account: CloudAccount; onAccepted: () => void }) {
  const { api, tenantId, canWrite } = usePlatform();
  const id = useId();
  const [open, setOpen] = useState(false);
  const [confirmation, setConfirmation] = useState("");
  const mutation = useAccountMutation();
  const disabled = !tenantId || !canWrite || mutation.busy;
  return <>
    <Button label="Delete account" variant="destructive" isDisabled={disabled} onClick={() => { setConfirmation(""); mutation.clearError(); setOpen(true); }} />
    <Dialog isOpen={open} onOpenChange={(next) => { if (!mutation.busy) setOpen(next); }} purpose={mutation.busy ? "required" : "form"} aria-describedby={`${id}-impact`} padding={0}>
      <DialogHeader title={`Delete ${accountName(account)}?`} onOpenChange={mutation.busy ? undefined : setOpen} />
      <VStack padding={5} gap={4}>
        <Text id={`${id}-impact`}>The controller removes the platform account record and ProviderConfig. AWS resources, the IAM role and its CloudFormation stack remain. Networks and clusters are not cascade-deleted; the backend refuses this request while dependent networks exist.</Text>
        <Value label="Platform account ID"><CopyValue value={account.id} label="Platform account ID" /></Value>
        <Value label="AWS account ID">{account.provider.case === "aws" ? <CopyValue value={account.provider.value.accountId} label="AWS account ID" /> : "Unsupported provider"}</Value>
        <form method="post" aria-label="Confirm account deletion" onSubmit={(event) => { event.preventDefault(); if (!disabled && confirmation === account.id) void mutation.run((signal) => deleteAccount(api, tenantId, account.id, signal), () => { setOpen(false); onAccepted(); }); }}>
          <VStack gap={4}>
            <TextInput id={`${id}-confirmation`} htmlName="confirmation" label="Type the platform account ID to confirm" value={confirmation} onChange={setConfirmation} isDisabled={disabled} autoComplete="off" autoCapitalize="none" hasAutoCorrect={false} hasSpellCheck={false} />
            <ErrorNotice error={mutation.error} />
            <HStack gap={2} justify="end" wrap="wrap"><Button label="Cancel" data-autofocus="" isDisabled={mutation.busy} onClick={() => setOpen(false)} /><Button type="submit" label="Confirm deletion" variant="destructive" isDisabled={disabled || confirmation !== account.id} isLoading={mutation.busy} /></HStack>
          </VStack>
        </form>
      </VStack>
    </Dialog>
  </>;
}

const inventoryColumns: TableColumn<ManagedResource>[] = [
  { key: "name", header: "Resource", renderCell: (row) => <CopyValue value={row.name} label="Resource name" /> },
  { key: "kind", header: "Kind", renderCell: (row) => <Text maxLines={1}>{row.kind}{row.zoneId ? ` · ${row.zoneId}` : ""}{row.role ? ` · ${row.role}` : ""}</Text> },
  { key: "externalName", header: "Cloud ID", renderCell: (row) => row.externalName ? <CopyValue value={row.externalName} label="Cloud resource ID" /> : <Text>Not assigned</Text> },
  { key: "state", header: "Status", renderCell: (row) => <StatusValue label={`${titleCase(ManagedResourceState[row.state] ?? "Unknown")}${row.paused ? " · Paused" : ""}`} tone={row.state === ManagedResourceState.READY ? "success" : row.state === ManagedResourceState.FAILED ? "error" : "neutral"} /> },
  { key: "ready", header: "Ready / synced", renderCell: (row) => <Text>{titleCase(ConditionStatus[row.ready] ?? "Unknown")} / {titleCase(ConditionStatus[row.synced] ?? "Unknown")}</Text> },
  { key: "message", header: "Details", renderCell: (row) => <Text maxLines={1}>{[row.reason, row.message, row.observedGeneration < row.generation ? "Latest change pending reconciliation" : "", row.deletionTime ? `Deleting since ${timestampText(row.deletionTime)}` : ""].filter(Boolean).join(" · ") || "No issues reported"}</Text> },
];
const accessColumns: TableColumn<AccessResult>[] = [
  { key: "region", header: "Region" },
  { key: "action", header: "Permission", renderCell: (row) => <Text>{row.action || "Assume role"}</Text> },
  { key: "ok", header: "Result", renderCell: (row) => <Text>{row.ok ? "Passed" : "Failed"}</Text> },
  { key: "message", header: "Details", renderCell: (row) => <Text>{[row.reason, row.message].filter(Boolean).join(": ") || "—"}</Text> },
];
const networkColumns: TableColumn<Network>[] = [
  { key: "name", header: "Network", renderCell: (row) => <AstryxLink href={`/networks/${encodeURIComponent(row.id)}`}>{row.displayName || row.name}</AstryxLink> },
  { key: "id", header: "Platform ID", renderCell: (row) => <CopyValue value={row.id} label="Network ID" /> },
  { key: "region", header: "Region", renderCell: (row) => <Text>{row.spec?.region || "Not reported"}</Text> },
  { key: "state", header: "Status", renderCell: (row) => <StatusValue label={titleCase(NetworkState[row.status?.state ?? 0] ?? "Unknown")} tone={row.status?.state === NetworkState.READY ? "success" : row.status?.state === NetworkState.FAILED ? "error" : "neutral"} /> },
  { key: "tier", header: "Rollout tier", renderCell: (row) => <Text>{titleCase(Tier[row.spec?.tier ?? Tier.TIER_UNSPECIFIED] ?? "Unknown")}</Text> },
];
const clusterColumns = (networks: readonly Network[]): TableColumn<Cluster>[] => [
  { key: "name", header: "Cluster", renderCell: (row) => <AstryxLink href={`/clusters/${encodeURIComponent(row.id)}`}>{row.displayName || row.name}</AstryxLink> },
  { key: "id", header: "Platform ID", renderCell: (row) => <CopyValue value={row.id} label="Cluster ID" /> },
  { key: "region", header: "Region", renderCell: (row) => <Text>{row.spec?.region || "Not reported"}</Text> },
  { key: "state", header: "Status", renderCell: (row) => <StatusValue label={titleCase(ClusterState[row.status?.state ?? 0] ?? "Unknown")} tone={row.status?.state === ClusterState.RUNNING ? "success" : row.status?.state === ClusterState.FAILED ? "error" : "neutral"} /> },
  { key: "version", header: "Kubernetes", renderCell: (row) => <Text>{row.status?.currentKubernetesVersion || "Not reported"}</Text> },
  { key: "channel", header: "Release channel", renderCell: (row) => <Text>{titleCase(ReleaseChannel[row.spec?.releaseChannel ?? ReleaseChannel.UNSPECIFIED] ?? "Unknown")}</Text> },
  { key: "network", header: "Network", renderCell: (row) => {
    const networkId = row.spec?.network?.networkId;
    const network = networks.find((entry) => entry.id === networkId);
    return networkId ? <AstryxLink href={`/networks/${encodeURIComponent(networkId)}`}>{network?.displayName || network?.name || networkId}</AstryxLink> : <Text>Not reported</Text>;
  } },
  { key: "tier", header: "Rollout tier", renderCell: (row) => <Text>{titleCase(ClusterTier[row.status?.tier ?? ClusterTier.CLUSTER_TIER_UNSPECIFIED] ?? "Unknown")}</Text> },
];

function AccountDetails({ id }: { id: string }) {
  const { api, tenantId, canWrite } = usePlatform();
  const uiId = useId();
  const [tab, setTab] = useState("overview");
  const [search, setSearch] = useState("");
  const [resourceKind, setResourceKind] = useState("all");
  const [resourceState, setResourceState] = useState("all");
  const [pausedFilter, setPausedFilter] = useState("all");
  const [revision, setRevision] = useState(0);
  const [waitingForAttempt, setWaitingForAttempt] = useState<string>();
  const [notice, setNotice] = useState("");
  const [deletionRequested, setDeletionRequested] = useState(false);
  const load = useCallback(async (signal: AbortSignal) => {
    if (!tenantId || deletionRequested) return undefined;
    const response = await api.tenant.getCloudAccount({ tenantId, id }, { signal });
    if (!response.account) throw new Error("The server did not return this account.");
    return response.account;
  }, [api, tenantId, id, deletionRequested]);
  const detail = useAsyncQuery(load);
  const account = detail.data;
  const loadRelated = useCallback(async (signal: AbortSignal) => {
    if (!tenantId || deletionRequested) return undefined;
    return loadAccountRelations(api, tenantId, id, signal);
  }, [api, tenantId, id, deletionRequested]);
  const related = useAsyncQuery(loadRelated);
  const awsId = account?.provider.case === "aws" ? account.provider.value.accountId : "";
  const awsRole = account?.provider.case === "aws" ? account.provider.value.role : "";
  const loadOnboarding = useCallback(async (signal: AbortSignal) => {
    if (!tenantId || !awsId || tab !== "access" || deletionRequested) return undefined;
    const identity = create(CloudAccountSchema, { id, provider: { case: "aws", value: { accountId: awsId, role: awsRole } } });
    const response = await api.tenant.getAccountOnboarding(onboardingRequest(tenantId, identity), { signal });
    if (!response.onboarding) throw new Error("The server did not return onboarding instructions.");
    return response.onboarding;
  }, [api, tenantId, id, awsId, awsRole, tab, deletionRequested]);
  const onboarding = useAsyncQuery(loadOnboarding);
  const awaitingAttempt = waitingForAttempt !== undefined && (!account || accessAttemptKey(account) === waitingForAttempt);
  const progressing = !!tenantId && !deletionRequested && !detail.error && (!!account && accountProgressing(account) || awaitingAttempt);
  useAccountPolling(progressing, detail.refresh, revision, awaitingAttempt || deletionRequested);
  useAccountPolling(progressing && !related.error, related.refresh, revision, awaitingAttempt || deletionRequested);
  function refresh() { detail.refresh(); related.refresh(); onboarding.refresh(); setRevision((value) => value + 1); }
  function accepted(saved: CloudAccount, message: string) { setWaitingForAttempt(accessAttemptKey(saved)); setNotice(message); refresh(); }
  const resources = account?.status?.managedResources ?? [];
  const resourceKinds = [...new Set(resources.map((resource) => resource.kind))].sort((a, b) => a.localeCompare(b));
  const resourceTerms = search.trim().toLowerCase().split(/\s+/).filter(Boolean);
  const filtered = resources.filter((resource) => {
    const searchable = `${resource.name} ${resource.kind} ${resource.externalName}`.toLowerCase();
    return resourceTerms.every((term) => searchable.includes(term)) &&
      (resourceKind === "all" || resource.kind === resourceKind) &&
      (resourceState === "all" || String(resource.state) === resourceState) &&
      (pausedFilter === "all" || resource.paused === (pausedFilter === "paused"));
  });
  const access = account?.status?.access;
  const regions = [...new Set([...(related.data?.networks.map((network) => network.spec?.region ?? "") ?? []), ...(access?.regions.map((result) => result.region) ?? [])].filter(Boolean))];
  const conditions = account?.status?.conditions ?? [];
  const healthIssues = conditions.filter((condition) => condition.status === ConditionStatus.FALSE || condition.status === ConditionStatus.UNKNOWN);
  return <VStack gap={6}>
    <DetailHeader title={account ? accountName(account) : "Account details"}
      metadata={<><HStack gap={2} vAlign="center"><ProviderIcon provider={account?.provider.case ?? ""} size="sm" /><Text color="secondary">{account?.provider.case === "aws" ? "Amazon Web Services" : "Cloud account"}</Text></HStack>{awsId && <CopyValue value={awsId} label="AWS account ID" isSecondary />}<Text color="secondary">{regions.length ? regions.join(", ") : "No regions reported"}</Text>{account && <CopyButton value={accountName(account)} label="Account name" />}</>}
      status={account && <AccessStatus account={account} />}
      actions={<>
        {account && <ResourceYamlButton kind="CloudAccount" resourceId={account.id} />}
        {account && <ConnectAccount account={account} onAccepted={(saved) => accepted(saved, "Account update accepted. Access will be checked asynchronously.")} />}
        {account && <DeleteAccount account={account} onAccepted={() => { setDeletionRequested(true); setNotice("Deletion requested. The controller must remove the ProviderConfig and account record. Return to accounts to observe completion."); }} />}
        <RefreshButton label="Refresh" isLoading={detail.refreshing || related.refreshing} isDisabled={!tenantId || deletionRequested} onClick={refresh} />
      </>} />
    {!tenantId && <Banner status="warning" title="Select a tenant to view this account" />}
    {tenantId && !canWrite && <Text color="secondary">Read-only access · Account mutations require tenant permissions.</Text>}
    {detail.loading && !deletionRequested && <Text role="status">Loading account…</Text>}
    <ErrorNotice error={detail.error} />
    <VStack aria-live="polite" role="status" gap={2}>{notice && <Banner status="info" title={notice} />}{awaitingAttempt && !deletionRequested && <Text>Waiting for a new access-check attempt. Acceptance does not mean verified.</Text>}</VStack>
    {account && <>
      <DetailSummary items={[
        { label: "Account access", value: <AccessStatus account={account} />, note: titleCase(accountStateLabel(account)) },
        { label: "Managed resources", value: resources.length, note: `${resources.filter((resource) => resource.state === ManagedResourceState.READY).length} ready` },
        { label: "Networks", value: related.data ? related.data.networks.length : related.error ? "Unavailable" : "Loading…", note: "Connected to this account" },
        { label: "Clusters", value: related.data ? related.data.clusters.length : related.error ? "Unavailable" : "Loading…", note: "On connected networks" },
      ]} />
      <TabList role="tablist" aria-label="Account sections" value={tab} onChange={setTab} hasDivider>
        {["overview", "resources", "activity", "access"].map((name) => <Tab key={name} id={`${uiId}-tab-${name}`} value={name} label={titleCase(name)} panelId={`${uiId}-panel-${name}`} />)}
      </TabList>
      <VStack role="tabpanel" id={`${uiId}-panel-${tab}`} aria-labelledby={`${uiId}-tab-${tab}`} tabIndex={0} gap={6}>
        {tab === "overview" && <>
          <HStack gap={6} wrap="wrap"><Value label="IAM role"><CopyValue value={awsRole} label="IAM role name" /></Value><Value label="Last access check">{timestampText(access?.checkedTime)}</Value><Value label="Created">{timestampText(account.createTime)}</Value></HStack>
          {healthIssues.length > 0 ? <Banner status={healthIssues.some((condition) => condition.status === ConditionStatus.FALSE) ? "warning" : "info"} title="Account needs attention" description={healthIssues.map((condition) => condition.message || condition.reason || condition.type).join(" · ")} /> : <Text color="secondary">{access?.verified && !access.stale ? "The last access check passed. Resource health is reported separately in Resources." : "Access has not been confirmed. Open Access to configure the role and request a verification."}</Text>}
          <Divider />
          <VStack gap={3}><Heading level={2}>Connected infrastructure</Heading><Text color="secondary">Networks in this account and the clusters running on them.</Text><ErrorNotice error={related.error} />{related.loading && <Text role="status">Loading related infrastructure…</Text>}{related.data && <>
            <Heading level={3}>Networks</Heading>{related.data.networks.length ? <Table searchable aria-label="Account related networks" data={related.data.networks} columns={networkColumns} idKey="id" density="compact" /> : <Text color="secondary">No networks belong to this account.</Text>}
            <Heading level={3}>Clusters</Heading>{related.data.clusters.length ? <Table searchable aria-label="Account related clusters" data={related.data.clusters} columns={clusterColumns(related.data.networks)} idKey="id" density="compact" /> : <Text color="secondary">No clusters belong to this account’s networks.</Text>}
          </>}</VStack>
          <Collapsible trigger="Platform connection details" defaultIsOpen={false}><VStack gap={4} padding={3}><Value label="Platform account ID"><CopyValue value={account.id} label="Platform account ID" /></Value><Value label="Tenant"><CopyValue value={account.tenantId} label="Tenant ID" /></Value><Value label="ProviderConfig"><CopyValue value={account.status?.providerConfigName ?? ""} label="ProviderConfig name" /></Value><Value label="Namespace"><CopyValue value={account.status?.namespace ?? ""} label="Namespace" /></Value><Value label="Assumed identity"><CopyValue value={account.status?.identity ?? ""} label="Assumed identity" /></Value><Value label="Updated">{timestampText(account.updateTime)}</Value></VStack></Collapsible>
        </>}
        {tab === "resources" && <>
          <HStack gap={3} justify="between" wrap="wrap"><VStack gap={1}><Heading level={2}>Managed resources</Heading><Text role="status" color="secondary">{filtered.length} of {resources.length} resources · {resources.filter((resource) => resource.paused).length} paused</Text></VStack></HStack>
          <FilterRow label="Resource filters">
            <TextInput id={`${uiId}-search`} label="Search resources" type="search" placeholder="Name, kind or cloud ID" value={search} onChange={setSearch} autoComplete="off" />
            <Selector label="Resource kind" value={resourceKind} onChange={setResourceKind} options={[{ value: "all", label: "All kinds" }, ...resourceKinds.map((kind) => ({ value: kind, label: kind || "Unknown" }))]} />
            <Selector label="Resource status" value={resourceState} onChange={setResourceState} options={[{ value: "all", label: "All statuses" }, ...Object.values(ManagedResourceState).filter((state) => typeof state === "number").map((state) => ({ value: String(state), label: titleCase(ManagedResourceState[state]) }))]} />
            <Selector label="Paused resources" value={pausedFilter} onChange={setPausedFilter} options={[{ value: "all", label: "All resources" }, { value: "paused", label: "Paused only" }, { value: "active", label: "Not paused" }]} />
            {(search || resourceKind !== "all" || resourceState !== "all" || pausedFilter !== "all") && <Button label="Clear resource filters" onClick={() => { setSearch(""); setResourceKind("all"); setResourceState("all"); setPausedFilter("all"); }} />}
          </FilterRow>
          {filtered.length ? <VStack isScrollable><Table aria-label="Account managed resource inventory" data={filtered} columns={inventoryColumns} idKey={(resource) => `${resource.kind}:${resource.name}`} density="compact" /></VStack> : <Text color="secondary">{resources.length ? "No resources match the current filters." : "No managed resources were reported."}</Text>}
        </>}
        {tab === "activity" && <>
          <VStack gap={2}><Heading level={2}>Account observations</Heading><Text color="secondary">Latest controller conditions. The account API does not expose an event history; network and cluster activity is available from their detail pages.</Text></VStack>
          {conditions.length ? <Table searchable aria-label="Account controller observations" density="compact" data={conditions.map((condition, index) => ({ ...condition, index }))} idKey="index" columns={[
            { key: "type", header: "Condition" },
            { key: "status", header: "Status", renderCell: (condition) => <Text>{titleCase(ConditionStatus[condition.status] ?? "Unknown")}</Text> },
            { key: "message", header: "Observation", renderCell: (condition) => <Text>{condition.message || condition.reason || "Not reported"}</Text> },
            { key: "lastTransitionTime", header: "Last transition", renderCell: (condition) => <Text>{timestampText(condition.lastTransitionTime)}</Text> },
          ]} /> : <Text color="secondary">No controller conditions reported.</Text>}
        </>}
        {tab === "access" && <>
          <HStack hAlign="between" vAlign="center" width="100%" wrap="wrap" gap={3}><VStack gap={2}><Heading level={2}>Role access</Heading><Text color="secondary">Configure trust, then check the permissions the platform needs.</Text></VStack><VerifyAccount account={account} onAccepted={(saved) => accepted(saved, "Verification requested. A new controller attempt will update the result.")} /></HStack>
          <HStack gap={6} wrap="wrap"><Value label="Last verdict"><AccessStatus account={account} /></Value><Value label="Checked">{timestampText(access?.checkedTime)}</Value><Value label="Last attempt">{timestampText(access?.attemptedTime)}</Value></HStack>
          {access?.message && <Banner status={access.stale ? "warning" : access.verified ? "info" : "error"} title={access.reason || "Access check"} description={access.message} />}
          {access && (access.regions.length > 0 || access.checks.length > 0) && <Table searchable aria-label="Account access checks" density="compact" data={[...access.regions, ...access.checks]} columns={accessColumns} idKey={(result) => `${result.region}:${result.action}`} />}
          <Divider />
          <VStack gap={3}><HStack hAlign="between" vAlign="center" width="100%" gap={3}><Heading level={3}>Set up the IAM role</Heading>{onboarding.error && <RefreshButton label="Retry instructions" isLoading={onboarding.refreshing} onClick={onboarding.refresh} />}</HStack><Text color="secondary">Use your own AWS session to create or update the role stack. No cloud credentials are collected and this UI makes no AWS calls.</Text><ErrorNotice error={onboarding.error} />{onboarding.loading && <Text role="status">Loading instructions…</Text>}{onboarding.data && <>
            <Value label="CloudFormation template path on your API server">{onboarding.data.templatePath}</Value>
            <HStack gap={6} wrap="wrap"><Value label="Stack name / RoleName"><CopyValue value={onboarding.data.roleName} label="Stack role name" /></Value><Value label="AWS account ID"><CopyValue value={onboarding.data.accountId} label="AWS account ID" /></Value></HStack>
            <Collapsible trigger="Template parameters and trusted identity" defaultIsOpen={false}><VStack gap={4} padding={3}><Value label="ManagementRoleArn"><CopyValue value={onboarding.data.managementRoleArn} label="Management role ARN" /></Value><HStack gap={2} align="end"><TextInput id={`${uiId}-external`} label="ExternalId parameter" description="Use only in this account’s role stack. Keep this value private." type="password" value={onboarding.data.externalId} isReadOnly autoComplete="off" /><CopyButton value={onboarding.data.externalId} label="External ID parameter" /></HStack><Value label="Assumed principal"><CopyValue value={access?.principalArn ?? ""} label="Assumed principal ARN" /></Value><Text>Pass RoleName, ManagementRoleArn and, when present, ExternalId to CloudFormation. Acknowledge named IAM resources. Wait for the stack to finish before requesting verification.</Text></VStack></Collapsible>
          </>}</VStack>
        </>}
      </VStack>
      <Text type="supporting" color="secondary">Use Refresh for the latest observations, or enable Live updates to refresh progressing accounts every five seconds for up to two minutes. Requested access checks are observed automatically during that window. A ready ProviderConfig alone does not mean account access is verified.</Text>
    </>}
  </VStack>;
}

function AccountsList() {
  const { api, tenantId, canWrite } = usePlatform();
  const [page, setPage] = useState(1);
  const [search, setSearch] = useState("");
  const [providerFilter, setProviderFilter] = useState("all");
  const [stateFilter, setStateFilter] = useState("all");
  const [accessFilter, setAccessFilter] = useState("all");
  const [revision, setRevision] = useState(0);
  const [saved, setSaved] = useState<CloudAccount>();
  const load = useCallback(async (signal: AbortSignal) => {
    if (!tenantId) return undefined;
    return api.tenant.listCloudAccounts({ tenantId }, { signal });
  }, [api, tenantId]);
  const list = useAsyncQuery(load);
  const accounts = list.data?.accounts ?? [];
  useAccountPolling(!!tenantId && !list.error && accounts.some(accountProgressing), list.refresh, revision);
  const terms = search.trim().toLowerCase().split(/\s+/).filter(Boolean);
  const filtered = accounts.filter((account) => {
    const provider = account.provider.case ?? "unsupported";
    const aws = account.provider.case === "aws" ? account.provider.value : undefined;
    const searchable = [accountName(account), account.accountName, account.id, aws?.accountId, aws?.role].filter(Boolean).join(" ").toLowerCase();
    const access = account.status?.access;
    const accessState = !access ? "not-checked" : access.stale ? "stale" : access.verified ? "verified" : "not-verified";
    return terms.every((term) => searchable.includes(term)) &&
      (providerFilter === "all" || providerFilter === provider) &&
      (stateFilter === "all" || stateFilter === String(account.status?.state ?? CloudAccountState.UNSPECIFIED)) &&
      (accessFilter === "all" || accessFilter === accessState);
  });
  const visible = accountPage(filtered, page);
  function refresh() { list.refresh(); setRevision((value) => value + 1); }
  function clearFilters() { setSearch(""); setProviderFilter("all"); setStateFilter("all"); setAccessFilter("all"); setPage(1); }
  const columns: TableColumn<CloudAccount>[] = [
    { key: "accountName", header: "Account name", renderCell: (account) => <AstryxLink href={`/accounts/${encodeURIComponent(account.id)}`}>{accountName(account)}</AstryxLink> },
    { key: "id", header: "Platform ID", renderCell: (account) => <CopyValue value={account.id} label="Platform account ID" /> },
    { key: "provider", header: "Provider", renderCell: (account) => <Text maxLines={1}>{account.provider.case === "aws" ? "AWS" : "Unsupported"}</Text> },
    { key: "cloudId", header: "Cloud account ID", renderCell: (account) => account.provider.case === "aws" ? <CopyValue value={account.provider.value.accountId} label="AWS account ID" /> : <Text>Not reported</Text> },
    { key: "status", header: "Status", renderCell: (account) => <StatusValue label={titleCase(accountStateLabel(account))} tone={account.status?.state === CloudAccountState.READY ? "success" : account.status?.state === CloudAccountState.FAILED ? "error" : "neutral"} /> },
    { key: "access", header: "Access", renderCell: (account) => <AccessStatus account={account} /> },
    { key: "checked", header: "Last verification", renderCell: (account) => {
      const checked = account.status?.access?.checkedTime;
      return <Text maxLines={1}>{checked ? new Date(Number(checked.seconds) * 1_000 + Math.floor(checked.nanos / 1_000_000)).toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" }) : "Not checked"}</Text>;
    } },
  ];
  return <VStack gap={5}>
    <HStack hAlign="end" vAlign="center" gap={3} wrap="wrap" width="100%"><StackItem size="fill"><VStack gap={1}><Heading level={2}>Cloud connections</Heading><Text color="secondary">Manage the accounts where your infrastructure runs.</Text></VStack></StackItem><HStack gap={2} vAlign="center" wrap="wrap"><ConnectAccount onAccepted={(account) => { setSaved(account); refresh(); }} /><RefreshButton label="Refresh accounts" isDisabled={!tenantId} isLoading={list.refreshing} onClick={refresh} /></HStack></HStack>
    {!tenantId && <Banner status="warning" title="Select a tenant to manage accounts" />}
    {tenantId && !canWrite && <Text color="secondary">Read-only access · Account mutations require tenant permissions.</Text>}
    <ErrorNotice error={list.error} />
    {saved && <Banner status="info" title="Account connection accepted" description="Access is checked asynchronously. Open the account to set up its role and review verification." endContent={<Button as={Link} href={`/accounts/${encodeURIComponent(saved.id)}`} label="View account" />} />}
    {list.loading && tenantId && <Text role="status">Loading accounts…</Text>}
    <FilterRow label="Account filters">
      <TextInput label="Search accounts" type="search" placeholder="Name, platform ID, cloud ID or role" value={search} onChange={(value) => { setSearch(value); setPage(1); }} autoComplete="off" />
      <Selector label="Provider" value={providerFilter} onChange={(value) => { setProviderFilter(value); setPage(1); }} options={[{ value: "all", label: "All providers" }, { value: "aws", label: "AWS" }, { value: "unsupported", label: "Unsupported" }]} />
      <Selector label="Connection state" value={stateFilter} onChange={(value) => { setStateFilter(value); setPage(1); }} options={[{ value: "all", label: "All states" }, ...Object.values(CloudAccountState).filter((state) => typeof state === "number").map((state) => ({ value: String(state), label: titleCase(CloudAccountState[state]) }))]} />
      <Selector label="Access state" value={accessFilter} onChange={(value) => { setAccessFilter(value); setPage(1); }} options={[{ value: "all", label: "All access states" }, { value: "verified", label: "Verified" }, { value: "not-verified", label: "Not verified" }, { value: "stale", label: "Stale" }, { value: "not-checked", label: "Not checked" }]} />
      {(search || providerFilter !== "all" || stateFilter !== "all" || accessFilter !== "all") && <Button label="Clear account filters" onClick={clearFilters} />}
    </FilterRow>
    {list.data && <Text role="status" color="secondary">{filtered.length} of {accounts.length} accounts</Text>}
    {list.data && (accounts.length ? filtered.length ? <><Table aria-label="Cloud accounts" data={visible.accounts} columns={columns} idKey="id" rowIndexStart={(visible.page - 1) * ACCOUNT_PAGE_SIZE + 1} rowCount={filtered.length} /><Pagination label="Accounts pagination" page={visible.page} onChange={setPage} totalItems={filtered.length} pageSize={ACCOUNT_PAGE_SIZE} variant="count" /></> : <Text>No accounts match the current filters.</Text> : <VStack padding={6} gap={3}><Heading level={3}>Connect your first cloud account</Heading><Text color="secondary">Connect an existing AWS account to begin creating networks and clusters.</Text></VStack>)}
  </VStack>;
}

export function AccountsPage({ resourceId }: { resourceId?: string } = {}) {
  return resourceId ? <AccountDetails key={resourceId} id={resourceId} /> : <AccountsList />;
}
