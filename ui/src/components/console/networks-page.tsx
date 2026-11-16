"use client";

import { useCallback, useEffect, useId, useRef, useState, type FormEvent } from "react";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { CheckboxInput } from "@astryxdesign/core/CheckboxInput";
import { Collapsible } from "@astryxdesign/core/Collapsible";
import { Divider } from "@astryxdesign/core/Divider";
import { FormLayout } from "@astryxdesign/core/FormLayout";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack } from "@astryxdesign/core/HStack";
import { Link } from "@astryxdesign/core/Link";
import { MetadataList, MetadataListItem } from "@astryxdesign/core/MetadataList";
import { Section } from "@astryxdesign/core/Section";
import { Selector } from "@astryxdesign/core/Selector";
import { Tab, TabList } from "@astryxdesign/core/TabList";
import { proportional, type TableColumn } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import { TextArea } from "@astryxdesign/core/TextArea";
import { TextInput } from "@astryxdesign/core/TextInput";
import { VStack } from "@astryxdesign/core/VStack";
import { timestampDate, type Timestamp } from "@bufbuild/protobuf/wkt";
import { fromJson } from "@bufbuild/protobuf";
import { usePlatform } from "@/components/platform-provider";
import { PlanReview } from "@/components/resources";
import { WizardDialog } from "./wizard-dialog";
import { ManifestCreateDialog } from "./manifest-create-dialog";
import { ImportManifestDialog } from "./import-manifest-dialog";
import { ConsoleTable as Table } from "./console-table";
import { DetailHeader, DetailSummary, StatusValue } from "./detail-layout";
import { CopyButton, CopyValue } from "./copy-value";
import { ResourceYamlButton } from "./resource-yaml-button";
import { RefreshButton } from "./refresh-button";
import { ProviderIcon } from "./provider-icons";
import { ActivityEventTable, ActivityEventFilters, filterActivityEvents, type ActivityFilters } from "./activity-event-table";
import { errorMessage, useAsyncQuery } from "@/lib/api/use-query";
import { collectPages } from "@/lib/api/pagination";
import { ConditionStatus, type Condition } from "@/gen/cloud/v1/conditions_pb";
import { CodeChangePhase } from "@/gen/cloud/v1/code_change_pb";
import { ClusterState, ReleaseChannel } from "@/gen/cloud/v1/cluster_pb";
import { Disposition, ImportPhase, type ImportedResource } from "@/gen/cloud/v1/import_pb";
import { ManagedResourceState } from "@/gen/cloud/v1/managed_resource_pb";
import { FlowLogTrafficType, NatGatewayMode, NetworkLayoutSchema, NetworkSchema, NetworkState, SubnetRole, type Network, type Subnet } from "@/gen/cloud/v1/network_pb";
import { Tier } from "@/gen/cloud/v1/tier_pb";
import type { CloudAccount } from "@/gen/cloud/v1/tenant_service_pb";
import {
  buildCreateNetwork, buildImportNetwork, buildNetworkUpdate, buildTakeOverNetwork, canApplyNetworkPlan, createAttempt, enumLabel,
  networkDraft, networkIsReady, networkJson, networkProvider, newNetworkDraft,
  networkTakeoverBlockReason, networkTakeoverKey, newNetworkImportDraft,
  reviewedNetworkApply, reviewNetworkUpdate,
  validateNetworkStep, validateNetworkImportStep,
  collectNetworkClusters, filterNetworkInventory, networkClusterRows,
  type CreateAttempt, type NetworkDraft, type NetworkImportDraft, type NetworkTakeoverConfirmation, type ReviewedNetworkUpdate,
} from "@/lib/workflows/networks";

/** Abort pending work on scope/selection changes and lock duplicate submissions. */
function useNetworkMutation() {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const pending = useRef<AbortController | null>(null);
  useEffect(() => () => { pending.current?.abort(); }, []);
  const run = async (action: (signal: AbortSignal) => Promise<void>) => {
    if (pending.current) return;
    const controller = new AbortController();
    pending.current = controller;
    setBusy(true); setError("");
    try { await action(controller.signal); }
    catch (failure) { if (!controller.signal.aborted) setError(errorMessage(failure)); }
    finally {
      if (!controller.signal.aborted) { setBusy(false); pending.current = null; }
    }
  };
  return { busy, error, setError, run };
}

function ErrorNotice({ error }: { error: string }) {
  return error ? <Banner status="error" title="Request failed" description={error} /> : null;
}

function time(value: Timestamp | undefined): string {
  return value ? timestampDate(value).toISOString() : "Not reported";
}

function AccountReference({ id, account }: { id: string; account?: CloudAccount }) {
  return <VStack gap={1}><Link href={`/accounts/${encodeURIComponent(id)}`}>{account?.accountName || id}</Link><CopyValue value={id} label="Platform account ID" isSecondary /></VStack>;
}

const tierOptions = [Tier.TIER_UNSPECIFIED, Tier.TIER_0, Tier.TIER_1, Tier.TIER_2].map((tier) => ({
  value: String(tier), label: tier === Tier.TIER_UNSPECIFIED ? "Server default (Tier 0)" : enumLabel(Tier, tier),
}));

function LabelSummary({ value }: { value: Record<string, string> }) {
  return Object.keys(value).length ? <MetadataList columns="multi">{Object.entries(value).map(([key, entry]) => <MetadataListItem key={key} label={key}>{entry || "Empty value"}</MetadataListItem>)}</MetadataList> : <Text type="supporting">No custom labels.</Text>;
}

function DraftLabelSummary({ json }: { json: string }) {
  let value: Record<string, string> | undefined;
  try { value = fromJson(NetworkSchema, { labels: JSON.parse(json) }).labels; }
  catch { value = undefined; }
  return value ? <LabelSummary value={value} /> : <Text>Return to Identity to correct the label values.</Text>;
}

function DraftSubnetSummary({ json }: { json: string }) {
  let subnets: Subnet[] | undefined;
  try { subnets = fromJson(NetworkLayoutSchema, { subnets: JSON.parse(json) }).subnets; }
  catch { subnets = undefined; }
  return subnets ? <Table data={subnets.map((subnet, id) => ({ id, subnet }))} idKey="id" density="compact" aria-label="Subnets to create" columns={[
      { key: "cidr", header: "CIDR", renderCell: ({ subnet }) => <Text>{subnet.cidrBlock}</Text> },
      { key: "zone", header: "Zone", renderCell: ({ subnet }) => <Text>{subnet.zoneId}</Text> },
      { key: "role", header: "Role", renderCell: ({ subnet }) => <Text>{enumLabel(SubnetRole, subnet.role)}</Text> },
    ]} /> : <Text>Return to Connectivity to correct the explicit subnets.</Text>;
}

function NetworkForm({ accounts, baseline, currentEtag, asRequested, onAccepted, onCancel }: {
  accounts: CloudAccount[];
  baseline?: Network;
  currentEtag?: string;
  asRequested?: boolean;
  onAccepted: (network: Network | undefined, message: string) => void;
  onCancel: () => void;
}) {
  const { api, tenantId, canWrite } = usePlatform();
  const [draft, setDraft] = useState<NetworkDraft>(() => baseline ? networkDraft(baseline) : newNetworkDraft());
  const [review, setReview] = useState<ReviewedNetworkUpdate>();
  const [validation, setValidation] = useState("");
  const errorFocus = useRef<HTMLElement>(null);
  const attempt = useRef<CreateAttempt | undefined>(undefined);
  const mutation = useNetworkMutation();
  const edit = !!baseline;
  const [yamlMode, setYamlMode] = useState(false);
  const createMode = useRef<"guided" | "yaml">("guided");
  const [step, setStep] = useState(0);
  const [labelsOpen, setLabelsOpen] = useState(false);
  const [subnetsOpen, setSubnetsOpen] = useState(false);
  const [flowOpen, setFlowOpen] = useState(false);
  const [loggingOpen, setLoggingOpen] = useState(!!baseline?.spec?.flowLogs?.enabled);
  const steps = edit ? ["Configure", "Review"] : ["Basics", "Networking", "Review"];
  const help = edit ? ["Update network details, rollout tier and optional logging.", "Review the requested changes, then generate a server plan."] : ["Choose the cloud account and region for your network.", "Use a balanced subnet layout and add optional traffic logging.", "Confirm the network and its impact before provisioning."];
  const finalStep = steps.length - 1;
  const account = accounts.find(({ id }) => id === draft.accountId);
  const supported = !!networkProvider(account);
  const locked = mutation.busy || !canWrite;
  const importedFlow = !!baseline?.status?.import;
  const stale = edit && baseline.etag !== currentEtag;
  const change = (next: Partial<NetworkDraft>) => {
    if (locked) return;
    setDraft((previous) => ({ ...previous, ...next }));
    setReview(undefined); setValidation(""); mutation.setError("");
    attempt.current = undefined;
  };
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (locked) return;
    setValidation(""); setReview(undefined);
    try {
      if (step < finalStep) {
        validateNetworkStep(tenantId, draft, edit ? "metadata" : step === 0 ? "identity" : "layout", account, baseline);
        if (edit || step === 1) validateNetworkStep(tenantId, draft, "flowLogs", account, baseline);
        setStep(step + 1); return;
      }
      if (!supported) throw new Error("Select an account with a supported provider. Refresh accounts if the network's account is missing.");
      if (stale) throw new Error("The network changed. Close and reopen the editor to review current values.");
      if (baseline) {
        const request = buildNetworkUpdate(tenantId, baseline, draft);
        void mutation.run(async (signal) => {
          const response = await api.network.updateNetwork(request, { signal });
          if (!signal.aborted) setReview(reviewNetworkUpdate(request, response.plan));
        });
      } else {
        attempt.current = createAttempt(tenantId, draft, attempt.current, () => crypto.randomUUID());
        const request = buildCreateNetwork(tenantId, draft, account, attempt.current.requestId);
        void mutation.run(async (signal) => {
          const response = await api.network.createNetwork(request, { signal });
          if (!signal.aborted) onAccepted(response.network, "Create request accepted. Cloud provisioning continues asynchronously; readiness is checked below.");
        });
      }
    } catch (error) {
      setValidation(errorMessage(error));
      if (step === 0) setLabelsOpen(true);
      if (!edit && step === 1) setSubnetsOpen(true);
      if (step === (edit ? 0 : 1)) { setLoggingOpen(true); setFlowOpen(true); }
      requestAnimationFrame(() => errorFocus.current?.focus());
    }
  };
  const apply = () => {
    if (locked || !review || !supported) return;
    void mutation.run(async (signal) => {
      const request = reviewedNetworkApply(review, currentEtag ?? "");
      // Never rebuild or silently replan here: only the reviewed request is sent.
      const response = await api.network.updateNetwork(request, { signal });
      if (signal.aborted) return;
      if (!response.updateRecorded) throw new Error("The server did not acknowledge recording the update. Nothing is marked ready; refresh before continuing.");
      onAccepted(response.network, "Update recorded. Cloud reconciliation continues asynchronously; readiness requires the observed generation to catch up.");
    });
  };
  return (
    <>
    <WizardDialog isOpen={!yamlMode} onOpenChange={(open) => { if (!open && !mutation.busy && createMode.current === "guided") onCancel(); }} title={edit ? "Edit network" : "Create network"} description={help[step]} steps={steps} step={step} busy={mutation.busy} footer={<HStack gap={2} wrap="wrap">
      <Button label="Cancel" type="button" onClick={onCancel} isDisabled={mutation.busy} />
      <Button label="Back" type="button" onClick={() => { setValidation(""); setStep(step - 1); }} isDisabled={step === 0 || mutation.busy} />
      <Button label={step < finalStep ? "Next" : edit ? "Preview changes" : "Create network"} form="network-write-form" variant={step === finalStep && review ? "secondary" : "primary"} type="submit" isDisabled={!canWrite || stale} isLoading={mutation.busy} />
      {step === finalStep && review && <Button label="Apply reviewed plan" type="button" variant="primary" onClick={apply} isLoading={mutation.busy} isDisabled={!canWrite || !supported || stale || !canApplyNetworkPlan(review.plan)} />}
    </HStack>}>
    <VStack gap={4}>
      {!edit && step === 0 && <HStack gap={2} wrap="wrap"><Button label="Use guided form" type="button" aria-pressed variant="secondary" isDisabled={locked} /><Button label="Paste YAML" type="button" aria-pressed={false} variant="ghost" onClick={() => { createMode.current = "yaml"; setYamlMode(true); }} isDisabled={locked} /></HStack>}
      {edit && <Text type="supporting">{asRequested ? "Editing stored client input (as requested)." : "Stored client input is unavailable; editing the resolved spec."} Name, account, region and layout are immutable.</Text>}
      {stale && <Banner status="warning" title="Network changed" description="Close and reopen this editor to load the current etag. This review cannot be applied." />}
      <form id="network-write-form" method="post" aria-label={edit ? "Edit network fields" : "Create network fields"} onSubmit={submit} noValidate>
        <VStack gap={4}>
          <FormLayout direction="vertical">
            {step === 0 && <>{edit ? <Text>Account: {account?.accountName || draft.accountId} · Region: {draft.region} · Name: {draft.name}</Text> : <>
              <Selector label="Cloud account" htmlName="account" value={draft.accountId} onChange={(accountId) => change({ accountId })} isRequired isDisabled={locked}
                options={accounts.map((entry) => ({ value: entry.id, label: `${entry.accountName || entry.id} · ${networkProvider(entry) || "Unsupported provider"}`, disabled: !networkProvider(entry) }))} />
              {!accounts.length && <Text>No cloud accounts are available. Add an account before creating a network.</Text>}
              <TextInput label="Resource name" htmlName="name" value={draft.name} onChange={(name) => change({ name })} description="Immutable DNS name, up to 40 characters." isRequired isDisabled={locked} autoComplete="off" autoCapitalize="none" hasAutoCorrect={false} hasSpellCheck={false} />
              <TextInput label="Region" htmlName="region" value={draft.region} onChange={(region) => change({ region })} description="Provider region, for example us-east-1. The server validates available zones." isRequired isDisabled={locked} autoComplete="off" autoCapitalize="none" hasAutoCorrect={false} hasSpellCheck={false} />
            </>}
            <TextInput label="Display name" htmlName="displayName" value={draft.displayName} onChange={(displayName) => change({ displayName })} isDisabled={locked} isOptional />
            <Selector label="Network tier" htmlName="tier" description="All clusters on this network inherit its rollout tier." value={String(draft.tier)} onChange={(tier) => change({ tier: Number(tier) })} options={tierOptions} isDisabled={locked} />
            <Collapsible trigger="Optional · resource labels" isOpen={labelsOpen} onOpenChange={setLabelsOpen}><TextArea label="Labels (JSON object)" htmlName="labels" description={'String keys and values, for example {"team":"platform"}. Applied as cloud tags.'} value={draft.labelsJson} onChange={(labelsJson) => change({ labelsJson })} isDisabled={locked} hasSpellCheck={false} /></Collapsible>
            </>}
            {!edit && step === 1 && <>
              <TextInput label="Network CIDR" htmlName="cidr" description="IPv4 /16 to /24. Default subnet carving requires /22 or larger." value={draft.cidrBlock} onChange={(cidrBlock) => change({ cidrBlock })} isRequired isDisabled={locked} />
              <Selector label="NAT mode" htmlName="natMode" value={String(draft.natMode)} onChange={(natMode) => change({ natMode: Number(natMode) })} isDisabled={locked} options={[
                { value: String(NatGatewayMode.SINGLE), label: "Single gateway" }, { value: String(NatGatewayMode.PER_ZONE), label: "Gateway per zone" },
              ]} />
              <TextInput label="Availability zone count" htmlName="zoneCount" description="Default 3; choose 2 to 6. Ignored when explicit subnets are supplied." value={draft.zoneCount} onChange={(zoneCount) => change({ zoneCount })} inputMode="numeric" isDisabled={locked || !!draft.subnetsJson.trim()} />
              <Collapsible trigger="Advanced · explicit subnet layout" isOpen={subnetsOpen} onOpenChange={setSubnetsOpen}><VStack gap={3}><Text type="supporting">Leave this empty to create a public and private subnet in each selected zone.</Text><TextArea label="Advanced: explicit subnets (JSON array)" htmlName="subnets" description={'Generated JSON: [{"role":"SUBNET_ROLE_PUBLIC","cidrBlock":"10.0.0.0/24","zoneId":"use1-az1"}, …]. Include PRIVATE subnets in at least two zones.'} value={draft.subnetsJson} onChange={(subnetsJson) => change({ subnetsJson })} isDisabled={locked} hasSpellCheck={false} rows={5} isOptional /></VStack></Collapsible>
            </>}
            {step === (edit ? 0 : 1) && <Collapsible trigger="Optional · flow logging" isOpen={loggingOpen} onOpenChange={setLoggingOpen}><VStack gap={4}><CheckboxInput label="Enable flow logs" htmlName="flowEnabled" description="Record IP traffic in CloudWatch Logs. Logging may incur provider charges." value={draft.flowEnabled} onChange={(flowEnabled) => change({ flowEnabled })} isDisabled={locked || importedFlow} />
            {importedFlow && <Text type="supporting">Imported flow logs are carried over; their settings cannot be changed here.</Text>}
            {draft.flowEnabled && <>
              <TextInput label="Flow log retention (days)" htmlName="retentionDays" description="CloudWatch period, for example 7, 30, 90 or 365." value={draft.retentionDays} onChange={(retentionDays) => change({ retentionDays })} inputMode="numeric" isDisabled={locked || importedFlow} />
              <Collapsible trigger="Advanced · traffic and aggregation" isOpen={flowOpen} onOpenChange={setFlowOpen}><VStack gap={4}><Selector label="Flow log traffic" htmlName="trafficType" value={String(draft.trafficType)} onChange={(trafficType) => change({ trafficType: Number(trafficType) })} isDisabled={locked || importedFlow} options={[FlowLogTrafficType.ALL, FlowLogTrafficType.ACCEPT, FlowLogTrafficType.REJECT].map((value) => ({ value: String(value), label: enumLabel(FlowLogTrafficType, value) }))} /><Selector label="Flow log aggregation" htmlName="aggregationSeconds" value={draft.aggregationSeconds} onChange={(aggregationSeconds) => change({ aggregationSeconds })} isDisabled={locked || importedFlow} options={[{ value: "60", label: "60 seconds" }, { value: "600", label: "600 seconds" }]} /></VStack></Collapsible>
              {edit && <Text type="supporting">To change traffic type or aggregation, turn logs off and apply first, then enable them with the new settings.</Text>}
            </>}
            </VStack></Collapsible>}
            {step === finalStep && <VStack gap={5}>
              <VStack gap={3}><Heading level={3}>Network</Heading><MetadataList columns="multi">
                <MetadataListItem label="Name">{draft.name}</MetadataListItem><MetadataListItem label="Display name">{draft.displayName || draft.name}</MetadataListItem>
                <MetadataListItem label="Provider">{networkProvider(account) === "aws" ? "Amazon Web Services" : "Unsupported"}</MetadataListItem><MetadataListItem label="Account">{account?.accountName || draft.accountId}</MetadataListItem>
                <MetadataListItem label="Region">{draft.region}</MetadataListItem><MetadataListItem label="Rollout tier">{enumLabel(Tier, draft.tier)}</MetadataListItem>
              </MetadataList></VStack><Divider />
              {!edit && <VStack gap={3}><Heading level={3}>Connectivity</Heading><MetadataList columns="multi"><MetadataListItem label="Address space">{draft.cidrBlock}</MetadataListItem><MetadataListItem label="NAT topology">{enumLabel(NatGatewayMode, draft.natMode)}</MetadataListItem><MetadataListItem label="Availability zones">{draft.subnetsJson.trim() ? "Defined by explicit subnets" : draft.zoneCount}</MetadataListItem></MetadataList>{draft.subnetsJson.trim() && <DraftSubnetSummary json={draft.subnetsJson} />}</VStack>}
              <VStack gap={3}><Heading level={3}>Observability</Heading><MetadataList columns="multi"><MetadataListItem label="Flow logs">{draft.flowEnabled ? "Enabled" : "Disabled"}</MetadataListItem>{draft.flowEnabled && <><MetadataListItem label="Traffic">{enumLabel(FlowLogTrafficType, draft.trafficType)}</MetadataListItem><MetadataListItem label="Retention">{draft.retentionDays} days</MetadataListItem><MetadataListItem label="Aggregation">{draft.aggregationSeconds} seconds</MetadataListItem></>}</MetadataList></VStack>
              <Collapsible trigger="Resource labels" defaultIsOpen={false}><DraftLabelSummary json={draft.labelsJson} /></Collapsible>
              <Section variant="muted" padding={4}><VStack gap={2}><Heading level={3}>Impact</Heading><Text>{edit ? "Generate a server plan to inspect the exact changes before applying. The account, region and address layout remain fixed." : `Creates a VPC with ${draft.natMode === NatGatewayMode.PER_ZONE ? "a NAT gateway in every zone" : "one shared NAT gateway"}. Cloud resources may incur provider charges while provisioning and running.`}</Text><Text type="supporting">Clusters inherit this network’s tier. Release channels remain a per-cluster setting.</Text></VStack></Section>
            </VStack>}
          </FormLayout>
          <VStack ref={errorFocus} tabIndex={-1} role="alert" gap={1}>{validation && <Text>{validation}</Text>}</VStack>
          <ErrorNotice error={mutation.error} />
        </VStack>
      </form>
      {step === finalStep && review && <VStack gap={4}>
        <PlanReview plan={review.plan} />
      </VStack>}
    </VStack>
    </WizardDialog>
    {!edit && <ManifestCreateDialog kind="Network" isOpen={yamlMode} onOpenChange={(open: boolean) => { if (!open && createMode.current === "yaml") onCancel(); }} onUseForm={() => { createMode.current = "guided"; setYamlMode(false); }} onCreated={(network: Network) => onAccepted(network, "Create request accepted from the reviewed YAML manifest. Cloud provisioning continues asynchronously; open network details to track readiness.")} />}
    </>
  );
}

function ImportNetworkForm({ accounts, onAccepted, onCancel }: {
  accounts: CloudAccount[];
  onAccepted: (network: Network | undefined, message: string) => void;
  onCancel: () => void;
}) {
  const { api, tenantId, canWrite } = usePlatform();
  const [draft, setDraft] = useState(newNetworkImportDraft);
  const [step, setStep] = useState(0);
  const [mode, setMode] = useState<"guided" | "yaml">("guided");
  const [manifestVisited, setManifestVisited] = useState(false);
  const [validation, setValidation] = useState("");
  const errorFocus = useRef<HTMLElement>(null);
  const attempt = useRef<CreateAttempt | undefined>(undefined);
  const mutation = useNetworkMutation();
  const locked = !canWrite || mutation.busy;
  const change = (patch: Partial<NetworkImportDraft>) => {
    if (locked) return;
    setDraft((previous) => ({ ...previous, ...patch }));
    attempt.current = undefined; setValidation(""); mutation.setError("");
  };
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (locked) return;
    setValidation("");
    try {
      if (step < 2) {
        validateNetworkImportStep(tenantId, draft, step === 0 ? "identity" : "resources", accounts.find(({ id }) => id === draft.accountId));
        setStep(step + 1); return;
      }
      attempt.current = createAttempt(tenantId, draft, attempt.current, () => crypto.randomUUID());
      const request = buildImportNetwork(tenantId, draft, accounts.find(({ id }) => id === draft.accountId), attempt.current.requestId);
      void mutation.run(async (signal) => {
        const response = await api.network.importNetwork(request, { signal });
        if (!signal.aborted) onAccepted(response.network, "Import request accepted for observe-only discovery. No cloud takeover was requested. Review the controller's import plan when discovery finishes.");
      });
    } catch (error) {
      setValidation(errorMessage(error));
      requestAnimationFrame(() => errorFocus.current?.focus());
    }
  };
  return <><WizardDialog isOpen={mode === "guided"} onOpenChange={(open) => { if (!open && !mutation.busy) onCancel(); }} title="Import existing network" description={["Choose the account and region of your existing VPC.", "Select the VPC and the public and private subnets to discover.", "Review the discovery request. Ownership is confirmed separately."][step]} steps={["Identity", "Cloud resources", "Review"]} step={step} busy={mutation.busy} footer={<HStack gap={2} wrap="wrap">
    <Button label="Cancel import" type="button" onClick={onCancel} isDisabled={mutation.busy} />
    <Button label="Back" type="button" onClick={() => { setValidation(""); setStep(step - 1); }} isDisabled={step === 0 || mutation.busy} />
    <Button label={step < 2 ? "Next" : "Import for discovery"} form="network-import-form" type="submit" variant="primary" isDisabled={!canWrite} isLoading={mutation.busy} />
  </HStack>}><VStack gap={4}>
    {step === 0 && <HStack gap={3} wrap="wrap" hAlign="between" vAlign="center"><Text type="label">Use guided form</Text><Button type="button" label="Paste YAML" variant="secondary" isDisabled={locked} onClick={() => { setManifestVisited(true); setMode("yaml"); }} /></HStack>}
    {step === 0 && <Text type="supporting">Import starts read-only discovery. The platform will not take ownership until you review the discovered resources and confirm takeover.</Text>}
    <form id="network-import-form" method="post" aria-label="Import network fields" onSubmit={submit} noValidate>
      <VStack gap={4}>
        <FormLayout direction="vertical">
          {step === 0 && <>
          <Selector label="Import cloud account" htmlName="importAccount" value={draft.accountId} onChange={(accountId) => change({ accountId })} isDisabled={locked} isRequired options={accounts.map((entry) => ({ value: entry.id, label: `${entry.accountName || entry.id} · ${networkProvider(entry) || "Unsupported provider"}`, disabled: !networkProvider(entry) }))} />
          {!accounts.length && <Text>No cloud accounts are available. Add an account before importing a network.</Text>}
          <TextInput label="Imported resource name" htmlName="importName" description="Immutable DNS name, up to 40 characters." value={draft.name} onChange={(name) => change({ name })} isDisabled={locked} isRequired autoComplete="off" autoCapitalize="none" hasAutoCorrect={false} hasSpellCheck={false} />
          <TextInput label="Imported display name" htmlName="importDisplayName" value={draft.displayName} onChange={(displayName) => change({ displayName })} isDisabled={locked} isOptional />
          <TextInput label="Import region" htmlName="importRegion" description="AWS region holding the VPC, for example us-east-1." value={draft.region} onChange={(region) => change({ region })} isDisabled={locked} isRequired autoComplete="off" autoCapitalize="none" hasAutoCorrect={false} hasSpellCheck={false} />
          </>}
          {step === 1 && <>
          <TextInput label="AWS VPC ID" htmlName="importVpcId" description="The VPC must belong to the selected account." value={draft.vpcId} onChange={(vpcId) => change({ vpcId })} isDisabled={locked} isRequired autoComplete="off" autoCapitalize="none" hasAutoCorrect={false} hasSpellCheck={false} />
          <TextArea label="AWS subnet IDs" htmlName="importSubnetIds" description="1 to 32 unique IDs separated by commas or whitespace. Include the public and private subnets to use. Other VPC subnets stay with their current owner; discovery verifies membership and routes." value={draft.subnetIds} onChange={(subnetIds) => change({ subnetIds })} isDisabled={locked} isRequired hasSpellCheck={false} autoCapitalize="none" hasAutoCorrect={false} />
          </>}
          {step === 2 && <VStack gap={5}>
            <VStack gap={3}><Heading level={3}>Network</Heading><MetadataList columns="multi">
              <MetadataListItem label="Name">{draft.name}</MetadataListItem><MetadataListItem label="Display name">{draft.displayName || draft.name}</MetadataListItem>
              <MetadataListItem label="Provider">Amazon Web Services</MetadataListItem><MetadataListItem label="Cloud account">{accounts.find(({ id }) => id === draft.accountId)?.accountName || draft.accountId}</MetadataListItem><MetadataListItem label="Region">{draft.region}</MetadataListItem><MetadataListItem label="VPC">{draft.vpcId}</MetadataListItem>
            </MetadataList></VStack><Divider />
            <VStack gap={3}><Heading level={3}>Subnets to discover</Heading><Table data={draft.subnetIds.split(/[\s,]+/).filter(Boolean).map((id, index) => ({ id: index, subnetId: id }))} idKey="id" density="compact" aria-label="Subnet IDs selected for network discovery" columns={[{ key: "subnetId", header: "Subnet ID" }]} /></VStack>
            <Section variant="muted" padding={4}><VStack gap={2}><Heading level={3}>Discovery only</Heading><Text>Cloud resources remain unchanged. The controller will map the selected resources and publish an import plan for you to review.</Text><Text type="supporting">Review rollout settings after discovery. Each cluster keeps its own release channel.</Text></VStack></Section>
          </VStack>}
        </FormLayout>
        <VStack ref={errorFocus} tabIndex={-1} role="alert" gap={1}>{validation && <Text>{validation}</Text>}</VStack>
        <ErrorNotice error={mutation.error} />
      </VStack>
    </form>
  </VStack></WizardDialog>
  {manifestVisited && <ImportManifestDialog kind="NetworkImport" isOpen={mode === "yaml"} onOpenChange={open => { if (!open) onCancel(); }} onUseForm={() => setMode("guided")} onImported={resource => onAccepted(resource, "Import request accepted from YAML for read-only discovery. Review the discovered plan before a separate takeover.")} />}
  </>;
}

function TakeOverNetworkForm({ network, accounts, onAccepted, onReload }: {
  network: Network; accounts: CloudAccount[];
  onAccepted: (network: Network | undefined, message: string) => void;
  onReload: () => void;
}) {
  const { api, tenantId, canWrite } = usePlatform();
  const [open, setOpen] = useState(false);
  const reviewKey = networkTakeoverKey(network);
  const [confirmation, setConfirmation] = useState<NetworkTakeoverConfirmation>({ reviewKey, ownershipConfirmation: "", acknowledgeNotImported: false });
  const [validation, setValidation] = useState("");
  const errorFocus = useRef<HTMLElement>(null);
  const mutation = useNetworkMutation();
  const account = accounts.find(({ id }) => id === network.spec?.bootstrapId);
  const blocked = networkTakeoverBlockReason(tenantId, network, account);
  const current = confirmation.reviewKey === reviewKey;
  const consent = current ? confirmation : { reviewKey, ownershipConfirmation: "", acknowledgeNotImported: false };
  const notImported = network.status?.import?.resources.filter(({ disposition }) => disposition === Disposition.NOT_IMPORTED) ?? [];
  const locked = !canWrite || mutation.busy || !!blocked;
  const change = (patch: Partial<NetworkTakeoverConfirmation>) => {
    if (locked) return;
    setConfirmation({ ...consent, ...patch }); setValidation(""); mutation.setError("");
  };
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (locked) return;
    setValidation("");
    try {
      buildTakeOverNetwork(tenantId, network, account, confirmation, canWrite);
      void mutation.run(async (signal) => {
        const current = (await api.network.getNetwork({ tenantId, networkId: network.id }, { signal })).network;
        signal.throwIfAborted();
        if (!current || current.tenantId !== tenantId || current.id !== network.id || networkTakeoverKey(current) !== reviewKey) {
          setConfirmation({ reviewKey: "", ownershipConfirmation: "", acknowledgeNotImported: false });
          onReload();
          throw new Error("The network or import plan changed. Network details are being reloaded; review takeover and confirm ownership again.");
        }
        const request = buildTakeOverNetwork(tenantId, current, account, confirmation, canWrite);
        const response = await api.network.takeOverNetwork(request, { signal });
        if (!signal.aborted) { setOpen(false); onAccepted(response.network, "Takeover request accepted. The controller will acquire management and converge the reviewed import plan asynchronously. Cloud readiness has not yet been confirmed."); }
      });
    } catch (error) {
      setValidation(errorMessage(error));
      requestAnimationFrame(() => errorFocus.current?.focus());
    }
  };
  return <>
    <Button label="Take over imported network" onClick={() => { setConfirmation({ reviewKey, ownershipConfirmation: "", acknowledgeNotImported: false }); setOpen(true); }} isDisabled={!canWrite || !!blocked} />
    {blocked && <Text type="supporting">{blocked}</Text>}
    <WizardDialog isOpen={open} onOpenChange={setOpen} title="Take over imported network" steps={["Confirm ownership"]} step={0} busy={mutation.busy} footer={<HStack gap={2} wrap="wrap">
      <Button label="Cancel takeover" onClick={() => setOpen(false)} isDisabled={mutation.busy} />
      <Button label="Confirm network takeover" form="network-takeover-form" type="submit" variant="primary" isDisabled={locked || !current || consent.ownershipConfirmation !== `take over ${network.name}` || (!!notImported.length && !consent.acknowledgeNotImported)} isLoading={mutation.busy} />
    </HStack>}><VStack gap={4}>
    <Text>Review every resource, change and replacement in the import preview below. Takeover lets the platform manage these cloud resources and converge the plan. CARRIED_OVER resources become platform-owned; NOT_IMPORTED resources stay outside platform management.</Text>
    <Text>Stop the previous tool from managing these resources before confirming ownership, for example by removing their Terraform state entries. Confirm only after every NOT_IMPORTED resource has another owner or was deleted.</Text>
    <ImportedNetworkInventory resources={network.status?.import?.resources ?? []} label="Resources reviewed for network takeover" />
    {blocked && <Banner status="warning" title="Takeover unavailable" description={blocked} />}
    {!current && <Text role="status">The import plan changed. Review the current preview and enter your confirmation again.</Text>}
    <form id="network-takeover-form" method="post" aria-label="Confirm network ownership" onSubmit={submit} noValidate>
      <VStack gap={3}>
        <TextInput label={`Type take over ${network.name} to confirm ownership`} htmlName="takeoverConfirmation" description="This confirms that the previous tool no longer manages these resources." value={consent.ownershipConfirmation} onChange={(ownershipConfirmation) => change({ ownershipConfirmation })} isDisabled={locked} autoComplete="off" autoCapitalize="none" hasAutoCorrect={false} hasSpellCheck={false} />
        {!!notImported.length && <CheckboxInput label="I confirm every NOT_IMPORTED resource has another owner or was deleted, and the previous tool no longer manages anything here" htmlName="acknowledgeNotImported" value={consent.acknowledgeNotImported} onChange={(acknowledgeNotImported) => change({ acknowledgeNotImported })} isDisabled={locked} description={`${notImported.length} resources remain outside platform management; inspect their details in the preview above.`} />}
        <VStack ref={errorFocus} tabIndex={-1} role="alert" gap={1}>{validation && <Text>{validation}</Text>}</VStack>
        <ErrorNotice error={mutation.error} />
      </VStack>
    </form>
  </VStack></WizardDialog></>;
}

function NetworkEditor({ networkId, currentEtag, accounts, onAccepted, onCancel }: {
  networkId: string; currentEtag: string; accounts: CloudAccount[];
  onAccepted: (network: Network | undefined, message: string) => void; onCancel: () => void;
}) {
  const { api, tenantId } = usePlatform();
  const load = useCallback((signal: AbortSignal) => api.network.getNetwork({ tenantId, networkId, asRequested: true }, { signal }), [api, tenantId, networkId]);
  const query = useAsyncQuery(load);
  return <VStack gap={4}>
    {query.loading && <Text role="status">Loading stored input for editing…</Text>}
    <ErrorNotice error={query.error} />
    {query.error && <Button label="Retry loading editor" onClick={query.refresh} />}
    {query.data?.network && <NetworkForm baseline={query.data.network} asRequested={query.data.asRequested} currentEtag={currentEtag} accounts={accounts} onAccepted={onAccepted} onCancel={onCancel} />}
    {!query.loading && !query.error && !query.data?.network && <Text>The server returned no network to edit.</Text>}
    {!query.data?.network && <Button label="Cancel edit" onClick={onCancel} />}
  </VStack>;
}

function NetworkConditions({ conditions }: { conditions: Condition[] }) {
  const rows = conditions.map((condition, id) => ({ id, condition }));
  return rows.length ? <Table searchable data={rows} idKey="id" aria-label="Network conditions" columns={[
      { key: "type", header: "Check", renderCell: ({ condition }) => <Text type="label" maxLines={1}>{condition.type}</Text> },
      { key: "status", header: "Status", renderCell: ({ condition }) => <StatusValue tone={condition.status === ConditionStatus.TRUE ? "success" : condition.status === ConditionStatus.FALSE ? "error" : "neutral"} label={enumLabel(ConditionStatus, condition.status)} /> },
      { key: "message", header: "Observation", renderCell: ({ condition }) => <Text maxLines={1}>{condition.message || condition.reason || "No details"}</Text> },
      { key: "time", header: "Updated", renderCell: ({ condition }) => <Text type="supporting" maxLines={1}>{time(condition.lastTransitionTime)}</Text> },
    ]} /> : <Text type="supporting">Health checks will appear after the first controller observation.</Text>;
}

function ImportedNetworkInventory({ resources, label }: { resources: ImportedResource[]; label: string }) {
  return resources.length ? <Table data={resources.map((resource, id) => ({ id, resource }))} idKey="id" density="compact" textOverflow="wrap" aria-label={label} columns={[
    { key: "resource", header: "Name", renderCell: ({ resource }) => resource.name ? <CopyValue value={resource.name} label="Imported resource name" /> : <Text maxLines={1}>Not managed</Text> },
    { key: "kind", header: "Kind", renderCell: ({ resource }) => <Text maxLines={1} hasTruncateTooltip>{resource.kind || "Not reported"}</Text> },
    { key: "apiVersion", header: "API version", renderCell: ({ resource }) => <Text maxLines={1} hasTruncateTooltip>{resource.apiVersion || "Not reported"}</Text> },
    { key: "external", header: "External ID", renderCell: ({ resource }) => resource.externalName ? <CopyValue value={resource.externalName} label="Imported cloud resource ID" /> : <Text>Not reported</Text> },
    { key: "disposition", header: "Disposition", renderCell: ({ resource }) => <Text maxLines={1}>{enumLabel(Disposition, resource.disposition)}</Text> },
    { key: "path", header: "Spec path", renderCell: ({ resource }) => <Text maxLines={1} hasTruncateTooltip>{resource.specPath || "Carried outside the spec"}</Text> },
    { key: "detail", header: "Impact", renderCell: ({ resource }) => <Text maxLines={1} hasTruncateTooltip>{resource.detail || "No change details reported."}</Text> },
    { key: "replacement", header: "Replacement", renderCell: ({ resource }) => <StatusValue label={resource.requiresReplacement ? "Required" : "No"} tone={resource.requiresReplacement ? "warning" : "neutral"} /> },
  ]} /> : <Text>No imported resources reported.</Text>;
}

function NetworkEvents({ networkId }: { networkId: string }) {
  const { api, tenantId } = usePlatform();
  const [tokens, setTokens] = useState([""]);
  const [activityFilters, setActivityFilters] = useState<ActivityFilters>({ search: "", kind: "all", onlyWarnings: false });
  const filteringActivity = !!activityFilters.search.trim() || activityFilters.kind !== "all" || activityFilters.onlyWarnings;
  const [requestedEventKinds, setRequestedEventKinds] = useState(true);
  const loadCompleteEvents = filteringActivity || requestedEventKinds;
  const token = tokens[tokens.length - 1];
  const load = useCallback((signal: AbortSignal) => api.network.listNetworkEvents({ tenantId, networkId, pageSize: 50, pageToken: token }, { signal }), [api, tenantId, networkId, token]);
  const query = useAsyncQuery(load, { pollMs: token || filteringActivity ? undefined : 5000 });
  const loadAll = useCallback(async (signal: AbortSignal) => {
    if (!loadCompleteEvents) return undefined;
    return collectPages(async pageToken => {
      const page = await api.network.listNetworkEvents({ tenantId, networkId, pageSize: 200, pageToken }, { signal });
      return { items: page.events, nextPageToken: page.nextPageToken };
    }, signal);
  }, [api, tenantId, networkId, loadCompleteEvents]);
  const allEvents = useAsyncQuery(loadAll);
  const activityQuery = filteringActivity ? allEvents : query;
  const refreshActivity = () => {
    if (!filteringActivity) query.refresh();
    if (loadCompleteEvents) allEvents.refresh();
  };
  const events = filteringActivity ? allEvents.data ?? [] : query.data?.events ?? [];
  const availableEventKinds = [...(query.data?.events ?? []), ...(allEvents.data ?? [])];
  const filteredEvents = filterActivityEvents(events, activityFilters);
  const next = query.data?.nextPageToken;
  const repeated = !!next && tokens.includes(next);
  const busy = query.loading || query.refreshing;
  return <VStack gap={5}>
    <HStack gap={3} wrap="wrap" hAlign="between" vAlign="center">
      <VStack gap={1}><Heading level={3}>Activity</Heading><Text type="supporting">Controller events, newest first.</Text></VStack>
      <RefreshButton label="Refresh activity" onClick={refreshActivity} isLoading={activityQuery.refreshing || allEvents.refreshing} />
    </HStack>
    <ActivityEventFilters value={activityFilters} onChange={value => { setActivityFilters(value); setTokens([""]); }} events={availableEventKinds} onRequestKinds={() => setRequestedEventKinds(true)} />
    {loadCompleteEvents && allEvents.loading && !filteringActivity && <Text role="status">Loading all event kinds and pages…</Text>}
    {!filteringActivity && allEvents.error && <Banner status="warning" title="Complete event kinds unavailable" description={`${allEvents.error} Refresh activity to retry loading all available events.`} />}
    <Text type="supporting" role="status">{activityQuery.loading ? filteringActivity ? "Loading all available event pages…" : "Loading events…" : `${filteredEvents.length} of ${events.length} events ${filteringActivity ? "across all available pages" : "on this page"}`} · Times shown in UTC.{!filteringActivity && " Use pagination to check other event pages."}</Text>
    <ErrorNotice error={activityQuery.error} />
    {activityQuery.data && <ActivityEventTable events={filteredEvents} title="Network events" emptyMessage={filteringActivity ? "No events match the activity filters." : "No events were reported."} />}
    {!filteringActivity && repeated && <Text>Pagination stopped: the server repeated an event page token.</Text>}
    {!filteringActivity && <HStack gap={2} wrap="wrap" hAlign="end" vAlign="center">
      <Text type="supporting">Page {tokens.length}</Text>
      <Button label="Previous events" onClick={() => setTokens((values) => values.slice(0, -1))} isDisabled={tokens.length === 1 || busy} />
      <Button label="Next events" onClick={() => next && setTokens((values) => [...values, next])} isDisabled={!next || repeated || busy} />
    </HStack>}
  </VStack>;
}

function DeleteNetwork({ network, onAccepted }: { network: Network; onAccepted: (network: Network | undefined, message: string) => void }) {
  const { api, tenantId, canWrite } = usePlatform();
  const [open, setOpen] = useState(false);
  const [confirmation, setConfirmation] = useState({ etag: network.etag, text: "" });
  const mutation = useNetworkMutation();
  const dependencies = network.status?.clusterIds ?? [];
  const confirmed = confirmation.etag === network.etag && confirmation.text === network.name;
  const disabled = !canWrite || !network.etag || !!dependencies.length || network.status?.state === NetworkState.DELETING;
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (disabled || !confirmed || mutation.busy) return;
    void mutation.run(async (signal) => {
      const response = await api.network.deleteNetwork({ tenantId, networkId: network.id, etag: network.etag }, { signal });
      if (!signal.aborted) { setOpen(false); onAccepted(response.network, "Deletion request accepted. Removal is asynchronous; refresh inventory to confirm completion."); }
    });
  };
  return <>
    <Button label="Delete network" variant="destructive" tooltip={dependencies.length ? "Remove dependent clusters before deleting this network." : "Delete this network and its owned resources"} onClick={() => { setConfirmation({ etag: network.etag, text: "" }); setOpen(true); }} isDisabled={disabled} />
    <WizardDialog isOpen={open} onOpenChange={setOpen} title="Delete network" steps={["Confirm deletion"]} step={0} busy={mutation.busy} footer={<HStack gap={2} wrap="wrap">
      <Button label="Cancel deletion" onClick={() => setOpen(false)} isDisabled={mutation.busy} />
      <Button label="Delete network and owned resources" form="network-delete-form" variant="destructive" type="submit" isDisabled={disabled || !confirmed} isLoading={mutation.busy} />
    </HStack>}><VStack gap={4}>
    <Text>{network.status?.state === NetworkState.IMPORTED ? "An import that was never taken over is only forgotten; its cloud resources remain." : "Deleting removes this network and its owned cloud resources. Carried-over resources may be retained according to ownership policy."}</Text>
    <Text>Dependent clusters: {dependencies.length ? dependencies.join(", ") : "None reported"}. Networks cannot be deleted while clusters use them.</Text>
    <form id="network-delete-form" method="post" aria-label="Confirm network deletion" onSubmit={submit}>
      <VStack gap={3}>
        <TextInput label={`Type ${network.name} to confirm deletion`} htmlName="deleteConfirmation" value={confirmation.etag === network.etag ? confirmation.text : ""} onChange={(text) => setConfirmation({ etag: network.etag, text })} isDisabled={disabled || mutation.busy} autoComplete="off" hasSpellCheck={false} />
        <ErrorNotice error={mutation.error} />
      </VStack>
    </form>
  </VStack></WizardDialog></>;
}

function RelatedNetworkClusters({ network }: { network: Network }) {
  const { api, tenantId } = usePlatform();
  const networkId = network.id;
  const load = useCallback((signal: AbortSignal) => collectNetworkClusters(networkId,
    (pageToken) => api.cluster.listClusters({ tenantId, pageSize: 200, pageToken }, { signal }), signal), [api, tenantId, networkId]);
  const query = useAsyncQuery(load);
  const rows = networkClusterRows(networkId, network.status?.clusterIds ?? [], query.data,
    query.data !== undefined && !query.loading && !query.error);
  return <VStack gap={3}>
    <Heading level={3}>Related clusters</Heading>
    <ErrorNotice error={query.error} />
    {query.error && <Text>The cluster list could not be refreshed. Known clusters and unresolved status references are shown below; references may be names rather than platform IDs.</Text>}
    {query.loading && <Text role="status">Loading related cluster names and state…</Text>}
    {rows.length ? <Table data={rows} idKey="key" density="compact" textOverflow="wrap" aria-label="Clusters belonging to this network" columns={[
      { key: "name", header: "Cluster", renderCell: ({ reference, cluster }) => cluster ? <HStack gap={1} vAlign="center"><Link href={`/clusters/${encodeURIComponent(cluster.id)}`} maxLines={1}>{cluster.displayName || cluster.name || cluster.id}</Link><CopyButton value={cluster.name} label="Cluster resource name" /></HStack> : <CopyValue value={reference} label="Cluster status reference" /> },
      { key: "id", header: "ID", renderCell: ({ cluster }) => cluster ? <CopyValue value={cluster.id} label="Cluster ID" /> : <Text type="supporting">Status reference · awaiting refresh</Text> },
      { key: "state", header: "State", renderCell: ({ cluster }) => cluster ? <StatusValue label={enumLabel(ClusterState, cluster.status?.state)} tone={cluster.status?.state === ClusterState.RUNNING ? "success" : cluster.status?.state === ClusterState.FAILED ? "error" : "neutral"} /> : <Text>Not resolved</Text> },
      { key: "version", header: "Kubernetes", renderCell: ({ cluster }) => <Text>{cluster ? cluster.status?.currentKubernetesVersion || "Not reported" : "Not resolved"}</Text> },
      { key: "channel", header: "Release channel", renderCell: ({ cluster }) => <Text>{cluster ? enumLabel(ReleaseChannel, cluster.spec?.releaseChannel) : "Not resolved"}</Text> },
    ]} /> : !query.loading && <Text>{query.error ? "No fallback cluster references reported." : "No related clusters reported."}</Text>}
    <HStack gap={2}><RefreshButton label="Refresh related clusters" onClick={query.refresh} isLoading={query.refreshing} /></HStack>
  </VStack>;
}

function NetworkResourceInventory({ network }: { network: Network }) {
  const [search, setSearch] = useState("");
  const [stateFilter, setStateFilter] = useState("all");
  const [pausedFilter, setPausedFilter] = useState("all");
  const [kindFilter, setKindFilter] = useState("all");
  const resources = network.status?.managedResources ?? [];
  const importResources = network.status?.import?.resources ?? [];
  const carriedResources = network.status?.carriedOver ?? [];
  const kinds = [...new Set([...resources, ...importResources, ...carriedResources].map(({ kind }) => kind))].sort();
  const states = [...new Set(resources.map(({ state }) => state))].sort((a, b) => a - b);
  const filtered = filterNetworkInventory(resources, search).filter((resource) =>
    (stateFilter === "all" || String(resource.state) === stateFilter) &&
    (kindFilter === "all" || resource.kind === kindFilter) &&
    (pausedFilter === "all" || resource.paused === (pausedFilter === "paused")));
  const filterImports = (items: ImportedResource[]) => stateFilter === "all" && pausedFilter === "all"
    ? filterNetworkInventory(items, search).filter(({ kind }) => kindFilter === "all" || kind === kindFilter) : [];
  const imported = filterImports(importResources);
  const carried = filterImports(carriedResources);
  const hasFilters = !!search.trim() || stateFilter !== "all" || pausedFilter !== "all" || kindFilter !== "all";
  const clear = () => { setSearch(""); setStateFilter("all"); setPausedFilter("all"); setKindFilter("all"); };
  return <VStack gap={5}>
    <HStack hAlign="between" vAlign="end" gap={4} wrap="wrap">
      <VStack gap={1}><Heading level={2}>Resource inventory</Heading><Text type="supporting">Every managed resource returned by the platform, including paused and unhealthy resources.</Text></VStack>
      <Text type="supporting" role="status">{filtered.length} of {resources.length} managed resources</Text>
    </HStack>
    <HStack gap={3} wrap="wrap" vAlign="end">
      <TextInput type="search" label="Search resources" description="Name, kind or cloud ID." value={search} onChange={setSearch} autoComplete="off" hasSpellCheck={false} width="min(100%, 24rem)" />
      <Selector label="Resource kind" value={kindFilter} onChange={setKindFilter} width="min(100%, 12rem)" options={[{ value: "all", label: "All kinds" }, ...kinds.map((kind) => ({ value: kind, label: kind || "Unspecified kind" }))]} />
      <Selector label="Resource state" value={stateFilter} onChange={setStateFilter} width="min(100%, 12rem)" options={[{ value: "all", label: "All states" }, ...states.map((state) => ({ value: String(state), label: enumLabel(ManagedResourceState, state) }))]} />
      <Selector label="Pause status" value={pausedFilter} onChange={setPausedFilter} width="min(100%, 12rem)" options={[{ value: "all", label: "Any pause status" }, { value: "paused", label: "Paused" }, { value: "active", label: "Not paused" }]} />
      {hasFilters && <Button label="Clear resource filters" variant="ghost" onClick={clear} />}
    </HStack>
    {filtered.length ? <Table data={filtered.map((resource, id) => ({ id, resource }))} idKey="id" aria-label="Network managed resource inventory" columns={[
      { key: "name", header: "Name", width: proportional(3, { minWidth: 80 }), renderCell: ({ resource }) => <CopyValue value={resource.name} label="Managed resource name" /> },
      { key: "kind", header: "Kind", width: proportional(1, { minWidth: 80 }), renderCell: ({ resource }) => <Text maxLines={1} hasTruncateTooltip>{resource.kind || "Not reported"}</Text> },
      { key: "external", header: "Cloud ID", width: proportional(3, { minWidth: 80 }), renderCell: ({ resource }) => resource.externalName ? <CopyValue value={resource.externalName} label="Cloud resource ID" /> : <Text maxLines={1}>Pending assignment</Text> },
      { key: "state", header: "State", width: proportional(1, { minWidth: 80 }), renderCell: ({ resource }) => <StatusValue tone={resource.state === ManagedResourceState.READY ? "success" : resource.state === ManagedResourceState.FAILED ? "error" : "warning"} label={enumLabel(ManagedResourceState, resource.state)} /> },
      { key: "ready", header: "Ready", width: proportional(1, { minWidth: 80 }), renderCell: ({ resource }) => <Text maxLines={1}>{enumLabel(ConditionStatus, resource.ready)}</Text> },
      { key: "synced", header: "Synced", width: proportional(1, { minWidth: 80 }), renderCell: ({ resource }) => <Text maxLines={1}>{enumLabel(ConditionStatus, resource.synced)}</Text> },
      { key: "paused", header: "Paused", width: proportional(1, { minWidth: 80 }), renderCell: ({ resource }) => <StatusValue tone={resource.paused ? "warning" : "neutral"} label={resource.paused ? "Paused" : "No"} /> },
    ]} /> : <Text>{hasFilters ? "No managed resources match your search and filters." : "Resources will appear here as the controller records them."}</Text>}
    {!!filtered.length && <Collapsible trigger="Resource observation details" defaultIsOpen={false}>
      <Table data={filtered.map((resource, id) => ({ id, resource }))} idKey="id" aria-label="Network resource observation details" columns={[
        { key: "name", header: "Name", width: proportional(2, { minWidth: 80 }), renderCell: ({ resource }) => <CopyValue value={resource.name} label="Observed resource name" /> },
        { key: "zone", header: "Zone", renderCell: ({ resource }) => <Text maxLines={1} hasTruncateTooltip>{resource.zoneId || "Not zonal"}</Text> },
        { key: "role", header: "Role", renderCell: ({ resource }) => <Text maxLines={1} hasTruncateTooltip>{resource.role || "Not reported"}</Text> },
        { key: "generation", header: "Generation", renderCell: ({ resource }) => <Text maxLines={1}>{String(resource.generation)}</Text> },
        { key: "observed", header: "Observed", renderCell: ({ resource }) => <Text maxLines={1}>{String(resource.observedGeneration)}</Text> },
        { key: "reason", header: "Reason", renderCell: ({ resource }) => <Text maxLines={1} hasTruncateTooltip>{resource.reason || "Not reported"}</Text> },
        { key: "message", header: "Message", width: proportional(3, { minWidth: 80 }), renderCell: ({ resource }) => <Text maxLines={1} hasTruncateTooltip>{resource.message || "No issues reported"}</Text> },
        { key: "deletion", header: "Deletion requested", width: proportional(2, { minWidth: 80 }), renderCell: ({ resource }) => <Text maxLines={1} hasTruncateTooltip>{time(resource.deletionTime)}</Text> },
      ]} />
    </Collapsible>}
    {network.status?.import && <VStack gap={3}><Divider /><HStack gap={2} wrap="wrap"><Heading level={3}>Import discovery</Heading><Text type="supporting">{enumLabel(ImportPhase, network.status.import.phase)} · {imported.length} matching resources</Text></HStack>{(stateFilter !== "all" || pausedFilter !== "all") && <Text type="supporting">Import previews do not report managed state or pause status. Clear those filters to inspect them.</Text>}<ImportedNetworkInventory resources={imported} label="Imported network resources" /></VStack>}
    {!!carriedResources.length && <VStack gap={3}><Divider /><Heading level={3}>Carried-over resources</Heading><ImportedNetworkInventory resources={carried} label="Carried-over network resource inventory" /></VStack>}
  </VStack>;
}

function NetworkConfiguration({ network, account }: { network: Network; account?: CloudAccount }) {
  const spec = network.spec;
  return <VStack gap={6}>
    <VStack gap={3}><Heading level={2}>Network configuration</Heading><Text type="supporting">Account, region and address layout are fixed after creation.</Text>
      <MetadataList columns="multi">
        <MetadataListItem label="Provider">{networkProvider(account) === "aws" ? "Amazon Web Services" : "Unknown or unsupported"}</MetadataListItem>
        <MetadataListItem label="Cloud account">{spec?.bootstrapId ? <AccountReference id={spec.bootstrapId} account={account} /> : "Not reported"}</MetadataListItem>
        <MetadataListItem label="Region">{spec?.region || "Not reported"}</MetadataListItem>
        <MetadataListItem label="Network CIDR">{spec?.layout?.cidrBlock || "Server default"}</MetadataListItem>
        <MetadataListItem label="NAT topology">{enumLabel(NatGatewayMode, spec?.layout?.natGatewayMode)}</MetadataListItem>
        <MetadataListItem label="Rollout tier">{enumLabel(Tier, spec?.tier)}</MetadataListItem>
        <MetadataListItem label="Flow logs">{spec?.flowLogs?.enabled ? `${enumLabel(FlowLogTrafficType, spec.flowLogs.trafficType)} · ${spec.flowLogs.retentionDays || 30} days retention` : "Disabled"}</MetadataListItem>
        <MetadataListItem label="Created">{time(network.createTime)}</MetadataListItem>
      </MetadataList>
    </VStack>
    <Divider />
    <VStack gap={3}><Heading level={3}>Subnet layout</Heading>
      {spec?.layout?.subnets.length ? <Table searchable data={spec.layout.subnets.map((subnet, id) => ({ id, subnet }))} idKey="id" density="compact" aria-label="Network subnet configuration" columns={[
        { key: "cidr", header: "CIDR", renderCell: ({ subnet }) => <Text>{subnet.cidrBlock}</Text> },
        { key: "zone", header: "Zone", renderCell: ({ subnet }) => <Text>{subnet.zoneId || "Regional"}</Text> },
        { key: "role", header: "Role", renderCell: ({ subnet }) => <Text>{enumLabel(SubnetRole, subnet.role)}</Text> },
      ]} /> : <Text type="supporting">The controller resolves the subnet layout from the requested availability zones.</Text>}
    </VStack>
    <VStack gap={3}><Heading level={3}>Labels</Heading><LabelSummary value={network.labels} /></VStack>
    <Divider />
    <Collapsible trigger="Advanced · raw configuration and revision" defaultIsOpen={false}>
      <VStack gap={4}><MetadataList><MetadataListItem label="Resource name"><CopyValue value={network.name} label="Network resource name" /></MetadataListItem><MetadataListItem label="Resource ID"><CopyValue value={network.id} label="Network ID" /></MetadataListItem><MetadataListItem label="Etag"><CopyValue value={network.etag} label="Network etag" /></MetadataListItem><MetadataListItem label="Generation">{String(network.generation)}</MetadataListItem></MetadataList><TextArea label="Resolved network JSON" value={networkJson(network)} isReadOnly rows={8} hasSpellCheck={false} /></VStack>
    </Collapsible>
  </VStack>;
}

function NetworkDetails({ networkId, accounts }: { networkId: string; accounts: CloudAccount[] }) {
  const { api, tenantId, canWrite } = usePlatform();
  const [editing, setEditing] = useState(false);
  const [notice, setNotice] = useState("");
  const [tab, setTab] = useState("overview");
  const panelId = useId();
  const load = useCallback((signal: AbortSignal) => api.network.getNetwork({ tenantId, networkId }, { signal }), [api, tenantId, networkId]);
  const query = useAsyncQuery(load, { pollMs: 5000 });
  const network = query.data?.network;
  const accepted = (_network: Network | undefined, message: string) => { setNotice(message); setEditing(false); query.refresh(); };
  const status = network?.status;
  const account = accounts.find(({ id }) => id === network?.spec?.bootstrapId);
  const resources = status?.managedResources ?? [];
  const ready = resources.filter((resource) => resource.state === ManagedResourceState.READY && resource.ready === ConditionStatus.TRUE && resource.observedGeneration >= resource.generation).length;
  const paused = resources.filter((resource) => resource.paused).length;
  const healthy = network ? networkIsReady(network) : false;
  const state = status?.state === NetworkState.READY && !healthy ? "Reconciling" : enumLabel(NetworkState, status?.state);
  const tabs = [{ value: "overview", label: "Overview" }, { value: "resources", label: "Resources" }, { value: "activity", label: "Activity" }, { value: "configuration", label: "Configuration" }];
  return <VStack gap={6}>
    <DetailHeader title={network?.displayName || network?.name || "Network details"}
      metadata={<><HStack gap={2} vAlign="center"><ProviderIcon provider={networkProvider(account) || ""} size="sm" /><Text type="supporting" hasCapsize>{networkProvider(account) === "aws" ? "Amazon Web Services" : "Cloud network"}</Text></HStack><Text type="supporting">{network?.spec?.region || "Region pending"}</Text>{network && <CopyValue value={network.name} label="Network resource name" isSecondary />}</>}
      status={network && <StatusValue tone={healthy ? "success" : status?.state === NetworkState.FAILED ? "error" : "warning"} label={state} />}
      actions={<>
            {network && canWrite && <Button label="Edit network" onClick={() => setEditing(true)} isDisabled={editing || !network.etag || !networkProvider(account) || status?.state === NetworkState.IMPORTED || status?.state === NetworkState.DELETING} />}
            <RefreshButton label="Refresh details" onClick={query.refresh} isLoading={query.refreshing} />
            {network && <ResourceYamlButton kind="Network" resourceId={network.id} />}
            {network && canWrite && <Button label={status?.freeze && !status.freeze.letThrough ? "Unfreeze network" : "Freeze network"} variant={status?.freeze && !status.freeze.letThrough ? "primary" : "destructive"} href={`/freeze?scope=network&id=${encodeURIComponent(network.id)}`} />}
            {network && canWrite && <DeleteNetwork network={network} onAccepted={accepted} />}
            {network && canWrite && status?.import && (status.state === NetworkState.IMPORTED || status.state === NetworkState.FAILED) && <TakeOverNetworkForm network={network} accounts={accounts} onAccepted={accepted} onReload={query.refresh} />}
      </>} />
    {notice && <Banner status="info" title="Request accepted" description={notice} />}
    <ErrorNotice error={query.error} />
    {query.loading && <Text role="status">Loading network health and resources…</Text>}
    {network && <>
      <DetailSummary items={[
        { label: "Managed resources", value: resources.length, note: `${paused} paused` },
        { label: "Resources ready", value: `${ready} / ${resources.length}`, note: "Observed cloud readiness" },
        { label: "Connected clusters", value: status?.clusterIds.length ?? 0, note: "Share this network" },
        { label: "Rollout tier", value: enumLabel(Tier, network.spec?.tier), note: "Inherited by every cluster" },
      ]} />
      <TabList value={tab} onChange={setTab} role="tablist" aria-label="Network detail sections" hasDivider>
        {tabs.map((item) => <Tab key={item.value} id={`${panelId}-${item.value}-tab`} value={item.value} label={item.label} panelId={`${panelId}-${item.value}-panel`} />)}
      </TabList>
      {tabs.map((item) => item.value === tab ? <VStack key={item.value} id={`${panelId}-${item.value}-panel`} role="tabpanel" aria-labelledby={`${panelId}-${item.value}-tab`} tabIndex={0} gap={6}>
        {tab === "overview" && <>
          <VStack gap={4}>
            <HStack hAlign="between" vAlign="start" gap={4} wrap="wrap"><VStack gap={1}><Heading level={2}>Network health</Heading><Text type="supporting">{healthy ? "The cloud matches the current desired generation." : "Waiting for cloud reconciliation to confirm the desired generation."}</Text></VStack><Text type="supporting">Generation {String(status?.observedGeneration ?? 0)} / {String(network.generation)}</Text></HStack>
            <Text>{status?.message || (healthy ? "All reported checks are current." : "No controller message yet.")}</Text>
            <NetworkConditions conditions={status?.conditions ?? []} />
          </VStack><Divider />
          <VStack gap={3}><Heading level={2}>Network topology</Heading><MetadataList columns="multi">
            <MetadataListItem label="Cloud network">{status?.resources?.cloudNetworkId ? <CopyValue value={status.resources.cloudNetworkId} label="Cloud network ID" /> : "Pending assignment"}</MetadataListItem>
            <MetadataListItem label="Address space">{network.spec?.layout?.cidrBlock || "Server default"}</MetadataListItem>
            <MetadataListItem label="NAT mode">{enumLabel(NatGatewayMode, network.spec?.layout?.natGatewayMode)}</MetadataListItem>
            <MetadataListItem label="Egress IPs">{status?.resources?.egressIps.join(", ") || "Not yet reported"}</MetadataListItem>
            <MetadataListItem label="Cloud account">{network.spec?.bootstrapId ? <AccountReference id={network.spec.bootstrapId} account={account} /> : "Not reported"}</MetadataListItem>
            <MetadataListItem label="Flow logs">{network.spec?.flowLogs?.enabled ? "Enabled" : "Disabled"}</MetadataListItem>
          </MetadataList></VStack>
          <Divider />
          <RelatedNetworkClusters network={network} />
          <Card variant="muted" padding={4}><VStack gap={3}><Heading level={3}>Rollout policy</Heading><Text>All clusters on this network inherit {enumLabel(Tier, network.spec?.tier)}. Release channels are configured on each cluster.</Text>{paused > 0 && <Text>{paused} managed resources are paused; reconciliation waits until they are resumed.</Text>}{status?.freeze && <Text>{status.freeze.letThrough ? "Allowed through the current freeze" : "Network reconciliation is frozen"}{status.freeze.reason ? ` · ${status.freeze.reason}` : ""}{status.freeze.until ? ` · Until ${time(status.freeze.until)}` : ""}</Text>}{status?.codeChange && <MetadataList><MetadataListItem label="Platform update">{enumLabel(CodeChangePhase, status.codeChange.phase)} · {status.codeChange.summary}</MetadataListItem><MetadataListItem label="Rollout observation">{status.codeChange.message || "No additional message"}</MetadataListItem></MetadataList>}{status?.import && <Text>Import phase: {enumLabel(ImportPhase, status.import.phase)}.</Text>}</VStack></Card>
        </>}
        {tab === "resources" && <NetworkResourceInventory network={network} />}
        {tab === "activity" && <VStack gap={6}><NetworkEvents networkId={networkId} /><Collapsible trigger="Persisted recent events" defaultIsOpen={false}><ActivityEventTable events={status?.recentEvents ?? []} title="Persisted recent network events" /></Collapsible></VStack>}
        {tab === "configuration" && <NetworkConfiguration network={network} account={account} />}
      </VStack> : null)}
      <Text type="supporting">Use Refresh for the latest observations. Enable Live updates to refresh every five seconds for up to 15 minutes; Refresh restarts observation.</Text>
      {canWrite && editing && <NetworkEditor networkId={networkId} currentEtag={network.etag} accounts={accounts} onAccepted={accepted} onCancel={() => setEditing(false)} />}
    </>}
  </VStack>;
}

interface NetworkRow extends Record<string, unknown> { id: string; network: Network }

/** Tenant/auth scope changes remount this page in the shared console. */
export function NetworksPage({ resourceId }: { resourceId?: string } = {}) {
  return resourceId ? <NetworkDetailPage key={resourceId} resourceId={resourceId} /> : <NetworksList />;
}

function NetworkDetailPage({ resourceId }: { resourceId: string }) {
  const { api, tenantId } = usePlatform();
  const loadAccounts = useCallback((signal: AbortSignal) => api.tenant.listCloudAccounts({ tenantId }, { signal }), [api, tenantId]);
  const accounts = useAsyncQuery(loadAccounts);
  return <VStack gap={6}>
    <ErrorNotice error={accounts.error} />
    {accounts.error && <RefreshButton label="Refresh accounts" onClick={accounts.refresh} isLoading={accounts.refreshing} />}
    <NetworkDetails networkId={resourceId} accounts={accounts.data?.accounts ?? []} />
  </VStack>;
}

function NetworksList() {
  const { api, tenantId, canWrite } = usePlatform();
  const [page, setPage] = useState(0);
  const [search, setSearch] = useState("");
  const [accountFilter, setAccountFilter] = useState("all");
  const [tierFilter, setTierFilter] = useState("all");
  const [stateFilter, setStateFilter] = useState("all");
  const [acceptedId, setAcceptedId] = useState<string>();
  const [creating, setCreating] = useState(false);
  const [importing, setImporting] = useState(false);
  const [notice, setNotice] = useState("");
  const load = useCallback((signal: AbortSignal) => collectPages(async (pageToken) => {
    const response = await api.network.listNetworks({ tenantId, pageSize: 200, pageToken }, { signal });
    return { items: response.networks, nextPageToken: response.nextPageToken };
  }, signal), [api, tenantId]);
  const loadAccounts = useCallback((signal: AbortSignal) => api.tenant.listCloudAccounts({ tenantId }, { signal }), [api, tenantId]);
  const list = useAsyncQuery(load, { pollMs: 5000 });
  const accounts = useAsyncQuery(loadAccounts);
  const accountMap = new Map(accounts.data?.accounts.map((account) => [account.id, account]) ?? []);
  const allNetworks = list.data ?? [];
  const accountIds = [...new Set([...accountMap.keys(), ...allNetworks.map((network) => network.spec?.bootstrapId ?? "")])].filter(Boolean);
  const tiers = [...new Set([...Object.values(Tier).filter((value): value is Tier => typeof value === "number"), ...allNetworks.map((network) => network.spec?.tier ?? Tier.TIER_UNSPECIFIED)])].sort((a, b) => a - b);
  const states = [...new Set([...Object.values(NetworkState).filter((value): value is NetworkState => typeof value === "number"), ...allNetworks.map((network) => network.status?.state ?? NetworkState.UNSPECIFIED)])].sort((a, b) => a - b);
  const terms = search.trim().toLowerCase().split(/\s+/).filter(Boolean);
  const filtered = allNetworks.filter((network) => {
    const account = accountMap.get(network.spec?.bootstrapId ?? "");
    const text = [network.name, network.displayName, network.id, network.spec?.bootstrapId, network.spec?.region, account?.accountName, account?.provider.case, account?.provider.case === "aws" ? account.provider.value.accountId : ""].join(" ").toLowerCase();
    return terms.every((term) => text.includes(term)) &&
      (accountFilter === "all" || network.spec?.bootstrapId === accountFilter) &&
      (tierFilter === "all" || String(network.spec?.tier ?? Tier.TIER_UNSPECIFIED) === tierFilter) &&
      (stateFilter === "all" || String(network.status?.state ?? NetworkState.UNSPECIFIED) === stateFilter);
  });
  const pageCount = Math.max(1, Math.ceil(filtered.length / 25));
  const currentPage = Math.min(page, pageCount - 1);
  const rows: NetworkRow[] = filtered.slice(currentPage * 25, (currentPage + 1) * 25).map((network) => ({ id: network.id, network }));
  const hasFilters = !!search.trim() || accountFilter !== "all" || tierFilter !== "all" || stateFilter !== "all";
  const columns: TableColumn<NetworkRow>[] = [
    { key: "name", header: "Name", width: proportional(3, { minWidth: 80 }), renderCell: ({ network }) => <HStack gap={1} vAlign="center"><Link href={`/networks/${encodeURIComponent(network.id)}`} maxLines={1} tooltip={network.displayName ? `${network.displayName} (${network.name})` : network.name}>{network.displayName || network.name || network.id}</Link><CopyButton value={network.displayName || network.name || network.id} label="Network name" /></HStack> },
    { key: "id", header: "ID", width: proportional(3, { minWidth: 80 }), renderCell: ({ network }) => <CopyValue value={network.id} label="Network ID" /> },
    { key: "account", header: "Account", width: proportional(2, { minWidth: 80 }), renderCell: ({ network }) => {
      const id = network.spec?.bootstrapId;
      const name = id ? accountMap.get(id)?.accountName || id : "Not reported";
      return id ? <Link href={`/accounts/${encodeURIComponent(id)}`} maxLines={1} tooltip={`${name} · ${id}`}>{name}</Link> : <Text maxLines={1}>{name}</Text>;
    } },
    { key: "region", header: "Region", width: proportional(1, { minWidth: 80 }), renderCell: ({ network }) => <Text maxLines={1} hasTruncateTooltip>{network.spec?.region || "Not reported"}</Text> },
    { key: "tier", header: "Tier", width: proportional(1, { minWidth: 80 }), renderCell: ({ network }) => <Text maxLines={1}>{enumLabel(Tier, network.spec?.tier)}</Text> },
    { key: "state", header: "Status", width: proportional(1, { minWidth: 80 }), renderCell: ({ network }) => <StatusValue label={enumLabel(NetworkState, network.status?.state)} tone={network.status?.state === NetworkState.READY ? "success" : network.status?.state === NetworkState.FAILED ? "error" : "neutral"} /> },
  ];
  const accepted = (network: Network | undefined, message: string) => {
    setNotice(message); setCreating(false); setImporting(false); setAcceptedId(network?.id); list.refresh();
  };
  return <Section variant="transparent" padding={4}><VStack gap={4}>
    <HStack gap={2} wrap="wrap">
      <Heading level={2}>Networks</Heading>
      <RefreshButton label="Refresh networks" onClick={() => { list.refresh(); accounts.refresh(); }} isLoading={list.refreshing || accounts.refreshing} />
      {canWrite && <Button label="Create network" variant="primary" onClick={() => { setCreating(true); setImporting(false); }} isDisabled={creating} />}
      {canWrite && <Button label="Import network" onClick={() => { setImporting(true); setCreating(false); }} isDisabled={importing} />}
    </HStack>
    {!canWrite && <Text type="supporting">Your tenant access is read only.</Text>}
    {notice && <Banner status="info" title="Request accepted" description={notice} />}
    {acceptedId && <Link href={`/networks/${encodeURIComponent(acceptedId)}`}>View network details and progress</Link>}
    <ErrorNotice error={list.error} />
    {list.error && list.data && <Text type="supporting">Showing the last fully loaded network inventory. Refresh failed; no partial pages were added.</Text>}
    <ErrorNotice error={accounts.error} />
    <HStack gap={3} wrap="wrap" vAlign="end">
      <TextInput type="search" label="Search networks" description="Search all tenant networks by name, ID, account or region." value={search} onChange={(value) => { setSearch(value); setPage(0); }} width="min(100%, 24rem)" autoComplete="off" hasSpellCheck={false} />
      <Selector label="Filter by account" value={accountFilter} onChange={(value) => { setAccountFilter(value); setPage(0); }} width="min(100%, 14rem)" options={[{ value: "all", label: "All accounts" }, ...accountIds.map((id) => ({ value: id, label: accountMap.get(id)?.accountName || id }))]} />
      <Selector label="Filter by tier" value={tierFilter} onChange={(value) => { setTierFilter(value); setPage(0); }} width="min(100%, 12rem)" options={[{ value: "all", label: "All tiers" }, ...tiers.map((tier) => ({ value: String(tier), label: enumLabel(Tier, tier) }))]} />
      <Selector label="Filter by status" value={stateFilter} onChange={(value) => { setStateFilter(value); setPage(0); }} width="min(100%, 12rem)" options={[{ value: "all", label: "All statuses" }, ...states.map((state) => ({ value: String(state), label: enumLabel(NetworkState, state) }))]} />
      {hasFilters && <Button label="Clear network filters" variant="ghost" onClick={() => { setSearch(""); setAccountFilter("all"); setTierFilter("all"); setStateFilter("all"); setPage(0); }} />}
    </HStack>
    {list.loading && <Text role="status">Loading all network pages…</Text>}
    {list.data && <Text type="supporting" role="status">{filtered.length} of {allNetworks.length} tenant networks · 25 per page</Text>}
    {list.data && (rows.length ? <Table data={rows} columns={columns} idKey="id" aria-label="Tenant networks" /> : <Text>{hasFilters ? "No networks match your search and filters." : "No networks in this tenant."}</Text>)}
    <HStack gap={2} wrap="wrap">
      <Button label="First page" onClick={() => setPage(0)} isDisabled={currentPage === 0} />
      <Button label="Previous networks" onClick={() => setPage(currentPage - 1)} isDisabled={currentPage === 0} />
      <Text>Page {currentPage + 1} of {pageCount}</Text>
      <Button label="Next networks" onClick={() => setPage(currentPage + 1)} isDisabled={currentPage + 1 >= pageCount} />
    </HStack>
    {creating && canWrite && <NetworkForm accounts={accounts.data?.accounts ?? []} onAccepted={accepted} onCancel={() => setCreating(false)} />}
    {importing && canWrite && <ImportNetworkForm accounts={accounts.data?.accounts ?? []} onAccepted={accepted} onCancel={() => setImporting(false)} />}
  </VStack></Section>;
}
