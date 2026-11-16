"use client";

import { useCallback, useId, useRef, useState, type ReactNode } from "react";
import { toJsonString } from "@bufbuild/protobuf";
import { Banner } from "@astryxdesign/core/Banner";
import { Collapsible } from "@astryxdesign/core/Collapsible";
import { Divider } from "@astryxdesign/core/Divider";
import { Button } from "@astryxdesign/core/Button";
import { CheckboxInput } from "@astryxdesign/core/CheckboxInput";
import { Dialog, DialogHeader } from "@astryxdesign/core/Dialog";
import { FormLayout } from "@astryxdesign/core/FormLayout";
import { Grid } from "@astryxdesign/core/Grid";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack } from "@astryxdesign/core/HStack";
import { StackItem } from "@astryxdesign/core/Stack";
import { Link } from "@astryxdesign/core/Link";
import { MetadataList, MetadataListItem } from "@astryxdesign/core/MetadataList";
import { Pagination } from "@astryxdesign/core/Pagination";
import { Selector } from "@astryxdesign/core/Selector";
import { ConsoleTable as Table } from "./console-table";
import { proportional, type TableColumn } from "@astryxdesign/core/Table";
import { TabList, Tab } from "@astryxdesign/core/TabList";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { Tooltip } from "@astryxdesign/core/Tooltip";
import { VStack } from "@astryxdesign/core/VStack";
import { usePlatform } from "@/components/platform-provider";
import { PlanReview } from "@/components/resources/plan-review";
import { collectPages } from "@/lib/api/pagination";
import { errorMessage, useAsyncQuery } from "@/lib/api/use-query";
import {
  applyReviewedClusterPlan, bindClusterPlan, buildClusterUpdate, buildCreateCluster,
  clusterForm, clusterIsConverging, clusterPlanApplicable, clusterProvider, clusterWizardSteps,
  validateClusterFormStep, buildImportCluster, type ClusterImportForm,
  createRequestIdentity, emptyClusterForm,
  type ClusterForm, type ReviewedClusterPlan,
} from "@/lib/workflows/clusters";
import {
  ClusterState, ClusterUpgradePhase, UpgradeMode, type Cluster, type PlatformComponent,
} from "@/gen/cloud/v1/cluster_pb";
import { CreateClusterRequestSchema, ImportClusterRequestSchema, UpdateClusterRequestSchema, type CreateClusterRequest, type ImportClusterRequest } from "@/gen/cloud/v1/cluster_service_pb";
import { ComponentPhase } from "@/gen/cloud/v1/component_pb";
import { ManagedResourceState, type ManagedResource } from "@/gen/cloud/v1/managed_resource_pb";
import { ConditionStatus } from "@/gen/cloud/v1/conditions_pb";
import { Disposition, ImportPhase } from "@/gen/cloud/v1/import_pb";
import { RetryAction } from "@/gen/cloud/v1/cluster_retry_pb";
import type { Network } from "@/gen/cloud/v1/network_pb";
import { WizardDialog } from "./wizard-dialog";
import { ClusterCreateDialog, ManifestCreateDialog } from "./manifest-create-dialog";
import { ImportManifestDialog } from "./import-manifest-dialog";
import { ClusterAccessButton } from "./cluster-access";
import { ClusterTakeoverButton } from "./cluster-takeover";
import { DetailHeader, DetailSummary, StatusValue } from "./detail-layout";
import { CopyButton, CopyValue } from "./copy-value";
import { ResourceYamlButton } from "./resource-yaml-button";
import { RefreshButton } from "./refresh-button";
import { ProviderIcon, KubernetesIcon } from "./provider-icons";
import { ActivityEventTable, ActivityEventFilters, filterActivityEvents, type ActivityFilters } from "./activity-event-table";
import { ClusterDeprecationsPanel } from "./cluster-deprecations";
import { ClusterRecommendationsPanel } from "./cluster-recommendations";
import { ClusterTenantComponents } from "./cluster-tenant-components";

function label(values: Record<number, string>, value?: number) {
  if (value === undefined) return "Not reported";
  const name = values[value]?.toLowerCase().replaceAll("_", " ");
  return name ? name[0].toUpperCase() + name.slice(1) : `Unknown (${value})`;
}

function statusValue(values: Record<number, string>, value?: number) {
  const text = label(values, value);
  const key = text.toLowerCase();
  const tone = ["running", "ready", "true", "converged", "matches"].includes(key) ? "success"
    : ["failed", "false", "blocked", "blocking"].includes(key) ? "error"
      : ["provisioning", "updating", "creating", "waiting", "deleting", "pending", "installing", "degraded", "discovering", "converging", "will update", "will create"].includes(key) ? "warning"
        : "neutral";
  return <StatusValue label={text} tone={tone} />;
}

function ErrorNotice({ message }: { message: string }) {
  return message ? <Banner status="error" title="Request failed" description={message} /> : null;
}

// Reuse the table-page template's content frame inside the parent console shell.
export function ClustersPage({ resourceId }: { resourceId?: string } = {}) {
  return resourceId ? <ClusterDetails key={resourceId} clusterId={resourceId} /> : <ClusterList />;
}

function clusterPause(cluster: Cluster): { key: string; label: string; detail: string; tone: "warning" | "neutral" } {
  const freeze = cluster.status?.freeze;
  const resources = cluster.status?.managedResources.filter((resource) => resource.paused).length ?? 0;
  const policy = freeze ? `Scope: ${freeze.scope || "Not reported"}. Reason: ${freeze.reason || "Not reported"}.` : "No effective cluster freeze is reported.";
  if (freeze && !freeze.letThrough) return { key: "paused", label: "Paused", detail: `Paused. ${policy}`, tone: "warning" };
  if (freeze?.letThrough) return { key: "let-through", label: "Let through", detail: `Cluster is allowed through the policy. ${policy}${resources ? ` ${resources} managed resources still report paused.` : ""}`, tone: "neutral" };
  if (resources) return { key: "resources-paused", label: "Resources paused", detail: `${resources} managed resources report paused; this does not indicate a whole-cluster freeze.`, tone: "warning" };
  return { key: "not-paused", label: "Not paused", detail: policy, tone: "neutral" };
}

function ClusterList() {
  const { api, tenantId, canWrite } = usePlatform();
  const [page, setPage] = useState(1);
  const [search, setSearch] = useState("");
  const [stateFilter, setStateFilter] = useState("all");
  const [pauseFilter, setPauseFilter] = useState("all");
  const [networkFilter, setNetworkFilter] = useState("all");
  const [channelFilter, setChannelFilter] = useState("all");
  const [tierFilter, setTierFilter] = useState("all");
  const [creating, setCreating] = useState(false);
  const [importing, setImporting] = useState(false);
  const [acceptedId, setAcceptedId] = useState("");
  const [acknowledgement, setAcknowledgement] = useState("");
  const load = useCallback((signal: AbortSignal) => collectPages(async (pageToken) => {
    const result = await api.cluster.listClusters({ tenantId, pageSize: 200, pageToken }, { signal });
    return { items: result.clusters, nextPageToken: result.nextPageToken };
  }, signal), [api, tenantId]);
  const list = useAsyncQuery(load);
  const loadNetworks = useCallback((signal: AbortSignal) => collectPages(async (pageToken) => {
    const result = await api.network.listNetworks({ tenantId, pageSize: 200, pageToken }, { signal });
    return { items: result.networks, nextPageToken: result.nextPageToken };
  }, signal), [api, tenantId]);
  const networks = useAsyncQuery(loadNetworks);
  const networkById = new Map((networks.data ?? []).map((network) => [network.id, network]));
  const inventory = list.data ?? [];
  const channelName = (value: number) => ["Server default", "Alpha", "Beta", "Stable"][value] || `Unknown (${value})`;
  const inheritedTier = (cluster: Cluster) => cluster.status?.tier || networkById.get(cluster.spec?.network?.networkId ?? "")?.spec?.tier || 0;
  const query = search.trim().toLocaleLowerCase();
  const filtered = inventory.filter((cluster) => {
    const networkId = cluster.spec?.network?.networkId ?? "";
    const network = networkById.get(networkId);
    const pause = clusterPause(cluster);
    return (!query || [cluster.name, cluster.displayName, cluster.id, networkId, network?.name, network?.displayName, cluster.spec?.region, pause.label, cluster.status?.freeze?.reason, cluster.status?.freeze?.scope].some((value) => value?.toLocaleLowerCase().includes(query))) &&
      (stateFilter === "all" || String(cluster.status?.state ?? 0) === stateFilter) &&
      (pauseFilter === "all" || pause.key === pauseFilter) &&
      (networkFilter === "all" || networkId === networkFilter) &&
      (channelFilter === "all" || String(cluster.spec?.releaseChannel ?? 0) === channelFilter) &&
      (tierFilter === "all" || String(inheritedTier(cluster)) === tierFilter);
  });
  const currentPage = Math.min(page, Math.max(1, Math.ceil(filtered.length / 25)));
  const visible = filtered.slice((currentPage - 1) * 25, currentPage * 25);
  const networkIds = [...new Set([...networkById.keys(), ...inventory.map((cluster) => cluster.spec?.network?.networkId ?? "").filter(Boolean)])];
  const changeFilter = (update: (value: string) => void) => (value: string) => { update(value); setPage(1); };
  type Row = { id: string; cluster: Cluster };
  const columns: TableColumn<Row>[] = [
    { key: "name", header: "Name", width: proportional(2, { minWidth: 80 }), renderCell: ({ cluster }) => <HStack gap={1} vAlign="center"><Link maxLines={1} href={`/clusters/${encodeURIComponent(cluster.id)}`}>{cluster.name || cluster.displayName}</Link><CopyButton value={cluster.name || cluster.displayName} label="cluster name" /></HStack> },
    { key: "id", header: "ID", width: proportional(2, { minWidth: 80 }), renderCell: ({ cluster }) => <CopyValue value={cluster.id} label="cluster ID" /> },
    { key: "state", header: "Status", width: proportional(1, { minWidth: 80 }), renderCell: ({ cluster }) => statusValue(ClusterState, cluster.status?.state) },
    { key: "pause", header: "Pause", width: proportional(1.5, { minWidth: 140 }), renderCell: ({ cluster }) => {
      const pause = clusterPause(cluster);
      return <Tooltip content={pause.detail}><StatusValue label={pause.label} tone={pause.tone} /></Tooltip>;
    } },
    { key: "version", header: "Kubernetes", width: proportional(1, { minWidth: 80 }), renderCell: ({ cluster }) => <Text maxLines={1}>{cluster.status?.currentKubernetesVersion || "Not observed"}</Text> },
    { key: "network", header: "Network", width: proportional(2, { minWidth: 80 }), renderCell: ({ cluster }) => {
      const id = cluster.spec?.network?.networkId;
      const network = id ? networkById.get(id) : undefined;
      return id ? <Link maxLines={1} href={`/networks/${encodeURIComponent(id)}`}>{network?.displayName || network?.name || id}</Link> : <Text>Not reported</Text>;
    } },
    { key: "region", header: "Region", width: proportional(1, { minWidth: 80 }), renderCell: ({ cluster }) => <Text maxLines={1}>{cluster.spec?.region || "Not reported"}</Text> },
    { key: "channel", header: "Channel", width: proportional(1, { minWidth: 80 }), renderCell: ({ cluster }) => <Text maxLines={1}>{channelName(cluster.spec?.releaseChannel ?? 0)}</Text> },
    { key: "tier", header: "Tier", width: proportional(1, { minWidth: 80 }), renderCell: ({ cluster }) => <Text maxLines={1}>{inheritedTier(cluster) ? `Tier ${inheritedTier(cluster) - 1}` : "Default"}</Text> },
  ];
  return <VStack gap={6}>
    <HStack gap={3} wrap="wrap" hAlign="end" vAlign="center" width="100%">
      <StackItem size="fill"><Heading level={2}>All clusters</Heading></StackItem>
      <HStack gap={3} wrap="wrap" vAlign="center">
        {canWrite && <><Button label="Create cluster" onClick={() => setCreating(true)} /><Button label="Import cluster" variant="secondary" onClick={() => setImporting(true)} /></>}
        <RefreshButton label="Refresh clusters" isLoading={list.refreshing || networks.refreshing} onClick={() => { list.refresh(); networks.refresh(); }} />
      </HStack>
    </HStack>
    {!canWrite && <Text type="supporting">Read-only access. Tenant administrators can create and change clusters.</Text>}
    {acknowledgement && <VStack role="status"><Text>{acknowledgement}</Text></VStack>}
    {acceptedId && <Link href={`/clusters/${encodeURIComponent(acceptedId)}`}>View cluster details and readiness</Link>}
    <HStack gap={3} wrap="wrap" vAlign="end">
      <TextInput label="Search clusters" value={search} onChange={changeFilter(setSearch)} type="search" placeholder="Name, ID, network or region" hasClear autoComplete="off" />
      <Selector label="Status" value={stateFilter} onChange={changeFilter(setStateFilter)} options={[{ value: "all", label: "All states" }, ...[...new Set(inventory.map((cluster) => cluster.status?.state ?? 0))].sort((a, b) => a - b).map((state) => ({ value: String(state), label: label(ClusterState, state) }))]} />
      <Selector label="Filter by pause" value={pauseFilter} onChange={changeFilter(setPauseFilter)} options={[{ value: "all", label: "All pause states" }, { value: "paused", label: "Paused" }, { value: "let-through", label: "Let through" }, { value: "resources-paused", label: "Resources paused" }, { value: "not-paused", label: "Not paused" }]} />
      <Selector label="Network" hasSearch value={networkFilter} onChange={changeFilter(setNetworkFilter)} options={[{ value: "all", label: "All networks" }, ...networkIds.map((id) => ({ value: id, label: networkById.get(id)?.displayName || networkById.get(id)?.name || id }))]} />
      <Selector label="Channel" value={channelFilter} onChange={changeFilter(setChannelFilter)} options={[{ value: "all", label: "All channels" }, ...[0, 1, 2, 3].map((channel) => ({ value: String(channel), label: channelName(channel) }))]} />
      <Selector label="Tier" value={tierFilter} onChange={changeFilter(setTierFilter)} options={[{ value: "all", label: "All tiers" }, { value: "0", label: "Default" }, ...[1, 2, 3].map((tier) => ({ value: String(tier), label: `Tier ${tier - 1}` }))]} />
      <Button label="Clear filters" variant="ghost" onClick={() => { setSearch(""); setStateFilter("all"); setPauseFilter("all"); setNetworkFilter("all"); setChannelFilter("all"); setTierFilter("all"); setPage(1); }} />
    </HStack>
    <ErrorNotice message={list.error} />
    <ErrorNotice message={networks.error} />
    {list.loading && <Text role="status">Loading all clusters…</Text>}
    {!!list.data && <VStack gap={4}>
      <Text type="supporting" role="status">{filtered.length} of {inventory.length} clusters</Text>
      {visible.length ? <Table data={visible.map((cluster) => ({ id: cluster.id, cluster }))} columns={columns} idKey="id" aria-label="Clusters" rowIndexStart={(currentPage - 1) * 25 + 1} rowCount={filtered.length} /> : <Text>{inventory.length ? "No clusters match these filters." : "No clusters found for this tenant."}</Text>}
      {filtered.length > 25 && <Pagination label="Clusters pagination" page={currentPage} onChange={setPage} totalItems={filtered.length} pageSize={25} variant="count" />}
    </VStack>}
    {creating && canWrite && <CreateCluster onClose={() => setCreating(false)} onCreated={(id) => { setCreating(false); setAcceptedId(id); setAcknowledgement("Create request recorded. Readiness is reported separately in cluster details."); list.refresh(); }} />}
    {importing && canWrite && <ImportCluster onClose={() => setImporting(false)} onImported={(id) => { setImporting(false); setAcceptedId(id); setAcknowledgement("Import request recorded. Discovery is asynchronous; cloud resources remain read-only until takeover."); list.refresh(); }} />}
  </VStack>;
}

function tierLabel(tier?: number) {
  return tier ? `Tier ${tier - 1} (from network)` : "Network default";
}

interface FieldsProps {
  value: ClusterForm;
  onChange: <K extends keyof ClusterForm>(field: K, value: ClusterForm[K]) => void;
  disabled: boolean;
  creating?: boolean;
  networkPicker?: ReactNode;
  region: string;
  tier?: number;
  step: number;
}

function ClusterFields({ value, onChange, disabled, creating, networkPicker, region, tier, step }: FieldsProps) {
  const text = (field: keyof ClusterForm, title: string, description?: string, required = false) => <TextInput
    label={title} value={String(value[field])} onChange={(next) => onChange(field, next)}
    description={description} isRequired={required} isDisabled={disabled}
    autoComplete="off" autoCapitalize="none" hasAutoCorrect={false} hasSpellCheck={false}
  />;
  return <VStack gap={5}>
    {step === 0 && <>
      <Text color="secondary">Choose where this cluster runs and how it receives releases.</Text>
      <FormLayout>
        <TextInput label="Cluster name" value={value.name} onChange={(next) => onChange("name", next)} isRequired isReadOnly={!creating} isDisabled={disabled} autoComplete="off" autoCapitalize="none" hasAutoCorrect={false} hasSpellCheck={false} description="A permanent identifier, such as production-api." />
        {networkPicker ?? <TextInput label="Network" value={value.networkId} isReadOnly />}
      </FormLayout>
      <MetadataList orientation="horizontal"><MetadataListItem label="Region">{region}</MetadataListItem><MetadataListItem label="Inherited tier">{tierLabel(tier)}</MetadataListItem></MetadataList>
      <Divider />
      <FormLayout direction="horizontal">
        <Selector label="Release channel" value={value.releaseChannel} isDisabled={disabled} onChange={(next) => onChange("releaseChannel", next)} options={[
          { value: "0", label: "Server default" }, { value: "1", label: "Alpha" }, { value: "2", label: "Beta" }, { value: "3", label: "Stable" },
        ]} />
        <Selector label="Upgrades" value={value.upgradeMode} isDisabled={disabled} onChange={(next) => onChange("upgradeMode", next)} options={[{ value: "1", label: "Manual" }, { value: "2", label: "Automatic" }]} />
      </FormLayout>
      <Text type="supporting">The server resolves Kubernetes versions. Automatic clusters follow their channel; an unset channel uses Stable in automatic mode.</Text>
      <CheckboxInput label="Namespace as a Service (NaaS)" htmlName="naas" value={value.naas} isDisabled={disabled} onChange={(next) => onChange("naas", next)} description="Enable support for team namespaces with separate access, quotas, and policies." />
      <Collapsible trigger="Advanced identity and version" defaultIsOpen={false}>
        <VStack gap={4} paddingBlock={3}>
          {text("displayName", "Display name", "Optional friendly name.")}
          {Number(value.upgradeMode) !== UpgradeMode.AUTOMATIC && text("kubernetesVersion", "Kubernetes minor version", "Optional. Leave empty for the server default.")}
        </VStack>
      </Collapsible>
    </>}
    {step === 1 && <>
      <Text color="secondary">Set the capacity range for the default node pool. The autoscaler manages the running node count.</Text>
      {text("instanceTypes", "Instance types", "Comma-separated AWS instance types.", true)}
      <FormLayout direction="horizontal">{text("minSize", "Minimum nodes", "Across the whole pool.", true)}{text("maxSize", "Maximum nodes", "Across the whole pool.", true)}</FormLayout>
      <Selector label="Capacity" value={value.capacityType} isDisabled={disabled} onChange={(next) => onChange("capacityType", next)} options={[{ value: "0", label: "On demand · server default" }, { value: "1", label: "On demand" }, { value: "2", label: "Spot" }]} />
      <Collapsible trigger="Advanced storage" defaultIsOpen={false}><VStack paddingBlock={3}>{text("diskSize", "Root disk (GiB)", "Leave empty for the provider default, or enter 20–16384 GiB.")}</VStack></Collapsible>
      {!creating && <Text type="supporting">Instance, capacity and disk changes may replace a node group. The server plan shows the exact impact.</Text>}
    </>}
    {step === 2 && <>
      <Text color="secondary">Control who can reach the Kubernetes API. At least one endpoint must remain enabled.</Text>
      <FormLayout>
        <CheckboxInput label="Private endpoint" description="Reachable from the cluster network." value={value.privateAccess} isDisabled={disabled} onChange={(next) => onChange("privateAccess", next)} />
        <CheckboxInput label="Public endpoint" description="Allow access from specific IPv4 networks." value={value.publicAccess} isDisabled={disabled} onChange={(next) => onChange("publicAccess", next)} />
        {value.publicAccess && text("publicCIDRs", "Allowed IPv4 CIDRs", "Comma-separated ranges. 0.0.0.0/0 allows any source.", true)}
      </FormLayout>
      {!value.publicAccess && <Text type="supporting">The management plane must be able to reach the private endpoint to install charts.</Text>}
      <Collapsible trigger="Advanced permissions and identity" defaultIsOpen={false}>
        <VStack gap={4} paddingBlock={3}>
          {text("adminPrincipals", "Cluster administrators", "Optional comma-separated IAM role or user ARNs.")}
          <Selector label="AWS service account roles (IRSA)" value={value.irsa} isDisabled={disabled} onChange={(next) => onChange("irsa", next)} options={[{ value: "default", label: "Enabled · server default" }, { value: "true", label: "Enabled" }, { value: "false", label: "Disabled" }]} />
        </VStack>
      </Collapsible>
    </>}
  </VStack>;
}

function ClusterReview({ form, region, networkName, accountName, tier }: { form: ClusterForm; region: string; networkName: string; accountName?: string; tier?: number }) {
  const automatic = Number(form.upgradeMode) === UpgradeMode.AUTOMATIC;
  return <VStack gap={5}>
    <Text color="secondary">Check the configuration before recording this request.</Text>
    <MetadataList title={<Heading level={4}>Placement</Heading>}>
      <MetadataListItem label="Cluster">{form.displayName || form.name}</MetadataListItem>
      <MetadataListItem label="Provider / account">AWS · {accountName || "Inherited from network"}</MetadataListItem>
      <MetadataListItem label="Network">{networkName}</MetadataListItem>
      <MetadataListItem label="Region / tier">{region} · {tierLabel(tier)}</MetadataListItem>
    </MetadataList>
    <Divider />
    <MetadataList title={<Heading level={4}>Runtime and capacity</Heading>}>
      <MetadataListItem label="Release channel">{[automatic ? "Stable · server default" : "None · server default", "Alpha", "Beta", "Stable"][Number(form.releaseChannel)]}</MetadataListItem>
      <MetadataListItem label="Upgrade policy">{automatic ? "Automatic · no version pin" : `Manual · ${form.kubernetesVersion || "server default version"}`}</MetadataListItem>
      <MetadataListItem label="Node pool">{form.instanceTypes} · {form.minSize}–{form.maxSize} nodes · {form.capacityType === "2" ? "Spot" : "On demand"}</MetadataListItem>
      <MetadataListItem label="Root disk">{form.diskSize ? `${form.diskSize} GiB` : "Provider default"}</MetadataListItem>
      <MetadataListItem label="Namespace as a Service (NaaS)">{form.naas ? "Enabled" : "Disabled"}</MetadataListItem>
    </MetadataList>
    <Divider />
    <MetadataList title={<Heading level={4}>Access</Heading>}>
      <MetadataListItem label="API endpoints">{[form.privateAccess && "Private", form.publicAccess && "Public"].filter(Boolean).join(" + ")}</MetadataListItem>
      {form.publicAccess && <MetadataListItem label="Allowed sources">{form.publicCIDRs}</MetadataListItem>}
      <MetadataListItem label="Administrators">{form.adminPrincipals || "No additional principals"}</MetadataListItem>
      <MetadataListItem label="Service account roles">{form.irsa === "false" ? "Disabled" : "Enabled"}</MetadataListItem>
    </MetadataList>
  </VStack>;
}

function useClusterNetworks() {
  const { api, tenantId } = usePlatform();
  const load = useCallback(async (signal: AbortSignal) => {
    const accounts = await api.tenant.listCloudAccounts({ tenantId }, { signal });
    const networks: Network[] = [];
    const seen = new Set<string>();
    let pageToken = "";
    do {
      const page = await api.network.listNetworks({ tenantId, pageSize: 200, pageToken }, { signal });
      networks.push(...page.networks);
      pageToken = page.nextPageToken;
      if (pageToken && seen.has(pageToken)) throw new Error("Network pagination repeated a token. Refresh to try again.");
      seen.add(pageToken);
    } while (pageToken);
    return { networks, accounts: accounts.accounts };
  }, [api, tenantId]);
  return useAsyncQuery(load);
}

function NetworkPicker({ choices, value, disabled, onChange }: { choices: ReturnType<typeof useClusterNetworks>; value: string; disabled: boolean; onChange: (value: string) => void }) {
  return <VStack gap={3}>
    <ErrorNotice message={choices.error} />
    {choices.error && <Button label="Reload networks and accounts" onClick={choices.refresh} isDisabled={disabled} />}
    <HStack gap={2} vAlign="end" wrap="wrap" hAlign="between" width="100%"><Selector label="Existing network" hasSearch value={value} isLoading={choices.loading} isDisabled={disabled || choices.loading} placeholder="Choose a network" onChange={onChange} options={(choices.data?.networks ?? []).map((item) => {
      let supported = true;
      try { clusterProvider(item, choices.data?.accounts ?? []); } catch { supported = false; }
      return { value: item.id, label: `${item.displayName || item.name} · ${item.spec?.region ?? "Unknown region"}`, disabled: !supported, description: supported ? "AWS · tier inherited from network" : "Unsupported or missing cloud account" };
    })} /><RefreshButton label="Refresh networks and accounts" onClick={choices.refresh} isDisabled={disabled} isLoading={choices.refreshing} /></HStack>
  </VStack>;
}

function CreateCluster({ onClose, onCreated }: { onClose: () => void; onCreated: (id: string) => void }) {
  const { api, tenantId, canWrite } = usePlatform();
  const [form, setForm] = useState<ClusterForm>({ ...emptyClusterForm });
  const [step, setStep] = useState(0);
  const [mode, setMode] = useState<"guided" | "yaml">("guided");
  const [manifestVisited, setManifestVisited] = useState(false);
  const [manifestBusy, setManifestBusy] = useState(false);
  const [review, setReview] = useState<CreateClusterRequest>();
  const [busy, setBusy] = useState(false);
  const locked = useRef(false);
  const [error, setError] = useState("");
  const errorRef = useRef<HTMLElement>(null);
  const formId = useId();
  const identity = useRef(createRequestIdentity(() => crypto.randomUUID()));
  const choices = useClusterNetworks();
  const network = choices.data?.networks.find((item) => item.id === form.networkId);
  function change<K extends keyof ClusterForm>(field: K, value: ClusterForm[K]) {
    identity.current.invalidate(); setReview(undefined); setError("");
    setForm((old) => ({ ...old, [field]: value }));
  }
  async function submit() {
    if (!canWrite || locked.current) return;
    try {
      if (step < 3) {
        validateClusterFormStep(form, step);
        if (step === 0) {
          if (!network || !choices.data) throw new Error("Choose an existing network.");
          clusterProvider(network, choices.data.accounts);
        }
        if (step === 2) {
          if (!network || !choices.data) throw new Error("Choose an existing network.");
          setReview(buildCreateCluster(tenantId, form, network, choices.data.accounts, identity.current.get()));
        }
        setError(""); setStep(step + 1); return;
      }
      if (!review) throw new Error("Review the create request before submitting.");
      locked.current = true; setBusy(true); setError("");
      const result = await api.cluster.createCluster(review);
      if (!result.cluster?.id) throw new Error("The server returned no cluster ID. Retry unchanged inputs with the same request ID.");
      onCreated(result.cluster.id);
    } catch (cause) { setError(errorMessage(cause)); requestAnimationFrame(() => errorRef.current?.focus()); }
    finally { locked.current = false; setBusy(false); }
  }
  return <>
  <ClusterCreateDialog mode={mode} onModeChange={(next) => { if (next === "yaml") setManifestVisited(true); setMode(next); }} isOpen onOpenChange={(open) => { if (!open && !locked.current) onClose(); }} title="Create cluster" steps={clusterWizardSteps} step={step} busy={busy || manifestBusy}
    yamlContent={manifestVisited ? <ManifestCreateDialog kind="Cluster" embedded isOpen={mode === "yaml"} onBusyChange={setManifestBusy} onOpenChange={(open: boolean) => { if (!open) onClose(); }} onCreated={(resource: Cluster) => onCreated(resource.id)} /> : undefined}
    description="Choose a network and configuration, then review your cluster before creating it."
    footer={<HStack gap={3} wrap="wrap">
      <Button label="Cancel creation" variant="secondary" onClick={onClose} isDisabled={busy} />
      {step > 0 && <Button label="Back" variant="secondary" onClick={() => { setStep(step - 1); setError(""); }} isDisabled={busy} />}
      <Button label={step === 3 ? "Create cluster" : "Next"} type="submit" form={formId} isLoading={busy} isDisabled={!canWrite} />
    </HStack>}>
    <form id={formId} method="post" onSubmit={(event) => { event.preventDefault(); void submit(); }}>
      <VStack gap={5}>
        {error && <VStack ref={errorRef} tabIndex={-1} role="alert"><ErrorNotice message={error} /></VStack>}
        {step < 3 ? <ClusterFields step={step} value={form} onChange={change} disabled={busy || !canWrite} creating region={network?.spec?.region ?? "Select a network"} tier={network?.spec?.tier}
          networkPicker={<NetworkPicker choices={choices} value={form.networkId} disabled={busy || !canWrite} onChange={(next) => change("networkId", next)} />} /> : <>
          <ClusterReview form={form} region={network?.spec?.region ?? ""} networkName={network?.displayName || network?.name || form.networkId} accountName={choices.data?.accounts.find((account) => account.id === network?.spec?.bootstrapId)?.accountName || network?.spec?.bootstrapId} tier={network?.spec?.tier} />
          <Banner status="info" title="Creates managed cloud resources" description="The controller provisions the control plane and node pool asynchronously. Readiness appears on the detail page; cloud charges may apply." />
          <Collapsible trigger="Advanced · exact API request" defaultIsOpen={false}><VStack as="pre" isScrollable><Text type="code">{review && toJsonString(CreateClusterRequestSchema, review, { prettySpaces: 2 })}</Text></VStack></Collapsible>
        </>}
      </VStack>
    </form>
  </ClusterCreateDialog>
  </>;
}

function ImportCluster({ onClose, onImported }: { onClose: () => void; onImported: (id: string) => void }) {
  const { api, tenantId, canWrite } = usePlatform();
  const [form, setForm] = useState<ClusterImportForm>({ name: "", displayName: "", networkId: "", defaultNodeGroupName: "" });
  const [step, setStep] = useState(0);
  const [mode, setMode] = useState<"guided" | "yaml">("guided");
  const [manifestVisited, setManifestVisited] = useState(false);
  const [review, setReview] = useState<ImportClusterRequest>();
  const [busy, setBusy] = useState(false);
  const locked = useRef(false);
  const identity = useRef(createRequestIdentity(() => crypto.randomUUID()));
  const [error, setError] = useState("");
  const errorRef = useRef<HTMLElement>(null);
  const formId = useId();
  const choices = useClusterNetworks();
  const network = choices.data?.networks.find((item) => item.id === form.networkId);
  function change(field: keyof ClusterImportForm, value: string) {
    identity.current.invalidate(); setReview(undefined); setError(""); setForm((old) => ({ ...old, [field]: value }));
  }
  async function submit() {
    if (!canWrite || locked.current) return;
    try {
      if (!network || !choices.data) throw new Error("Choose an existing network.");
      if (step < 2) {
        if (step === 0) {
          clusterProvider(network, choices.data.accounts);
          validateClusterFormStep({ ...emptyClusterForm, ...form }, 0);
        } else setReview(buildImportCluster(tenantId, form, network, choices.data.accounts, identity.current.get()));
        setError(""); setStep(step + 1); return;
      }
      if (!review) throw new Error("Review the import request first.");
      locked.current = true; setBusy(true); setError("");
      const result = await api.cluster.importCluster(review);
      if (!result.cluster?.id) throw new Error("No imported cluster ID was returned. Retry unchanged inputs with the same request ID.");
      onImported(result.cluster.id);
    } catch (cause) { setError(errorMessage(cause)); requestAnimationFrame(() => errorRef.current?.focus()); }
    finally { locked.current = false; setBusy(false); }
  }
  return <><WizardDialog isOpen={mode === "guided"} onOpenChange={(open) => { if (!open && !locked.current) onClose(); }} title="Import cluster" steps={["Identity and network", "Existing node group", "Review"]} step={step} busy={busy}
    description="Discover an existing AWS cluster in read-only mode."
    footer={<HStack gap={3} wrap="wrap"><Button label="Cancel import" variant="secondary" onClick={onClose} isDisabled={busy} />{step > 0 && <Button label="Back" variant="secondary" onClick={() => { setStep(step - 1); setError(""); }} isDisabled={busy} />}<Button label={step === 2 ? "Record read-only import" : "Next"} type="submit" form={formId} isLoading={busy} isDisabled={!canWrite} /></HStack>}>
    <form id={formId} method="post" onSubmit={(event) => { event.preventDefault(); void submit(); }}>
      <VStack gap={5}>
        {step === 0 && <HStack gap={3} wrap="wrap" hAlign="between" vAlign="center"><Text type="label">Use guided form</Text><Button type="button" label="Paste YAML" variant="secondary" isDisabled={busy || !canWrite} onClick={() => { setManifestVisited(true); setMode("yaml"); }} /></HStack>}
        <VStack ref={errorRef} tabIndex={-1} role="alert"><ErrorNotice message={error} /></VStack>
        {step === 0 && <FormLayout>
          <TextInput label="Existing cluster name" value={form.name} onChange={(next) => change("name", next)} isRequired isDisabled={busy} autoComplete="off" autoCapitalize="none" hasAutoCorrect={false} hasSpellCheck={false} />
          <TextInput label="Display name" value={form.displayName} onChange={(next) => change("displayName", next)} isDisabled={busy} />
          <NetworkPicker choices={choices} value={form.networkId} disabled={busy} onChange={(next) => change("networkId", next)} />
          <TextInput label="Region from network" value={network?.spec?.region ?? ""} isReadOnly />
        </FormLayout>}
        {step === 1 && <TextInput label="Existing default node group name" value={form.defaultNodeGroupName} onChange={(next) => change("defaultNodeGroupName", next)} isRequired isDisabled={busy} description="This node group becomes the default pool. Other node groups remain unmanaged and can block deletion." autoComplete="off" autoCapitalize="none" hasAutoCorrect={false} hasSpellCheck={false} />}
        {step === 2 && <>
          <Text>Import {form.name} from {network?.spec?.region}, using network {network?.name} and node group {form.defaultNodeGroupName}.</Text>
          <Text>The controller discovers cloud resources asynchronously. This request observes them read-only; it does not take ownership or alter the cloud.</Text>
          <MetadataList><MetadataListItem label="Provider / account">AWS · {choices.data?.accounts.find((account) => account.id === network?.spec?.bootstrapId)?.accountName || network?.spec?.bootstrapId}</MetadataListItem><MetadataListItem label="Network / region">{network?.name} · {network?.spec?.region}</MetadataListItem><MetadataListItem label="Inherited tier">{tierLabel(network?.spec?.tier)}</MetadataListItem><MetadataListItem label="Default node group">{form.defaultNodeGroupName}</MetadataListItem><MetadataListItem label="Ownership">Read-only discovery. No takeover or cloud writes.</MetadataListItem></MetadataList>
          <Collapsible trigger="Advanced · exact import request" defaultIsOpen={false}><VStack as="pre" isScrollable><Text type="code">{review && toJsonString(ImportClusterRequestSchema, review, { prettySpaces: 2 })}</Text></VStack></Collapsible>
        </>}
      </VStack>
    </form>
  </WizardDialog>
  {manifestVisited && <ImportManifestDialog kind="ClusterImport" isOpen={mode === "yaml"} onOpenChange={open => { if (!open) onClose(); }} onUseForm={() => setMode("guided")} onImported={resource => onImported(resource.id)} />}
  </>;
}

function ClusterDetails({ clusterId }: { clusterId: string }) {
  const { api, tenantId, canWrite } = usePlatform();
  const [tab, setTab] = useState("overview");
  const panelPrefix = useId();
  const [polling, setPolling] = useState(true);
  const [editing, setEditing] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [retrying, setRetrying] = useState(false);
  const [message, setMessage] = useState("");
  const [eventTokens, setEventTokens] = useState([""]);
  const [activityFilters, setActivityFilters] = useState<ActivityFilters>({ search: "", kind: "all", onlyWarnings: false });
  const filteringActivity = !!activityFilters.search.trim() || activityFilters.kind !== "all" || activityFilters.onlyWarnings;
  const [requestedEventKinds, setRequestedEventKinds] = useState(false);
  const loadCompleteEvents = filteringActivity || requestedEventKinds;
  const load = useCallback(async (signal: AbortSignal) => {
    const result = await api.cluster.getCluster({ tenantId, clusterId }, { signal });
    if (!signal.aborted) setPolling(!!result.cluster && clusterIsConverging(result.cluster));
    return result;
  }, [api, tenantId, clusterId]);
  const detail = useAsyncQuery(load, { pollMs: polling ? 5000 : undefined });
  const eventToken = eventTokens[eventTokens.length - 1];
  const loadEvents = useCallback((signal: AbortSignal) => api.cluster.listClusterEvents({ tenantId, clusterId, pageSize: 25, pageToken: eventToken }, { signal }), [api, tenantId, clusterId, eventToken]);
  const events = useAsyncQuery(loadEvents, { pollMs: !filteringActivity && polling ? 5000 : undefined });
  const loadAllEvents = useCallback(async (signal: AbortSignal) => {
    if (!loadCompleteEvents) return undefined;
    return collectPages(async pageToken => {
      const page = await api.cluster.listClusterEvents({ tenantId, clusterId, pageSize: 200, pageToken }, { signal });
      return { items: page.events, nextPageToken: page.nextPageToken };
    }, signal);
  }, [api, tenantId, clusterId, loadCompleteEvents]);
  const allEvents = useAsyncQuery(loadAllEvents);
  const activityQuery = filteringActivity ? allEvents : events;
  const refreshActivity = () => {
    if (!filteringActivity) events.refresh();
    if (loadCompleteEvents) allEvents.refresh();
  };
  const refresh = () => { detail.refresh(); refreshActivity(); };
  const cluster = detail.data?.cluster;
  const status = cluster?.status;
  const aws = status?.provider.case === "aws" ? status.provider.value : undefined;
  const resources = status?.managedResources ?? [];
  const ready = resources.filter((resource) => resource.ready === ConditionStatus.TRUE).length;
  const nodeGroups = resources.filter((resource) => /nodegroup|nodepool/i.test(resource.kind));
  const networkId = cluster?.spec?.network?.networkId;
  const loadNetwork = useCallback((signal: AbortSignal) => networkId ? api.network.getNetwork({ tenantId, networkId }, { signal }) : Promise.resolve(undefined), [api, tenantId, networkId]);
  const network = useAsyncQuery(loadNetwork);
  const pool = cluster?.spec?.defaultNodePool;
  const channel = ["Server default", "Alpha", "Beta", "Stable"][cluster?.spec?.releaseChannel ?? 0] || "Unknown";
  const channelSlug = [undefined, "alpha", "beta", "stable"][cluster?.spec?.releaseChannel ?? 0];
  const desiredAws = cluster?.spec?.provider.case === "aws" ? cluster.spec.provider.value : undefined;
  const addonNames = [...new Set([...(desiredAws?.addons.map((addon) => addon.name) ?? []), ...(aws?.addons.map((addon) => addon.name) ?? [])])];
  const pageEvents = events.data?.events.length ? events.data.events : eventToken ? [] : status?.recentEvents ?? [];
  const activityEvents = filteringActivity ? allEvents.data ?? [] : pageEvents;
  const filteredEvents = filterActivityEvents(activityEvents, activityFilters);
  const availableEventKinds = [...pageEvents, ...(allEvents.data ?? [])];
  const panels = ["overview", "resources", "activity", "components", "deprecations", "recommendations"] as const;
  const panelId = (name: string) => `${panelPrefix}-${name}`;
  function openTab(name: string) { setTab(name); if (name === "activity") setRequestedEventKinds(true); }
  return <VStack gap={6} as="section" aria-label="Cluster details">
    {!cluster && <DetailHeader title={detail.loading ? "Loading cluster" : "Cluster details"} />}
    <ErrorNotice message={detail.error} />
    {detail.loading && <Text role="status">Loading cluster…</Text>}
    {cluster && <>
      <DetailHeader
        title={cluster.displayName || cluster.name}
        description={`${cluster.spec?.provider.case?.toUpperCase() || "Provider pending"} · ${cluster.spec?.region || "Region pending"}`}
        metadata={<HStack gap={4} wrap="wrap" vAlign="center"><ProviderIcon provider={cluster.spec?.provider.case ?? ""} size="sm" /><CopyValue value={cluster.name} label="cluster name" isSecondary /><CopyValue value={cluster.id} label="cluster ID" isSecondary /></HStack>}
        status={statusValue(ClusterState, status?.state)}
        actions={<HStack gap={2} wrap="wrap" vAlign="center">
          <ResourceYamlButton kind="Cluster" resourceId={cluster.id} />
          <ClusterAccessButton cluster={cluster} />
          {canWrite && status?.import && <ClusterTakeoverButton cluster={cluster} onAccepted={() => { setMessage("Takeover request accepted. Ownership and convergence continue asynchronously; readiness is not yet confirmed."); refresh(); }} />}
          {canWrite && <Button label={status?.freeze && !status.freeze.letThrough ? "Unfreeze cluster" : "Freeze cluster"} variant={status?.freeze && !status.freeze.letThrough ? "primary" : "secondary"} href={`/freeze?scope=cluster&id=${encodeURIComponent(cluster.id)}`} />}
          {canWrite && <><Button label="Edit cluster" variant="secondary" onClick={() => { setEditing(true); setMessage(""); }} isDisabled={status?.state === ClusterState.DELETING} /><Button label="Delete" variant="destructive" onClick={() => setDeleting(true)} /></>}
          <RefreshButton label="Refresh" onClick={refresh} isLoading={detail.refreshing} />
        </HStack>}
      />
      {message && <VStack role="status"><Banner status="info" title="Request recorded" description={message} /></VStack>}
      <DetailSummary items={[
        { label: "Kubernetes", value: <HStack gap={2} vAlign="center"><KubernetesIcon /><Text type="large" weight="semibold" hasCapsize>{status?.currentKubernetesVersion || "Resolving"}</Text></HStack>, note: status?.kubernetes?.available ? `${status.kubernetes.available} available` : "Observed control plane" },
        { label: "Resources ready", value: `${ready} / ${resources.length}`, note: resources.some((resource) => resource.paused) ? `${resources.filter((resource) => resource.paused).length} paused` : "Managed cloud inventory" },
        { label: "Node capacity", value: pool?.scalingConfig ? `${pool.scalingConfig.minSize}–${pool.scalingConfig.maxSize}` : "Pending", note: pool?.instanceTypes.join(", ") || "Default node pool" },
        { label: "Release channel", value: channelSlug ? <Link href={`/channels/${channelSlug}`}>{channel}</Link> : channel, note: cluster.spec?.upgradeMode === UpgradeMode.AUTOMATIC ? "Automatic upgrades" : "Manual upgrades" },
      ]} />
      <TabList value={tab} onChange={openTab} role="tablist" aria-label="Cluster detail sections" hasDivider>
        {panels.map((name) => <Tab key={name} value={name} label={name === "deprecations" ? "Deprecated APIs" : name[0].toUpperCase() + name.slice(1)} panelId={panelId(name)} />)}
      </TabList>
      {tab === "overview" && <VStack id={panelId("overview")} role="tabpanel" aria-label="Overview" tabIndex={0} gap={8}>
        {status?.message && <Text color="secondary">{status.message}</Text>}
        <Grid columns={{ minWidth: 300, max: 2 }} gap={8}>
          <VStack gap={4}>
            <Heading level={3}>Deployment</Heading>
            <MetadataList>
              <MetadataListItem label="Network">{networkId ? <CopyReference href={`/networks/${encodeURIComponent(networkId)}`} value={networkId} display={network.data?.network?.displayName || network.data?.network?.name} label="network ID" /> : "Not reported"}</MetadataListItem>
              <MetadataListItem label="Cloud account">{network.data?.network?.spec?.bootstrapId ? <CopyReference href={`/accounts/${encodeURIComponent(network.data.network.spec.bootstrapId)}`} value={network.data.network.spec.bootstrapId} label="cloud account ID" /> : "Inherited from network"}</MetadataListItem>
              <MetadataListItem label="Rollout tier">{tierLabel(status?.tier)}</MetadataListItem>
              <MetadataListItem label="Generation">{String(status?.observedGeneration ?? 0)} observed / {String(cluster.generation)} desired</MetadataListItem>
              <MetadataListItem label="Platform release">{status?.platformRelease ? <Link href={`/releases/${encodeURIComponent(status.platformRelease)}`}>{status.platformRelease}</Link> : "Not reported"}</MetadataListItem>
              <MetadataListItem label="Namespace as a Service (NaaS)"><VStack gap={1}><Text>{cluster.spec?.naas ? "Enabled" : "Disabled"}</Text><Text type="supporting">{cluster.spec?.naas ? "Check Components for setup progress." : "Existing namespaces and workloads are kept when disabling."}</Text></VStack></MetadataListItem>
            </MetadataList>
            <ErrorNotice message={network.error} />
          </VStack>
          <VStack gap={4}>
            <Heading level={3}>Health and rollout</Heading>
            {status?.conditions.some((condition) => condition.status === ConditionStatus.FALSE) && <Banner status="warning" title="Health checks need attention" description={status.conditions.filter((condition) => condition.status === ConditionStatus.FALSE).map((condition) => condition.reason || condition.type).join(" · ")} />}
            <MetadataList>
              <MetadataListItem label="Kubernetes">{status?.kubernetes?.message || (status?.kubernetes?.channelMinor ? `Channel offers ${status.kubernetes.channelMinor}` : "Version resolved by the server")}</MetadataListItem>
              <MetadataListItem label="Upgrade">{status?.upgrade ? `${status.upgrade.fromVersion} → ${status.upgrade.toVersion} · ${label(ClusterUpgradePhase, status.upgrade.phase)}` : "No upgrade in progress"}</MetadataListItem>
              <MetadataListItem label="Release move">{status?.releaseMove ? `${status.releaseMove.current || "Unassigned"} → ${status.releaseMove.target || "Current"} · ${status.releaseMove.message || "No message"}` : "No release move reported"}</MetadataListItem>
            </MetadataList>
            {status?.upgrade?.message && <Text type="supporting">{status.upgrade.message}</Text>}
            <HStack gap={2} wrap="wrap"><Button label="View deprecated APIs" onClick={() => openTab("deprecations")} /><Button label="View recommendations" onClick={() => openTab("recommendations")} /></HStack>
            {canWrite && <HStack><Button label="Retry failed work" variant="secondary" onClick={() => setRetrying(true)} /></HStack>}
          </VStack>
        </Grid>
        <Divider />
        <VStack gap={4}>
          <HStack hAlign="between" gap={3} wrap="wrap"><Heading level={3}>Related resources</Heading><Link href={`#${panelId("resources")}`} onClick={() => openTab("resources")}>View all {resources.length} resources</Link></HStack>
          <DetailRows title="Related cluster resources" rows={[
            ...nodeGroups.map((resource) => ({ name: resource.name, copyName: resource.name, state: statusValue(ManagedResourceState, resource.state), detail: <VStack gap={1}>{resource.externalName && <CopyValue value={resource.externalName} label="node group cloud ID" />}<Text type="supporting">{resource.message || "Node group"}</Text></VStack>, href: `#${panelId("resources")}`, activate: () => openTab("resources") })),
            ...(aws?.addons ?? []).map((addon) => ({ name: addon.name, state: statusValue(ManagedResourceState, addon.state), detail: `Add-on · ${addon.version}`, href: `#${panelId("components")}`, activate: () => openTab("components") })),
          ]} />
        </VStack>
        <VStack gap={4}>
          <Heading level={3}>All health conditions</Heading>
          <Table searchable aria-label="Cluster health conditions" data={(status?.conditions ?? []).map((condition, rowId) => ({ ...condition, rowId }))} idKey="rowId" columns={[
            { key: "type", header: "Condition", renderCell: condition => <Text maxLines={1} hasTruncateTooltip>{condition.type}</Text> },
            { key: "status", header: "Status", renderCell: condition => statusValue(ConditionStatus, condition.status) },
            { key: "reason", header: "Reason", renderCell: condition => <Text maxLines={1} hasTruncateTooltip>{condition.reason || "Not reported"}</Text> },
            { key: "message", header: "Message", renderCell: condition => <Text maxLines={1} hasTruncateTooltip>{condition.message || "No details reported"}</Text> },
            { key: "updated", header: "Updated (UTC)", renderCell: condition => <Text maxLines={1} hasTruncateTooltip>{condition.lastTransitionTime ? new Date(Number(condition.lastTransitionTime.seconds) * 1000).toISOString() : "Not reported"}</Text> },
          ]} />
          {!status?.conditions.length && <Text color="secondary">Health conditions will appear after the controller reports them.</Text>}
        </VStack>
        <VStack gap={4}>
          <Heading level={3}>Provider identifiers and endpoint</Heading>
          <Table searchable aria-label="Provider identifiers and endpoint" idKey="name" data={[
            { name: "API endpoint", value: status?.endpoint?.url || "" },
            ...(status?.provider.case === "aws" || cluster.spec?.provider.case === "aws" ? [
              { name: "Cluster ARN", value: aws?.clusterArn || "" },
              { name: "Node group ARN", value: aws?.nodeGroupArn || "" },
              { name: "Security group", value: aws?.clusterSecurityGroupId || "" },
              { name: "Provider version", value: aws?.platformVersion || "" },
            ] : []),
          ]} columns={[
            { key: "name", header: "Identifier", width: proportional(1, { minWidth: 120 }) },
            { key: "value", header: "Value", width: proportional(3, { minWidth: 200 }), renderCell: item => item.value ? <CopyValue value={item.value} label={item.name} /> : <Text color="secondary">Pending</Text> },
          ]} />
        </VStack>
      </VStack>}
      {tab === "resources" && <VStack id={panelId("resources")} role="tabpanel" aria-label="Resources" tabIndex={0} gap={6}>
        <ManagedInventory resources={resources} title="Cluster managed resource inventory" />
        {status?.import && <Collapsible trigger={`Import discovery · ${label(ImportPhase, status.import.phase)}`} defaultIsOpen={false}>
          <DetailRows title="Cluster import resource inventory" rows={status.import.resources.map((resource) => ({ name: `${resource.kind} / ${resource.name}`, copyName: resource.name, state: statusValue(Disposition, resource.disposition), detail: <VStack gap={1}>{resource.externalName && <CopyValue value={resource.externalName} label="imported resource cloud ID" />}<Text>{resource.apiVersion} · {resource.detail}{resource.requiresReplacement ? " · Requires replacement" : ""}</Text></VStack> }))} />
        </Collapsible>}
        {!!status?.carriedOver.length && <Collapsible trigger="Carried-over resources" defaultIsOpen={false}><DetailRows title="Carried-over cluster resources" rows={status.carriedOver.map((resource) => ({ name: `${resource.kind} / ${resource.name}`, copyName: resource.name, state: statusValue(Disposition, resource.disposition), detail: <VStack gap={1}>{resource.externalName && <CopyValue value={resource.externalName} label="carried-over resource cloud ID" />}<Text>{resource.detail}</Text></VStack> }))} /></Collapsible>}
      </VStack>}
      {tab === "activity" && <VStack id={panelId("activity")} role="tabpanel" aria-label="Activity" tabIndex={0} gap={5}>
        <HStack gap={3} wrap="wrap" hAlign="between" vAlign="center"><VStack gap={1}><Heading level={3}>Activity</Heading><Text type="supporting">Controller events, newest first.</Text></VStack><RefreshButton label="Refresh activity" onClick={refreshActivity} isLoading={activityQuery.refreshing || allEvents.refreshing} /></HStack>
        <ActivityEventFilters value={activityFilters} onChange={value => { setActivityFilters(value); setEventTokens([""]); }} events={availableEventKinds} onRequestKinds={() => setRequestedEventKinds(true)} />
        {loadCompleteEvents && allEvents.loading && !filteringActivity && <Text role="status">Loading all event kinds and pages…</Text>}
        {!filteringActivity && allEvents.error && <Banner status="warning" title="Complete event kinds unavailable" description={`${allEvents.error} Refresh activity to retry loading all available events.`} />}
        <Text type="supporting" role="status">{activityQuery.loading ? filteringActivity ? "Loading all available event pages…" : "Loading events…" : `${filteredEvents.length} of ${activityEvents.length} events ${filteringActivity ? "across all available pages" : "on this page"}`} · Times shown in UTC.{!filteringActivity && " Use pagination to check other event pages."}</Text>
        <ErrorNotice message={activityQuery.error} />
        {(!filteringActivity || allEvents.data) && <ActivityEventTable title="Cluster events" events={filteredEvents} emptyMessage={filteringActivity ? "No events match the current filters." : "No events were reported."} />}
        {!filteringActivity && <HStack gap={2} wrap="wrap" hAlign="end" vAlign="center">
          <Text type="supporting">Page {eventTokens.length}</Text>
          <Button label="Previous events" isDisabled={eventTokens.length < 2 || events.loading || events.refreshing} onClick={() => setEventTokens(eventTokens.slice(0, -1))} />
          <Button label="Next events" isDisabled={!events.data?.nextPageToken || eventTokens.includes(events.data.nextPageToken) || events.loading || events.refreshing} onClick={() => { if (events.data?.nextPageToken) setEventTokens([...eventTokens, events.data.nextPageToken]); }} />
        </HStack>}
      </VStack>}
      {tab === "components" && <VStack id={panelId("components")} role="tabpanel" aria-label="Components" tabIndex={0} gap={6}>
        <VStack gap={3}>
          <Heading level={3}>EKS add-ons</Heading>
          <Text type="supporting">Target is the resolved desired version; running is the last observed version. AWS resolves add-ons for the Kubernetes minor unless an explicit version was requested. Release channels do not pin add-on versions.</Text>
          {addonNames.length ? <Table aria-label="Cluster EKS add-on target and running versions" density="compact" hasHover textOverflow="wrap" idKey="name" data={addonNames.map((name) => ({
            name, target: desiredAws?.addons.find((addon) => addon.name === name)?.version || "Not reported",
            running: aws?.addons.find((addon) => addon.name === name),
          }))} columns={[
            { key: "name", header: "Add-on", width: proportional(2, { minWidth: 80 }) },
            { key: "target", header: "Target (resolved)", width: proportional(1, { minWidth: 80 }) },
            { key: "version", header: "Running", width: proportional(1, { minWidth: 80 }), renderCell: (row) => <Text>{row.running?.version || "Not observed"}</Text> },
            { key: "state", header: "State", width: proportional(1, { minWidth: 80 }), renderCell: (row) => statusValue(ManagedResourceState, row.running?.state) },
            { key: "message", header: "Message", width: proportional(2, { minWidth: 80 }), renderCell: (row) => <Text type="supporting">{row.running?.message || (row.running ? "No message reported" : "Waiting for observation")}</Text> },
          ]} /> : <Text type="supporting">No EKS add-on targets or running versions reported.</Text>}
        </VStack>
        <VStack gap={3}>
          <HStack gap={3} wrap="wrap" hAlign="between" vAlign="center"><Heading level={3}>Platform components</Heading>{status?.platformRelease && <Link href={`/releases/${encodeURIComponent(status.platformRelease)}`}>Release {status.platformRelease}</Link>}</HStack>
          <Text type="supporting">The platform release pins chart versions. Installed versions show the last observed ready version and can lag during rollout.</Text>
          <PlatformComponentInventory components={status?.platformComponents ?? []} />
        </VStack>
        <ClusterTenantComponents clusterId={clusterId} />
      </VStack>}
      {tab === "deprecations" && <VStack id={panelId("deprecations")} role="tabpanel" aria-label="Deprecated APIs" tabIndex={0} gap={6}><ClusterDeprecationsPanel clusterId={clusterId} /></VStack>}
      {tab === "recommendations" && <VStack id={panelId("recommendations")} role="tabpanel" aria-label="Recommendations" tabIndex={0} gap={6}><ClusterRecommendationsPanel clusterId={clusterId} /></VStack>}
      {editing && canWrite && <EditCluster clusterId={clusterId} currentEtag={cluster.etag} onClose={() => setEditing(false)} onApplied={() => { setEditing(false); setMessage("Update recorded. The controller is converging the desired state."); refresh(); }} />}
      {deleting && canWrite && <DeleteCluster cluster={cluster} onCancel={() => setDeleting(false)} onDeleted={() => { setDeleting(false); setMessage("Deletion requested. Resources are removed asynchronously."); refresh(); }} />}
      {retrying && canWrite && <RetryCluster clusterId={clusterId} onClose={() => setRetrying(false)} onRetried={refresh} />}
    </>}
  </VStack>;
}

function CopyReference({ value, href, display, label: copyLabel }: { value: string; href: string; display?: string; label: string }) {
  return <HStack gap={2} wrap="wrap" vAlign="center"><Link href={href}>{display || value}</Link><CopyButton value={value} label={copyLabel} /></HStack>;
}

function PlatformComponentInventory({ components }: { components: PlatformComponent[] }) {
  const columns: TableColumn<PlatformComponent>[] = [
    { key: "name", header: "Component", width: proportional(2, { minWidth: 80 }) },
    { key: "version", header: "Release-pinned version", width: proportional(1, { minWidth: 80 }), renderCell: (component) => <Text>{component.version || "No release pin reported"}</Text> },
    { key: "installedVersion", header: "Installed version", width: proportional(1, { minWidth: 80 }), renderCell: (component) => <Text>{component.installedVersion || "Not observed"}</Text> },
    { key: "phase", header: "State", width: proportional(1, { minWidth: 80 }), renderCell: (component) => statusValue(ComponentPhase, component.phase) },
    { key: "message", header: "Details", width: proportional(2, { minWidth: 80 }), renderCell: (component) => <VStack gap={1}><Text type="supporting">{component.message || component.helmState || "No message reported"}</Text>{component.lastError && <Text type="supporting">{component.lastError}</Text>}</VStack> },
  ];
  return components.length ? <Table aria-label="Cluster platform component pinned and installed versions" data={components} columns={columns} idKey="name" density="compact" hasHover textOverflow="wrap" /> : <Text type="supporting">No platform components reported.</Text>;
}

function ManagedInventory({ resources, title }: { resources: ManagedResource[]; title: string }) {
  const [search, setSearch] = useState("");
  const [health, setHealth] = useState("all");
  const [kind, setKind] = useState("all");
  const [state, setState] = useState("all");
  const [paused, setPaused] = useState("all");
  const query = search.trim().toLocaleLowerCase();
  const matching = resources.filter((resource) => {
    const matchesText = !query || [resource.name, resource.kind, resource.externalName].some((value) => value.toLocaleLowerCase().includes(query));
    const ready = resource.ready === ConditionStatus.TRUE && resource.synced === ConditionStatus.TRUE;
    return matchesText && (kind === "all" || resource.kind === kind) &&
      (state === "all" || String(resource.state) === state) &&
      (paused === "all" || resource.paused === (paused === "paused")) &&
      (health === "all" || (health === "ready" ? ready : !ready));
  });
  const kinds = [...new Set(resources.map((resource) => resource.kind))].sort();
  const states = [...new Set(resources.map((resource) => resource.state))].sort((a, b) => a - b);
  const columns: TableColumn<ManagedResource>[] = [
    { key: "name", header: "Name", width: proportional(2, { minWidth: 80 }), renderCell: (resource) => <CopyValue value={resource.name} label="managed resource name" /> },
    { key: "kind", header: "Kind", width: proportional(1, { minWidth: 80 }) },
    { key: "externalName", header: "Cloud ID", width: proportional(2, { minWidth: 80 }), renderCell: (resource) => resource.externalName ? <CopyValue value={resource.externalName} label="managed resource cloud ID" /> : <Text>Not assigned</Text> },
    { key: "state", header: "State", width: proportional(1, { minWidth: 80 }), renderCell: (resource) => statusValue(ManagedResourceState, resource.state) },
    { key: "paused", header: "Paused", width: proportional(1, { minWidth: 80 }), renderCell: (resource) => <Text maxLines={1}>{resource.paused ? "Paused" : "Active"}</Text> },
    { key: "ready", header: "Ready", width: proportional(1, { minWidth: 80 }), renderCell: (resource) => <Text maxLines={1}>{label(ConditionStatus, resource.ready)}</Text> },
    { key: "synced", header: "Synced", width: proportional(1, { minWidth: 80 }), renderCell: (resource) => <Text maxLines={1}>{label(ConditionStatus, resource.synced)}</Text> },
    { key: "reason", header: "Reason", width: proportional(1, { minWidth: 80 }), renderCell: (resource) => <Text maxLines={1}>{resource.reason || "—"}</Text> },
    { key: "message", header: "Message", width: proportional(2, { minWidth: 80 }), renderCell: (resource) => <Text maxLines={1}>{resource.message || "No message reported"}</Text> },
  ];
  return <VStack gap={4}>
    <HStack gap={4} wrap="wrap" hAlign="between" vAlign="center">
      <VStack gap={1}><Heading level={3}>Cloud resources</Heading><Text type="supporting" role="status">{matching.length} of {resources.length} resources · {resources.filter((resource) => resource.paused).length} paused</Text></VStack>
    </HStack>
    <HStack gap={3} wrap="wrap" vAlign="end">
      <TextInput label="Search resources" placeholder="Name, kind or cloud ID" type="search" value={search} onChange={setSearch} hasClear autoComplete="off" />
      <Selector label="Resource kind" hasSearch value={kind} onChange={setKind} options={[{ value: "all", label: "All kinds" }, ...kinds.map((value) => ({ value, label: value || "Unspecified" }))]} />
      <Selector label="State" value={state} onChange={setState} options={[{ value: "all", label: "All states" }, ...states.map((value) => ({ value: String(value), label: label(ManagedResourceState, value) }))]} />
      <Selector label="Health" value={health} onChange={setHealth} options={[{ value: "all", label: "All health" }, { value: "ready", label: "Ready and synced" }, { value: "attention", label: "Needs attention" }]} />
      <Selector label="Paused" value={paused} onChange={setPaused} options={[{ value: "all", label: "All resources" }, { value: "paused", label: "Paused" }, { value: "active", label: "Active" }]} />
      <Button label="Clear resource filters" variant="ghost" onClick={() => { setSearch(""); setKind("all"); setState("all"); setHealth("all"); setPaused("all"); }} />
    </HStack>
    {matching.length ? <Table aria-label={title} data={matching} columns={columns} idKey={(resource) => `${resource.kind}:${resource.name}`} /> : <Text>{resources.length ? "No resources match these filters." : "No managed resources reported yet."}</Text>}
  </VStack>;
}

function DetailRows({ title, rows }: { title: string; rows: { name: string; state: ReactNode; detail: ReactNode; href?: string; anchor?: string; copyName?: string; activate?: () => void }[] }) {
  type Row = { id: number; name: string; state: ReactNode; detail: ReactNode; href?: string; anchor?: string; copyName?: string; activate?: () => void };
  const columns: TableColumn<Row>[] = [
    { key: "name", header: "Name", width: proportional(2, { minWidth: 80 }), renderCell: (row) => <HStack id={row.anchor} gap={2} wrap="wrap" vAlign="center">{row.href ? <Link href={row.href} onClick={row.activate}>{row.name}</Link> : <Text>{row.name}</Text>}{row.copyName && <CopyButton value={row.copyName} label="resource name" />}</HStack> },
    { key: "state", header: "Status", width: proportional(1, { minWidth: 80 }), renderCell: (row) => typeof row.state === "string" ? <Text>{row.state}</Text> : row.state },
    { key: "detail", header: "Details", width: proportional(3, { minWidth: 80 }), renderCell: (row) => typeof row.detail === "string" ? <Text>{row.detail}</Text> : row.detail },
  ];
  return rows.length ? <Table aria-label={title} data={rows.map((row, id) => ({ ...row, id }))} columns={columns} idKey="id" textOverflow="wrap" density="compact" hasHover /> : <Text type="supporting">No {title.toLowerCase()} reported.</Text>;
}

function EditCluster({ clusterId, currentEtag, onClose, onApplied }: { clusterId: string; currentEtag: string; onClose: () => void; onApplied: () => void }) {
  const { api, tenantId } = usePlatform();
  const load = useCallback((signal: AbortSignal) => api.cluster.getCluster({ tenantId, clusterId, asRequested: true }, { signal }), [api, tenantId, clusterId]);
  const requested = useAsyncQuery(load);
  if (requested.data?.cluster) return <ClusterEditor key={requested.data.cluster.etag} baseline={requested.data.cluster} asRequested={requested.data.asRequested} currentEtag={currentEtag} onApplied={onApplied} onClose={onClose} onReload={requested.refresh} />;
  return <WizardDialog isOpen onOpenChange={(open) => { if (!open) onClose(); }} title="Edit cluster" steps={clusterWizardSteps} step={0}>
    <ErrorNotice message={requested.error} />
    {requested.loading && <Text>Loading the original request…</Text>}
    <HStack gap={3}><Button label="Reload editable cluster" variant="secondary" onClick={requested.refresh} /><Button label="Cancel editing" variant="ghost" onClick={onClose} /></HStack>
  </WizardDialog>;
}

function ClusterEditor({ baseline, asRequested, currentEtag, onApplied, onClose, onReload }: { baseline: Cluster; asRequested: boolean; currentEtag: string; onApplied: () => void; onClose: () => void; onReload: () => void }) {
  const { api, tenantId, canWrite } = usePlatform();
  const [form, setForm] = useState(() => clusterForm(baseline));
  const [step, setStep] = useState(0);
  const formId = useId();
  const [touched, setTouched] = useState<Set<string>>(new Set());
  const [review, setReview] = useState<ReviewedClusterPlan>();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const locked = useRef(false);
  const revision = useRef(0);
  const errorRef = useRef<HTMLElement>(null);
  function change<K extends keyof ClusterForm>(field: K, value: ClusterForm[K]) {
    revision.current++;
    setReview(undefined); setError("");
    setForm((old) => ({ ...old, [field]: value }));
    setTouched((old) => new Set([...old, field]));
  }
  async function submit(apply: boolean) {
    if (!canWrite || locked.current) return;
    if (!apply && step < 2) {
      try { validateClusterFormStep(form, step, touched); setStep(step + 1); setError(""); }
      catch (cause) { setError(errorMessage(cause)); requestAnimationFrame(() => errorRef.current?.focus()); }
      return;
    }
    locked.current = true; setBusy(true); setError("");
    const inputRevision = revision.current;
    try {
      if (baseline.etag !== currentEtag) throw new Error("The cluster changed. Reload editable cluster and review a new plan.");
      const request = buildClusterUpdate(tenantId, baseline, form, touched);
      if (apply) {
        if (!review) throw new Error("Review a plan first.");
        const response = await api.cluster.updateCluster(applyReviewedClusterPlan(review, request));
        if (!response.updateRecorded) throw new Error("The server did not acknowledge the update. Reload and review again.");
        onApplied();
      } else {
        setReview(undefined);
        const response = await api.cluster.updateCluster(request);
        if (!response.plan) throw new Error("The server returned no plan. Apply is blocked.");
        if (revision.current === inputRevision) { setReview(bindClusterPlan(request, response.plan)); setStep(3); }
      }
    } catch (cause) { setReview(undefined); setError(errorMessage(cause)); requestAnimationFrame(() => errorRef.current?.focus()); }
    finally { locked.current = false; setBusy(false); }
  }
  return <WizardDialog isOpen onOpenChange={(open) => { if (!open && !locked.current) onClose(); }} title="Edit cluster" steps={clusterWizardSteps} step={step} busy={busy}
    description="Only edited fields are sent. Review the server plan before applying."
    footer={<HStack gap={3} wrap="wrap">
      <Button label="Cancel editing" variant="secondary" onClick={onClose} isDisabled={busy} />
      {step > 0 && <Button label="Back" variant="secondary" onClick={() => { setStep(step - 1); setError(""); }} isDisabled={busy} />}
      <Button label={step === 3 ? "Apply reviewed plan" : step === 2 ? "Preview update plan" : "Next"} type="submit" form={formId} isLoading={busy} isDisabled={!canWrite || (step === 3 && (!review || !clusterPlanApplicable(review.plan) || baseline.etag !== currentEtag))} />
    </HStack>}>
    {!asRequested && <Banner status="info" title="Original input unavailable" description="The server returned resolved defaults. Only fields you edit will be included in the update mask." />}
    {baseline.etag !== currentEtag && <Banner status="warning" title="Cluster changed" description="Reload editable cluster before previewing or applying changes." />}
    {baseline.etag !== currentEtag && <Button label="Reload editable cluster" onClick={onReload} isDisabled={busy} />}
    <VStack ref={errorRef} tabIndex={-1} role="alert"><ErrorNotice message={error} /></VStack>
    <form id={formId} method="post" onSubmit={(event) => { event.preventDefault(); void submit(step === 3); }}>
      <VStack gap={6}>
        {step < 3 && <ClusterFields step={step} value={form} onChange={change} disabled={busy || !canWrite} region={baseline.spec?.region ?? ""} tier={baseline.status?.tier} />}
    {step === 3 && review && <VStack gap={4}>
      <PlanReview plan={review.plan} />
      {touched.has("naas") && <Banner status={form.naas ? "info" : "warning"} title={form.naas ? "Enable Namespace as a Service" : "Disable Namespace as a Service"} description={form.naas ? "Enable support for team namespaces with separate access, quotas, and policies. Setup continues after you apply this change." : "Turn off namespace tenancy controls for this cluster. Existing namespaces, workloads, and data will remain. Review the impact on teams using these controls before applying."} />}
      <MetadataList><MetadataListItem label="Edited settings">{[...touched].filter((field) => !(field === "kubernetesVersion" && Number(form.upgradeMode) === UpgradeMode.AUTOMATIC)).map((field) => field.replace(/([A-Z])/g, " $1").toLowerCase()).join(", ")}</MetadataListItem><MetadataListItem label="Concurrency token">{baseline.etag}</MetadataListItem></MetadataList>
      <Collapsible trigger="Advanced · exact reviewed request" defaultIsOpen={false}><VStack as="pre" isScrollable><Text type="code">{toJsonString(UpdateClusterRequestSchema, review.request, { prettySpaces: 2 })}</Text></VStack></Collapsible>
    </VStack>}
      </VStack>
    </form>
  </WizardDialog>;
}

function DeleteCluster({ cluster, onCancel, onDeleted }: { cluster: Cluster; onCancel: () => void; onDeleted: () => void }) {
  const { api, tenantId, canWrite } = usePlatform();
  const [confirmation, setConfirmation] = useState("");
  const [force, setForce] = useState(false);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const locked = useRef(false);
  return <Dialog isOpen onOpenChange={(open) => { if (!open && !locked.current) onCancel(); }} purpose={busy ? "required" : "form"} width="min(42rem, calc(100vw - 2rem))">
    <DialogHeader title="Confirm cluster deletion" onOpenChange={busy ? undefined : (open) => { if (!open && !locked.current) onCancel(); }} />
    <VStack gap={4} padding={4}>
    <Text>Normal deletion removes the cluster, node pools and owned cloud resources. The shared network remains. Imported observe-only resources and resources marked shared are retained.</Text>
    <form method="post" onSubmit={(event) => {
      event.preventDefault();
      if (!canWrite || locked.current) return;
      if (confirmation !== cluster.name) { setError(`Type ${cluster.name} exactly to confirm deletion.`); return; }
      if (!cluster.etag) { setError("Reload details before deleting: the cluster has no etag."); return; }
      locked.current = true; setBusy(true); setError("");
      void api.cluster.deleteCluster({ tenantId, clusterId: cluster.id, etag: cluster.etag, force }).then(onDeleted).catch((cause) => setError(errorMessage(cause))).finally(() => { locked.current = false; setBusy(false); });
    }}>
      <VStack gap={4}>
        <VStack role="alert"><ErrorNotice message={error} /></VStack>
        <CheckboxInput label="Force deletion" value={force} onChange={(next) => { setForce(next); setConfirmation(""); setError(""); }} isDisabled={busy} description="For an unreachable Kubernetes API: skip Helm uninstall. Chart-created load balancers or volumes may remain in the cloud. Force is sticky once deletion starts." />
        <TextInput label={`Type ${cluster.name} to confirm`} value={confirmation} onChange={(next) => { setConfirmation(next); setError(""); }} isDisabled={busy} autoComplete="off" />
        <HStack gap={3} wrap="wrap"><Button label="Cancel deletion" variant="secondary" onClick={onCancel} isDisabled={busy} /><Button label={force ? "Confirm forced deletion" : "Confirm normal deletion"} variant="destructive" type="submit" isLoading={busy} isDisabled={!canWrite} /></HStack>
      </VStack>
    </form>
    </VStack>
  </Dialog>;
}

function RetryCluster({ clusterId, onRetried, onClose }: { clusterId: string; onRetried: () => void; onClose: () => void }) {
  const { api, tenantId, canWrite } = usePlatform();
  const [names, setNames] = useState("");
  const [upgrade, setUpgrade] = useState(false);
  const [error, setError] = useState("");
  const [result, setResult] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const locked = useRef(false);
  return <Dialog isOpen onOpenChange={(open) => { if (!open && !locked.current) onClose(); }} purpose={busy ? "required" : "form"} width="min(42rem, calc(100vw - 2rem))">
    <DialogHeader title="Retry failed work" onOpenChange={busy ? undefined : (open) => { if (!open && !locked.current) onClose(); }} />
    <VStack gap={4} padding={4}>
    <form method="post" onSubmit={(event) => {
      event.preventDefault();
      if (!canWrite || locked.current) return;
      locked.current = true; setBusy(true); setError(""); setResult([]);
      void api.cluster.retryCluster({ tenantId, clusterId, components: names.split(/[\s,]+/).filter(Boolean), upgrade })
        .then((response) => { setResult(response.items.map((item) => `${item.name}: ${label(RetryAction, item.action)} · ${item.message}`)); onRetried(); })
        .catch((cause) => setError(errorMessage(cause))).finally(() => { locked.current = false; setBusy(false); });
    }}>
      <VStack gap={4}>
        <VStack role="alert"><ErrorNotice message={error} /></VStack>
        <TextInput label="Components to retry" description="Optional comma-separated names. Empty with upgrade unchecked retries all failed work." value={names} onChange={setNames} isDisabled={busy} />
        <CheckboxInput label="Retry failed automatic upgrade" value={upgrade} onChange={setUpgrade} isDisabled={busy} />
        <Button label="Request retry" type="submit" isLoading={busy} isDisabled={!canWrite} />
        <Button label="Close retry" variant="secondary" onClick={onClose} isDisabled={busy} />
        <VStack role="status">{result.map((item, index) => <Text key={index}>{item}</Text>)}</VStack>
      </VStack>
    </form>
    </VStack>
  </Dialog>;
}
