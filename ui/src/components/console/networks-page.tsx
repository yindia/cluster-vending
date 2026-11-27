"use client";

import { Pagination } from "@astryxdesign/core/Pagination";
import { SearchFilters } from "./filter-row";
import dynamic from "next/dynamic";

import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { CheckboxInput } from "@astryxdesign/core/CheckboxInput";
import { Collapsible } from "@astryxdesign/core/Collapsible";
import { Divider } from "@astryxdesign/core/Divider";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack } from "@astryxdesign/core/HStack";
import { Link } from "@astryxdesign/core/Link";
import { MetadataList, MetadataListItem } from "@astryxdesign/core/MetadataList";
import { Selector } from "@astryxdesign/core/Selector";
import { ResourceActions } from "./resource-actions";
import { DetailNavigation, useDetailSection } from "./detail-navigation";
import { pixel, proportional, type TableColumn } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import { TextArea } from "@astryxdesign/core/TextArea";
import { TextInput } from "@astryxdesign/core/TextInput";
import { VStack } from "@astryxdesign/core/VStack";
import { timestampDate, type Timestamp } from "@bufbuild/protobuf/wkt";
import { usePlatform } from "@/components/platform-provider";
import { WizardDialog } from "./wizard-dialog";
import { ManifestCreateDialog } from "./manifest-create-dialog";
import { ManifestUpdateDialog } from "./manifest-update-dialog";
import { Icon } from "@astryxdesign/core/Icon";
import { ConsoleTable as Table } from "./console-table";
import { DetailHeader, DetailSummary, StatusValue } from "./detail-layout";
import { CopyButton, CopyValue } from "./copy-value";
import { ResourceYamlButton } from "./resource-yaml-button";
import { RefreshButton } from "./refresh-button";
import { PauseValue, pauseState } from "./pause-value";
import { ProviderIcon } from "./provider-icons";
import { ActivityEventTable, ActivityEventFilters, filterActivityEvents, type ActivityFilters } from "./activity-event-table";
import { errorMessage, useAsyncQuery } from "@/lib/api/use-query";
import { collectPages } from "@/lib/api/pagination";
import { ConditionStatus, type Condition } from "@/gen/cloud/v1/conditions_pb";
import { CodeChangePhase } from "@/gen/cloud/v1/code_change_pb";
import { ClusterState, ReleaseChannel } from "@/gen/cloud/v1/cluster_pb";
import { Disposition, ImportPhase, type ImportedResource } from "@/gen/cloud/v1/import_pb";
import { ManagedResourceState } from "@/gen/cloud/v1/managed_resource_pb";
import { FlowLogTrafficType, NatGatewayMode, NetworkState, SubnetRole, type Network } from "@/gen/cloud/v1/network_pb";
import { Tier } from "@/gen/cloud/v1/tier_pb";
import type { CloudAccount } from "@/gen/cloud/v1/tenant_service_pb";
import { buildTakeOverNetwork, enumLabel, networkIsReady, networkJson, networkProvider, networkTakeoverBlockReason, networkTakeoverKey, collectNetworkClusters, filterNetworkInventory, networkClusterRows, type NetworkTakeoverConfirmation } from "@/lib/workflows/networks";

const NetworkTopology = dynamic(() => import("./network-topology"), { ssr: false, loading: () => <Text>Loading network topology…</Text> });

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

function LabelSummary({ value }: { value: Record<string, string> }) {
  return Object.keys(value).length ? <MetadataList columns="multi">{Object.entries(value).map(([key, entry]) => <MetadataListItem key={key} label={key}>{entry || "Empty value"}</MetadataListItem>)}</MetadataList> : <Text type="supporting">No custom labels.</Text>;
}

const networkMark = <Icon icon="viewColumns" size="lg" />;
function TakeOverNetworkForm({ network, accounts, onAccepted, onReload, onClose }: {
  network: Network; accounts: CloudAccount[];
  onAccepted: (network: Network | undefined, message: string) => void;
  onReload: () => void; onClose: () => void;
}) {
  const { api, tenantId } = usePlatform();
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
  const locked = mutation.busy || !!blocked;
  const change = (patch: Partial<NetworkTakeoverConfirmation>) => {
    if (locked) return;
    setConfirmation({ ...consent, ...patch }); setValidation(""); mutation.setError("");
  };
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (locked) return;
    setValidation("");
    try {
      buildTakeOverNetwork(tenantId, network, account, confirmation);
      void mutation.run(async (signal) => {
        const current = (await api.network.getNetwork({ tenantId, networkId: network.id }, { signal })).network;
        signal.throwIfAborted();
        if (!current || current.tenantId !== tenantId || current.id !== network.id || networkTakeoverKey(current) !== reviewKey) {
          setConfirmation({ reviewKey: "", ownershipConfirmation: "", acknowledgeNotImported: false });
          onReload();
          throw new Error("The network or import plan changed. Network details are being reloaded; review takeover and confirm ownership again.");
        }
        const request = buildTakeOverNetwork(tenantId, current, account, confirmation);
        const response = await api.network.takeOverNetwork(request, { signal });
        if (!signal.aborted) { onAccepted(response.network, "Takeover request accepted. The controller will acquire management and converge the reviewed import plan asynchronously. Cloud readiness has not yet been confirmed."); }
      });
    } catch (error) {
      setValidation(errorMessage(error));
      requestAnimationFrame(() => errorFocus.current?.focus());
    }
  };
  return <WizardDialog isOpen onOpenChange={open => { if (!open) onClose(); }} title="Take over imported network" steps={["Confirm ownership"]} step={0} busy={mutation.busy} footer={<HStack gap={2} wrap="wrap">
      <Button label="Cancel takeover" onClick={onClose} isDisabled={mutation.busy} />
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
        {validation && <VStack ref={errorFocus} tabIndex={-1} role="alert"><ErrorNotice error={validation} /></VStack>}
        <ErrorNotice error={mutation.error} />
      </VStack>
    </form>
  </VStack></WizardDialog>;
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

function NetworkEvents({ networkId, onRefreshNetwork }: { networkId: string; onRefreshNetwork: () => void }) {
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
    <HStack gap={3} wrap="wrap" hAlign="between" vAlign="center" width="100%">
      <VStack gap={1}><Heading level={3}>Activity</Heading><Text type="supporting">Controller events, newest first.</Text></VStack>
      <RefreshButton label="Refresh activity" onClick={() => { onRefreshNetwork(); refreshActivity(); }} isLoading={activityQuery.refreshing || allEvents.refreshing} />
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

function DeleteNetwork({ network, onAccepted, onClose }: { onClose: () => void; network: Network; onAccepted: (network: Network | undefined, message: string) => void }) {
  const { api, tenantId } = usePlatform();
  const [confirmation, setConfirmation] = useState({ etag: network.etag, text: "" });
  const mutation = useNetworkMutation();
  const dependencies = network.status?.clusterIds ?? [];
  const confirmed = confirmation.etag === network.etag && confirmation.text === network.name;
  const disabled = !network.etag || !!dependencies.length || network.status?.state === NetworkState.DELETING;
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (disabled || !confirmed || mutation.busy) return;
    void mutation.run(async (signal) => {
      const response = await api.network.deleteNetwork({ tenantId, networkId: network.id, etag: network.etag }, { signal });
      if (!signal.aborted) { onAccepted(response.network, "Deletion request accepted. Removal is asynchronous; refresh inventory to confirm completion."); }
    });
  };
  return <WizardDialog isOpen onOpenChange={open => { if (!open) onClose(); }} title="Delete network" steps={["Confirm deletion"]} step={0} busy={mutation.busy} footer={<HStack gap={2} wrap="wrap">
      <Button label="Cancel deletion" onClick={onClose} isDisabled={mutation.busy} />
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
  </VStack></WizardDialog>;
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
    <HStack gap={3} hAlign="between" vAlign="center" wrap="wrap" width="100%">
      <Heading level={3}>Related clusters</Heading>
      <RefreshButton label="Refresh related clusters" onClick={query.refresh} isLoading={query.refreshing} />
    </HStack>
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
    <SearchFilters label="Resource filters" values={[kindFilter, stateFilter, pausedFilter]}
      search={<TextInput type="search" label="Search resources" placeholder="Name, kind or cloud ID" hasClear value={search} onChange={setSearch} autoComplete="off" hasSpellCheck={false} width="min(100%, 24rem)" />}>
      <Selector label="Resource kind" value={kindFilter} onChange={setKindFilter} width="min(100%, 12rem)" options={[{ value: "all", label: "All kinds" }, ...kinds.map((kind) => ({ value: kind, label: kind || "Unspecified kind" }))]} />
      <Selector label="Resource state" value={stateFilter} onChange={setStateFilter} width="min(100%, 12rem)" options={[{ value: "all", label: "All states" }, ...states.map((state) => ({ value: String(state), label: enumLabel(ManagedResourceState, state) }))]} />
      <Selector label="Pause status" value={pausedFilter} onChange={setPausedFilter} width="min(100%, 12rem)" options={[{ value: "all", label: "Any pause status" }, { value: "paused", label: "Paused" }, { value: "active", label: "Not paused" }]} />
      {hasFilters && <Button label="Clear resource filters" variant="ghost" onClick={clear} />}
    </SearchFilters>
    {filtered.length ? <Table data={filtered.map((resource, id) => ({ id, resource }))} idKey="id" aria-label="Network managed resource inventory" columns={[
      { key: "name", header: "Name", width: proportional(2, { minWidth: 80 }), renderCell: ({ resource }) => <CopyValue value={resource.name} label="Managed resource name" /> },
      { key: "state", header: "Status", width: pixel(152), renderCell: ({ resource }) => <StatusValue tone={resource.state === ManagedResourceState.READY ? "success" : resource.state === ManagedResourceState.FAILED ? "error" : "warning"} label={enumLabel(ManagedResourceState, resource.state)} /> },
      { key: "kind", header: "Kind", width: proportional(1, { minWidth: 80 }), renderCell: ({ resource }) => <Text maxLines={1} hasTruncateTooltip>{resource.kind || "Not reported"}</Text> },
      { key: "external", header: "Cloud ID", width: proportional(3, { minWidth: 80 }), renderCell: ({ resource }) => resource.externalName ? <CopyValue value={resource.externalName} label="Cloud resource ID" /> : <Text maxLines={1}>Pending assignment</Text> },
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
    <VStack gap={3}><HStack hAlign="between" vAlign="center" gap={2} wrap="wrap"><Heading level={2}>Network configuration</Heading><ResourceYamlButton kind="Network" resourceId={network.id} /></HStack><Text type="supporting">Account, region and address layout are fixed after creation.</Text>
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
  const { api, tenantId } = usePlatform();
  const [action, setAction] = useState<"delete" | "takeover">();
  const [editing, setEditing] = useState(false);
  const [notice, setNotice] = useState("");
  const tabs = [{ value: "overview", label: "Overview" }, { value: "topology", label: "Topology" }, { value: "resources", label: "Resources" }, { value: "activity", label: "Activity" }, { value: "configuration", label: "Configuration" }];
  const navigation = useDetailSection(tabs);
  const tab = navigation.active;
  const load = useCallback((signal: AbortSignal) => api.network.getNetwork({ tenantId, networkId }, { signal }), [api, tenantId, networkId]);
  const query = useAsyncQuery(load, { pollMs: 5000 });
  const network = query.data?.network;
  const accepted = (_network: Network | undefined, message: string) => { setNotice(message); setEditing(false); setAction(undefined); query.refresh(); };
  const status = network?.status;
  const account = accounts.find(({ id }) => id === network?.spec?.bootstrapId);
  const resources = status?.managedResources ?? [];
  const ready = resources.filter((resource) => resource.state === ManagedResourceState.READY && resource.ready === ConditionStatus.TRUE && resource.observedGeneration >= resource.generation).length;
  const paused = resources.filter((resource) => resource.paused).length;
  const takeoverBlock = network && status?.import ? networkTakeoverBlockReason(tenantId, network, account) : undefined;
  const healthy = network ? networkIsReady(network) : false;
  const state = status?.state === NetworkState.READY && !healthy ? "Reconciling" : enumLabel(NetworkState, status?.state);

  return <VStack gap={6}>
    <DetailHeader title={network?.displayName || network?.name || "Network details"}
      metadata={<><HStack gap={2} vAlign="center"><ProviderIcon provider={networkProvider(account) || ""} size="sm" /><Text type="supporting">{networkProvider(account) === "aws" ? "Amazon Web Services" : "Cloud network"}</Text></HStack><Text type="supporting">{network?.spec?.region || "Region pending"}</Text>{network && <CopyValue value={network.name} label="Network resource name" isSecondary />}{network && <CopyValue value={network.id} label="Network ID" isSecondary />}</>}
      status={network && <><StatusValue tone={healthy ? "success" : status?.state === NetworkState.FAILED ? "error" : "warning"} label={state} />{status?.freeze && <PauseValue freeze={status.freeze} />}</>}
      actions={<>
            {network && <Button label="Edit network" onClick={() => setEditing(true)} isDisabled={editing || !network.etag || !networkProvider(account) || status?.state === NetworkState.IMPORTED || status?.state === NetworkState.DELETING} />}
            {network && <ResourceActions resource="Network" items={[
              { id: "freeze", label: status?.freeze && !status.freeze.letThrough ? "Unfreeze network" : "Freeze network", href: `/freeze?scope=network&id=${encodeURIComponent(network.id)}` },
              ...(status?.import && (status.state === NetworkState.IMPORTED || status.state === NetworkState.FAILED) ? [{
                id: "takeover", label: "Take over imported network", description: takeoverBlock || undefined, isDisabled: !!takeoverBlock, onClick: () => setAction("takeover"),
              }] : []),
              { id: "delete", label: "Delete network", variant: "destructive", isDisabled: !network.etag || !!status?.clusterIds.length || status?.state === NetworkState.DELETING,
                description: status?.clusterIds.length ? "Remove dependent clusters first." : undefined, onClick: () => setAction("delete") },
            ]} />}
            {!network && query.error && <RefreshButton label="Retry network details" onClick={query.refresh} />}
      </>} />
    {notice && <Banner status="info" title="Request accepted" description={notice} />}
    <ErrorNotice error={query.error} />
    {query.loading && <Text role="status">Loading network health and resources…</Text>}
    {network && <>
      <DetailNavigation resource="Network" pages={tabs} selection={navigation} actions={["overview", "resources", "configuration"].includes(tab) && <RefreshButton label="Refresh details" onClick={query.refresh} isLoading={query.refreshing} />} />
      {tabs.map((item) => item.value === tab ? <VStack key={item.value} id={navigation.panelId(tab)} role="tabpanel" aria-label={navigation.label} tabIndex={0} gap={6}>
        {tab === "overview" && <>
          <DetailSummary items={[
            { label: "Managed resources", value: resources.length, note: `${paused} paused` },
            { label: "Resources ready", value: resources.length ? `${ready} / ${resources.length}` : "Not reported", note: "Observed cloud readiness" },
            { label: "Connected clusters", value: status?.clusterIds.length ?? 0, note: "Share this network" },
            { label: "Rollout tier", value: enumLabel(Tier, network.spec?.tier), note: "Inherited by every cluster" },
          ]} />
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
        {tab === "topology" && <NetworkTopology network={network} account={account} onRefresh={query.refresh} />}
        {tab === "resources" && <NetworkResourceInventory network={network} />}
        {tab === "activity" && <VStack gap={6}><NetworkEvents networkId={networkId} onRefreshNetwork={query.refresh} /><Collapsible trigger="Persisted recent events" defaultIsOpen={false}><ActivityEventTable events={status?.recentEvents ?? []} title="Persisted recent network events" /></Collapsible></VStack>}
        {tab === "configuration" && <NetworkConfiguration network={network} account={account} />}
      </VStack> : null)}
      {network && action === "delete" && <DeleteNetwork network={network} onClose={() => setAction(undefined)} onAccepted={accepted} />}
      {network && action === "takeover" && <TakeOverNetworkForm network={network} accounts={accounts} onClose={() => setAction(undefined)} onAccepted={accepted} onReload={query.refresh} />}
      {editing && <ManifestUpdateDialog kind="Network" id={networkId} currentEtag={network.etag} mark={networkMark} onClose={() => setEditing(false)} onApplied={(message) => accepted(undefined, message)} />}
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
    {accounts.error && <HStack hAlign="end" vAlign="center" width="100%"><RefreshButton label="Refresh accounts" onClick={accounts.refresh} isLoading={accounts.refreshing} /></HStack>}
    <NetworkDetails networkId={resourceId} accounts={accounts.data?.accounts ?? []} />
  </VStack>;
}

function NetworksList() {
  const { api, tenantId } = usePlatform();
  const [page, setPage] = useState(0);
  const [search, setSearch] = useState("");
  const [accountFilter, setAccountFilter] = useState("all");
  const [tierFilter, setTierFilter] = useState("all");
  const [stateFilter, setStateFilter] = useState("all");
  const [pauseFilter, setPauseFilter] = useState("all");
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
      (stateFilter === "all" || String(network.status?.state ?? NetworkState.UNSPECIFIED) === stateFilter) &&
      (pauseFilter === "all" || pauseState(network.status?.freeze) === pauseFilter);
  });
  const pageCount = Math.max(1, Math.ceil(filtered.length / 25));
  const currentPage = Math.min(page, pageCount - 1);
  const rows: NetworkRow[] = filtered.slice(currentPage * 25, (currentPage + 1) * 25).map((network) => ({ id: network.id, network }));
  const hasFilters = !!search.trim() || accountFilter !== "all" || tierFilter !== "all" || stateFilter !== "all" || pauseFilter !== "all";
  const columns: TableColumn<NetworkRow>[] = [
    { key: "name", header: "Name", width: proportional(2, { minWidth: 160 }), renderCell: ({ network }) => <HStack gap={1} vAlign="center"><Link href={`/networks/${encodeURIComponent(network.id)}`} maxLines={1} tooltip={network.displayName ? `${network.displayName} (${network.name})` : network.name}>{network.displayName || network.name || network.id}</Link><CopyButton value={network.displayName || network.name || network.id} label="Network name" /></HStack> },
    { key: "state", header: "Status", width: proportional(1, { minWidth: 100 }), renderCell: ({ network }) => <StatusValue label={enumLabel(NetworkState, network.status?.state)} tone={network.status?.state === NetworkState.READY ? "success" : network.status?.state === NetworkState.FAILED ? "error" : "neutral"} /> },
    { key: "pause", header: "Pause", width: proportional(1, { minWidth: 120 }), renderCell: ({ network }) => <PauseValue freeze={network.status?.freeze} /> },
    { key: "id", header: "ID", width: proportional(3, { minWidth: 80 }), renderCell: ({ network }) => <CopyValue value={network.id} label="Network ID" /> },
    { key: "account", header: "Account", width: proportional(2, { minWidth: 80 }), renderCell: ({ network }) => {
      const id = network.spec?.bootstrapId;
      const name = id ? accountMap.get(id)?.accountName || id : "Not reported";
      return id ? <Link href={`/accounts/${encodeURIComponent(id)}`} maxLines={1} tooltip={`${name} · ${id}`}>{name}</Link> : <Text maxLines={1}>{name}</Text>;
    } },
    { key: "region", header: "Region", width: proportional(1, { minWidth: 80 }), renderCell: ({ network }) => <Text maxLines={1} hasTruncateTooltip>{network.spec?.region || "Not reported"}</Text> },
    { key: "tier", header: "Tier", width: proportional(1, { minWidth: 80 }), renderCell: ({ network }) => <Text maxLines={1}>{enumLabel(Tier, network.spec?.tier)}</Text> },
  ];
  const accepted = (network: Network | undefined, message: string) => {
    setNotice(message); setCreating(false); setImporting(false); setAcceptedId(network?.id); list.refresh();
  };
  return <VStack gap={5}>
    <DetailHeader title="Networks" description="Manage network placement and the rollout tier inherited by clusters." actions={<>
        {<Button label="Create network" variant="primary" onClick={() => { setCreating(true); setImporting(false); }} isDisabled={creating} />}
        {<Button label="Import network" onClick={() => { setImporting(true); setCreating(false); }} isDisabled={importing} />}
        <RefreshButton label="Refresh networks" onClick={() => { list.refresh(); accounts.refresh(); }} isLoading={list.refreshing || accounts.refreshing} />
    </>} />
    {notice && <Banner status="info" title="Request accepted" description={notice} />}
    {acceptedId && <Link href={`/networks/${encodeURIComponent(acceptedId)}`}>View network details and progress</Link>}
    <ErrorNotice error={list.error} />
    {list.error && list.data && <Text type="supporting">Showing the last fully loaded network inventory. Refresh failed; no partial pages were added.</Text>}
    <ErrorNotice error={accounts.error} />
    <SearchFilters label="Network filters" values={[accountFilter, tierFilter, stateFilter, pauseFilter]}
      search={<TextInput type="search" label="Search networks" placeholder="Name, ID, account or region" hasClear value={search} onChange={(value) => { setSearch(value); setPage(0); }} width="min(100%, 24rem)" autoComplete="off" hasSpellCheck={false} />}>
      <Selector label="Filter by account" value={accountFilter} onChange={(value) => { setAccountFilter(value); setPage(0); }} width="min(100%, 14rem)" options={[{ value: "all", label: "All accounts" }, ...accountIds.map((id) => ({ value: id, label: accountMap.get(id)?.accountName || id }))]} />
      <Selector label="Filter by tier" value={tierFilter} onChange={(value) => { setTierFilter(value); setPage(0); }} width="min(100%, 12rem)" options={[{ value: "all", label: "All tiers" }, ...tiers.map((tier) => ({ value: String(tier), label: enumLabel(Tier, tier) }))]} />
      <Selector label="Filter by status" value={stateFilter} onChange={(value) => { setStateFilter(value); setPage(0); }} width="min(100%, 12rem)" options={[{ value: "all", label: "All statuses" }, ...states.map((state) => ({ value: String(state), label: enumLabel(NetworkState, state) }))]} />
      <Selector label="Filter by pause" value={pauseFilter} onChange={(value) => { setPauseFilter(value); setPage(0); }} width="min(100%, 12rem)" options={[{ value: "all", label: "All pause states" }, { value: "paused", label: "Paused" }, { value: "not-paused", label: "Not paused" }, { value: "let-through", label: "Let through" }]} />
      {hasFilters && <Button label="Clear network filters" variant="ghost" onClick={() => { setSearch(""); setAccountFilter("all"); setTierFilter("all"); setStateFilter("all"); setPauseFilter("all"); setPage(0); }} />}
    </SearchFilters>
    {list.loading && <Text role="status">Loading all network pages…</Text>}
    {list.data && <Text type="supporting" role="status">{filtered.length} of {allNetworks.length} tenant networks</Text>}
    {list.data && (rows.length ? <Table data={rows} columns={columns} idKey="id" aria-label="Tenant networks" rowIndexStart={currentPage * 25 + 1} rowCount={filtered.length} /> : <Text>{hasFilters ? "No networks match your search and filters." : "No networks in this tenant."}</Text>)}
    {pageCount > 1 && <Pagination label="Networks pagination" page={currentPage + 1} onChange={next => setPage(next - 1)} totalItems={filtered.length} pageSize={25} variant="count" />}
    {creating && <ManifestCreateDialog kind="Network" mark={networkMark} onClose={() => setCreating(false)} onCreated={(created) => accepted(created, "Create request accepted. Cloud provisioning continues asynchronously; open the network to track readiness.")} />}
    {importing && <ManifestCreateDialog kind="NetworkImport" mark={networkMark} onClose={() => setImporting(false)} onCreated={(imported) => accepted(imported, "Import request accepted for observe-only discovery. No cloud takeover was requested. Review the controller's import plan when discovery finishes.")} />}
  </VStack>;
}
