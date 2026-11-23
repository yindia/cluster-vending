"use client";

import { SearchFilters } from "./filter-row";
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
import { pixel, proportional, type TableColumn } from "@astryxdesign/core/Table";
import { ManagedResourcePreviews } from "./managed-resource-previews";
import { ClusterSubTenants } from "./cluster-subtenants";
import { clusterSections } from "./cluster-sections";
import { DetailNavigation, useDetailSection } from "./detail-navigation";
import { ComponentPhase } from "@/gen/cloud/v1/component_pb";
import { NodeGroupPhase } from "@/gen/cloud/v1/node_group_pb";
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
  validateClusterFormStep, buildImportCluster, platformManagedClusterFields, type ClusterImportForm,
  createRequestIdentity, emptyClusterForm,
  type ClusterForm, type ReviewedClusterPlan,
} from "@/lib/workflows/clusters";
import {
  ClusterState, ClusterRolloutResult, ClusterUpgradePhase, UpgradeMode, type Cluster, type PlatformComponent,
} from "@/gen/cloud/v1/cluster_pb";
import { CreateClusterRequestSchema, ImportClusterRequestSchema, UpdateClusterRequestSchema, type CreateClusterRequest, type ImportClusterRequest } from "@/gen/cloud/v1/cluster_service_pb";
import { platformInstallationStatus } from "@/lib/workflows/installation-status";
import { awsAddonInventory } from "@/lib/providers/aws-addons";
import { ManagedResourceState, type ManagedResource } from "@/gen/cloud/v1/managed_resource_pb";
import { ConditionStatus } from "@/gen/cloud/v1/conditions_pb";
import { Disposition, ImportPhase } from "@/gen/cloud/v1/import_pb";
import { RetryAction } from "@/gen/cloud/v1/cluster_retry_pb";
import type { Network } from "@/gen/cloud/v1/network_pb";
import { ResourceWorkflowDialog, ManifestCreateDialog } from "./manifest-create-dialog";
import { accountDescription } from "@/lib/workflows/cluster-placement";
import { supportsCapability } from "@/lib/providers/registry";
import { ClusterAccessButton } from "./cluster-access";
import { ClusterActions } from "./cluster-takeover";
import { DetailHeader, DetailSummary, StatusValue } from "./detail-layout";
import { ClusterWorkloadSummary } from "./cluster-workload-summary";
import { CopyButton, CopyValue } from "./copy-value";
import { ResourceYamlButton } from "./resource-yaml-button";
import { ClusterExtraFields, ClusterExtraReview, useClusterFieldRows } from "./cluster-form-fields";
import { ClusterTopology } from "./cluster-topology";
import { ClusterConfiguration } from "./cluster-configuration";
import { ClusterNodeGroups } from "./cluster-node-groups";
import { RefreshButton } from "./refresh-button";
import { ProviderIcon, KubernetesIcon } from "./provider-icons";
import { ActivityEventTable, ActivityEventFilters, filterActivityEvents, type ActivityFilters } from "./activity-event-table";
import { ClusterDeprecationsPanel } from "./cluster-deprecations";
import { ClusterRecommendationsPanel } from "./cluster-recommendations";
import { ClusterViolationsPanel } from "./cluster-violations";
import { ClusterComponentsPanel } from "./cluster-tenant-components";

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
  const { api, tenantId } = usePlatform();
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
    <DetailHeader title="Clusters" description="Manage Kubernetes clusters, node groups, and upgrades." actions={<>
        {<><Button label="Create cluster" variant="primary" onClick={() => setCreating(true)} /><Button label="Import cluster" variant="secondary" onClick={() => setImporting(true)} /></>}
        <RefreshButton label="Refresh clusters" isLoading={list.refreshing || networks.refreshing} onClick={() => { list.refresh(); networks.refresh(); }} />
    </>} />
    {acknowledgement && <VStack role="status"><Text>{acknowledgement}</Text></VStack>}
    {acceptedId && <Link href={`/clusters/${encodeURIComponent(acceptedId)}`}>View cluster details and readiness</Link>}
    <SearchFilters label="Cluster filters" values={[stateFilter, pauseFilter, networkFilter, channelFilter, tierFilter]}
      search={<TextInput label="Search clusters" value={search} onChange={changeFilter(setSearch)} type="search" placeholder="Name, ID, network or region" hasClear autoComplete="off" />}>
      <Selector label="Status" value={stateFilter} onChange={changeFilter(setStateFilter)} options={[{ value: "all", label: "All states" }, ...[...new Set(inventory.map((cluster) => cluster.status?.state ?? 0))].sort((a, b) => a - b).map((state) => ({ value: String(state), label: label(ClusterState, state) }))]} />
      <Selector label="Filter by pause" value={pauseFilter} onChange={changeFilter(setPauseFilter)} options={[{ value: "all", label: "All pause states" }, { value: "paused", label: "Paused" }, { value: "let-through", label: "Let through" }, { value: "resources-paused", label: "Resources paused" }, { value: "not-paused", label: "Not paused" }]} />
      <Selector label="Network" hasSearch value={networkFilter} onChange={changeFilter(setNetworkFilter)} options={[{ value: "all", label: "All networks" }, ...networkIds.map((id) => ({ value: id, label: networkById.get(id)?.displayName || networkById.get(id)?.name || id }))]} />
      <Selector label="Channel" value={channelFilter} onChange={changeFilter(setChannelFilter)} options={[{ value: "all", label: "All channels" }, ...[0, 1, 2, 3].map((channel) => ({ value: String(channel), label: channelName(channel) }))]} />
      <Selector label="Tier" value={tierFilter} onChange={changeFilter(setTierFilter)} options={[{ value: "all", label: "All tiers" }, { value: "0", label: "Default" }, ...[1, 2, 3].map((tier) => ({ value: String(tier), label: `Tier ${tier - 1}` }))]} />
      <Button label="Clear filters" variant="ghost" onClick={() => { setSearch(""); setStateFilter("all"); setPauseFilter("all"); setNetworkFilter("all"); setChannelFilter("all"); setTierFilter("all"); setPage(1); }} />
    </SearchFilters>
    <ErrorNotice message={list.error} />
    <ErrorNotice message={networks.error} />
    {list.loading && <Text role="status">Loading all clusters…</Text>}
    {!!list.data && <VStack gap={4}>
      <Text type="supporting" role="status">{filtered.length} of {inventory.length} clusters</Text>
      {visible.length ? <Table data={visible.map((cluster) => ({ id: cluster.id, cluster }))} columns={columns} idKey="id" aria-label="Clusters" rowIndexStart={(currentPage - 1) * 25 + 1} rowCount={filtered.length} /> : <Text>{inventory.length ? "No clusters match these filters." : "No clusters found for this tenant."}</Text>}
      {filtered.length > 25 && <Pagination label="Clusters pagination" page={currentPage} onChange={setPage} totalItems={filtered.length} pageSize={25} variant="count" />}
    </VStack>}
    {creating && <CreateCluster onClose={() => setCreating(false)} onCreated={(id) => { setCreating(false); setAcceptedId(id); setAcknowledgement("Create request recorded. Readiness is reported separately in cluster details."); list.refresh(); }} />}
    {importing && <ImportCluster onClose={() => setImporting(false)} onImported={(id) => { setImporting(false); setAcceptedId(id); setAcknowledgement("Import request recorded. Discovery is asynchronous; cloud resources remain read-only until takeover."); list.refresh(); }} />}
  </VStack>;
}

function tierLabel(tier?: number) {
  return tier ? `Tier ${tier - 1} (from network)` : "Network default";
}

const platformManagedNote = "Managed by the platform: the system node group, which runs Karpenter and the platform's components.";

interface FieldsProps {
  value: ClusterForm;
  onChange: <K extends keyof ClusterForm>(field: K, value: ClusterForm[K]) => void;
  disabled: boolean;
  creating?: boolean;
  naasLockReason?: string;
  rows: ReturnType<typeof useClusterFieldRows>;
  baseline?: Cluster;
  networkPicker?: ReactNode;
  networkName?: string;
  region: string;
  tier?: number;
  step: number;
}

function ClusterFields({ value, onChange, disabled, creating, networkPicker, networkName, region, tier, step, naasLockReason, rows, baseline }: FieldsProps) {
  const text = (field: keyof ClusterForm, title: string, description?: string, required = false, locked = false) => <TextInput
    label={title} value={String(value[field])} onChange={(next) => { if (!locked) onChange(field, next); }}
    description={locked ? platformManagedNote : description} isRequired={required && !locked} isReadOnly={locked} isDisabled={disabled}
    autoComplete="off" autoCapitalize="none" hasAutoCorrect={false} hasSpellCheck={false}
  />;
  return <VStack gap={5}>
    {step === 0 && <>
      {creating ? <FormLayout>
        <TextInput label="Cluster name" value={value.name} onChange={(next) => onChange("name", next)} placeholder="production-eks" isRequired isDisabled={disabled} autoComplete="off" autoCapitalize="none" hasAutoCorrect={false} hasSpellCheck={false} description="1–40 lowercase letters, digits and hyphens. Start with a letter and end with a letter or digit." />
        {networkPicker}
      </FormLayout> : <>
        {text("displayName", "Display name", "Optional name shown in the dashboard.")}
        <VStack data-placement-inline=""><Text type="supporting">{networkName || value.networkId} · {region} · {tierLabel(tier)}</Text></VStack>
      </>}
      <Divider />
      <VStack gap={3}>
        <Heading level={4}>Release policy</Heading>
        <FormLayout direction="horizontal" data-workflow-fields="">
          <Selector label="Release channel" value={value.releaseChannel} isDisabled={disabled} onChange={(next) => onChange("releaseChannel", next)} options={[
            { value: "0", label: Number(value.upgradeMode) === UpgradeMode.AUTOMATIC ? "Stable (default)" : "None (default)" }, { value: "1", label: "Alpha" }, { value: "2", label: "Beta" }, { value: "3", label: "Stable" },
          ]} />
          <Selector label="Upgrades" value={value.upgradeMode} isDisabled={disabled} onChange={(next) => onChange("upgradeMode", next)} options={[{ value: "1", label: "Manual" }, { value: "2", label: "Automatic" }]} />
        </FormLayout>
        <Text type="supporting">{Number(value.upgradeMode) === UpgradeMode.AUTOMATIC ? "Kubernetes upgrades follow the selected release channel." : "You choose when to upgrade Kubernetes."}</Text>
      </VStack>
      <CheckboxInput label="Namespace as a Service (NaaS)" htmlName="naas" value={value.naas} isDisabled={disabled || !!naasLockReason} onChange={(next) => onChange("naas", next)} description={naasLockReason || "Enable support for team namespaces with separate access, quotas, and policies."} />
    </>}
    {step === 1 && <>
      <Text color="secondary">Configure the default node pool across the selected availability zones.</Text>
      {text("instanceTypes", "Instance types", "Comma-separated AWS instance types.", true)}
      <VStack gap={3}>
        <Heading level={4}>Scaling limits</Heading>
        <FormLayout direction="horizontal" data-workflow-fields="">
          {text("minSize", "Minimum nodes", undefined, true, !creating)}
          {creating ? text("desiredSize", "Initial desired nodes") : <TextInput label="Initial desired nodes" value={value.desiredSize} isReadOnly autoComplete="off" />}
          {text("maxSize", "Maximum nodes", undefined, true, !creating)}
        </FormLayout>
        {!creating && <Text type="supporting">Managed by the platform: the default node group runs Karpenter and the platform&apos;s components. Add capacity for workloads with node groups.</Text>}
        {creating && <Text type="supporting">Leave the initial count empty to start at the minimum, or at 1 if the minimum is 0.</Text>}
      </VStack>
      <Selector label="Capacity" value={value.capacityType} isDisabled={disabled} onChange={(next) => onChange("capacityType", next)} options={[{ value: "0", label: "On demand · server default" }, { value: "1", label: "On demand" }, { value: "2", label: "Spot" }]} />
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
    </>}
    <VStack gap={4} data-advanced-options="">
      {step === 0 && (creating || Number(value.upgradeMode) !== UpgradeMode.AUTOMATIC) && <Collapsible trigger={creating ? "Advanced identity and version" : "Kubernetes version"} defaultIsOpen={false}>
        <VStack gap={4} paddingBlock={3}>
          {creating && text("displayName", "Display name", "Optional friendly name.")}
          {Number(value.upgradeMode) !== UpgradeMode.AUTOMATIC && text("kubernetesVersion", "Kubernetes minor version", "Optional. Leave empty for the server default.")}
        </VStack>
      </Collapsible>}
      {step === 1 && <Collapsible trigger="Advanced storage" defaultIsOpen={false}><VStack paddingBlock={3}>{text("diskSize", "Root disk (GiB)", "Leave empty for the provider default, or enter 20–16384 GiB.")}</VStack></Collapsible>}
      {step === 2 && <Collapsible trigger="Advanced permissions and identity" defaultIsOpen={false}>
        <VStack gap={4} paddingBlock={3}>
          {text("adminPrincipals", "Cluster administrators", "Optional comma-separated IAM role or user ARNs.")}
          <Selector label="AWS service account roles (IRSA)" value={value.irsa} isDisabled={disabled} onChange={(next) => onChange("irsa", next)} options={[{ value: "default", label: "Enabled · server default" }, { value: "true", label: "Enabled" }, { value: "false", label: "Disabled" }]} />
        </VStack>
      </Collapsible>}
      <ClusterExtraFields value={value} onChange={onChange} disabled={disabled} creating={!!creating} step={step} rows={rows} baseline={baseline} />
    </VStack>
  </VStack>;
}

function ClusterReview({ form, region, networkName, accountName, tier }: { form: ClusterForm; region: string; networkName: string; accountName?: string; tier?: number }) {
  const automatic = Number(form.upgradeMode) === UpgradeMode.AUTOMATIC;
  return <VStack gap={5}>
    <MetadataList columns="multi" title={<Heading level={4}>Placement</Heading>}>
      <MetadataListItem label="Cluster name">{form.name}</MetadataListItem>
      {form.displayName && <MetadataListItem label="Display name">{form.displayName}</MetadataListItem>}
      <MetadataListItem label="Cloud account">{accountName}</MetadataListItem>
      <MetadataListItem label="Network">{networkName}</MetadataListItem>
      <MetadataListItem label="Region / tier">{region} · {tierLabel(tier)}</MetadataListItem>
    </MetadataList>
    <Divider />
    <MetadataList columns="multi" title={<Heading level={4}>Runtime and capacity</Heading>}>
      <MetadataListItem label="Release channel">{[automatic ? "Stable · server default" : "None · server default", "Alpha", "Beta", "Stable"][Number(form.releaseChannel)]}</MetadataListItem>
      <MetadataListItem label="Upgrade policy">{automatic ? "Automatic · no version pin" : `Manual · ${form.kubernetesVersion || "server default version"}`}</MetadataListItem>
      <MetadataListItem label="Node pool">{form.instanceTypes} · {form.minSize}–{form.maxSize} nodes · {form.capacityType === "2" ? "Spot" : "On demand"}</MetadataListItem>
      <MetadataListItem label="Initial desired nodes">{form.desiredSize || Math.max(1, Number(form.minSize))}</MetadataListItem>
      <MetadataListItem label="Root disk">{form.diskSize ? `${form.diskSize} GiB` : "Provider default"}</MetadataListItem>
      <MetadataListItem label="Namespace as a Service (NaaS)">{form.naas ? "Enabled" : "Disabled"}</MetadataListItem>
    </MetadataList>
    <Divider />
    <MetadataList columns="multi" title={<Heading level={4}>Access</Heading>}>
      <MetadataListItem label="API endpoints">{[form.privateAccess && "Private", form.publicAccess && "Public"].filter(Boolean).join(" + ")}</MetadataListItem>
      {form.publicAccess && <MetadataListItem label="Allowed sources">{form.publicCIDRs}</MetadataListItem>}
      <MetadataListItem label="Administrators">{form.adminPrincipals || "No additional principals"}</MetadataListItem>
      <MetadataListItem label="Service account roles">{form.irsa === "false" ? "Disabled" : "Enabled"}</MetadataListItem>
    </MetadataList>
    <ClusterExtraReview form={form} creating />
  </VStack>;
}

function useClusterNetworks() {
  const { api, tenantId } = usePlatform();
  const load = useCallback(async (signal: AbortSignal) => {
    const [accounts, networks] = await Promise.all([
      api.tenant.listCloudAccounts({ tenantId }, { signal }),
      collectPages(async (pageToken) => {
        const page = await api.network.listNetworks({ tenantId, pageSize: 200, pageToken }, { signal });
        return { items: page.networks, nextPageToken: page.nextPageToken };
      }, signal),
    ]);
    return { networks, accounts: accounts.accounts };
  }, [api, tenantId]);
  return useAsyncQuery(load);
}

function NetworkPlacement({ network, choices, cluster }: { network?: Network; choices?: ReturnType<typeof useClusterNetworks>; cluster?: Cluster }) {
  const networkName = network?.displayName || network?.name || cluster?.spec?.network?.networkId;
  if (!networkName) return null;
  const account = choices?.data?.accounts.find((item) => item.id === network?.spec?.bootstrapId);
  const tier = network?.spec?.tier || cluster?.status?.tier;
  return <VStack gap={3}>
    <Divider />
    <VStack gap={1}><Text type="supporting" color="secondary">Selected network</Text><Heading level={4}>{networkName}</Heading></VStack>
    <MetadataList label={{ position: "top" }}>
      {choices && <MetadataListItem label="Cloud account">{accountDescription(account)}</MetadataListItem>}
      <MetadataListItem label="Region">{network?.spec?.region || cluster?.spec?.region}</MetadataListItem>
      <MetadataListItem label="Inherited tier">{tier ? `Tier ${tier - 1}` : "Network default"}</MetadataListItem>
    </MetadataList>
  </VStack>;
}

function NetworkPicker({ choices, value, disabled, onChange, operation }: { choices: ReturnType<typeof useClusterNetworks>; value: string; disabled: boolean; onChange: (value: string) => void; operation: "cluster.create" | "cluster.import" }) {
  const networks = choices.data?.networks ?? [];
  const selected = networks.find((item) => item.id === value);
  const accountFor = (item: Network) => choices.data?.accounts.find((account) => account.id === item.spec?.bootstrapId);
  const options = networks.map((item) => {
    const account = accountFor(item);
    const supported = supportsCapability(account?.provider.case ?? "", operation);
    return { value: item.id, label: `${item.displayName || item.name} · ${item.spec?.region || "Unknown region"}`, disabled: !supported, description: supported ? accountDescription(account) : "Unsupported or missing cloud account" };
  });
  return <VStack gap={3}>
    <ErrorNotice message={choices.error} />
    <HStack gap={2} vAlign="end" width="100%">
      <StackItem size="fill"><Selector label="Existing network" hasSearch value={value} isLoading={choices.loading} isDisabled={disabled || choices.loading || !options.length} placeholder="Choose a network" onChange={onChange} options={options} /></StackItem>
      <RefreshButton label="Refresh networks and accounts" onClick={choices.refresh} isDisabled={disabled} isLoading={choices.refreshing} />
    </HStack>
    {selected && <VStack data-placement-inline=""><Text type="supporting">{accountDescription(accountFor(selected))} · {tierLabel(selected.spec?.tier)}</Text></VStack>}
    {choices.data && !choices.error && !options.some((option) => !option.disabled) &&
      <Text type="supporting">{options.length ? "This tenant has no networks with a supported cloud account." : "Create or import a network in this tenant, then refresh this list."}</Text>}
  </VStack>;
}

function CreateCluster({ onClose, onCreated }: { onClose: () => void; onCreated: (id: string) => void }) {
  const { api, tenantId } = usePlatform();
  const [form, setForm] = useState<ClusterForm>({ ...emptyClusterForm });
  const rows = useClusterFieldRows(form);
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
    if (locked.current) return;
    try {
      if (step < 3) {
        rows.validate(step);
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
  <ResourceWorkflowDialog method={{ label: "Creation method", value: mode, onChange: (next) => { if (next === "yaml") setManifestVisited(true); setMode(next); } }} isOpen onOpenChange={(open) => { if (!open && !locked.current) onClose(); }} title="Create cluster" steps={clusterWizardSteps} step={step} busy={busy || manifestBusy}
    placement={<NetworkPlacement network={network} choices={choices} />}
    yamlContent={manifestVisited ? <ManifestCreateDialog kind="Cluster" embedded isOpen={mode === "yaml"} onBusyChange={setManifestBusy} onOpenChange={(open: boolean) => { if (!open) onClose(); }} onCreated={(resource: Cluster) => onCreated(resource.id)} /> : undefined}
    description={`Provision a managed cluster in tenant ${tenantId}.`}
    footer={<HStack gap={3} wrap="wrap">
      <Button label="Cancel creation" variant="secondary" onClick={onClose} isDisabled={busy} />
      {step > 0 && <Button label="Back" variant="secondary" onClick={() => { setStep(step - 1); setError(""); }} isDisabled={busy} />}
      <Button label={step === 3 ? "Create cluster" : "Next"} variant="primary" type="submit" form={formId} isLoading={busy} />
    </HStack>}>
    <form id={formId} method="post" aria-label="Cluster creation settings" onSubmit={(event) => { event.preventDefault(); void submit(); }}>
      <VStack gap={5}>
        {error && <VStack ref={errorRef} tabIndex={-1} role="alert"><ErrorNotice message={error} /></VStack>}
        {step < 3 ? <ClusterFields step={step} value={form} onChange={change} rows={rows} disabled={busy} creating region={network?.spec?.region ?? "Select a network"} tier={network?.spec?.tier}
          networkPicker={<NetworkPicker operation="cluster.create" choices={choices} value={form.networkId} disabled={busy} onChange={(next) => change("networkId", next)} />} /> : <>
          <ClusterReview form={form} region={network?.spec?.region ?? ""} networkName={network?.displayName || network?.name || form.networkId} accountName={accountDescription(choices.data?.accounts.find((account) => account.id === network?.spec?.bootstrapId))} tier={network?.spec?.tier} />
          <Banner status="info" title="Creates managed cloud resources" description="The controller provisions the control plane and node pool asynchronously. Readiness appears on the detail page; cloud charges may apply." />
          <Collapsible trigger="Advanced · exact API request" defaultIsOpen={false}><VStack as="pre" isScrollable><Text type="code">{review && toJsonString(CreateClusterRequestSchema, review, { prettySpaces: 2 })}</Text></VStack></Collapsible>
        </>}
      </VStack>
    </form>
  </ResourceWorkflowDialog>
  </>;
}

function ImportCluster({ onClose, onImported }: { onClose: () => void; onImported: (id: string) => void }) {
  const { api, tenantId } = usePlatform();
  const [form, setForm] = useState<ClusterImportForm>({ name: "", displayName: "", networkId: "", defaultNodeGroupName: "" });
  const [step, setStep] = useState(0);
  const [mode, setMode] = useState<"guided" | "yaml">("guided");
  const [manifestVisited, setManifestVisited] = useState(false);
  const [manifestBusy, setManifestBusy] = useState(false);
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
    if (locked.current) return;
    try {
      if (!network || !choices.data) throw new Error("Choose an existing network.");
      if (step === 0) {
        setReview(buildImportCluster(tenantId, form, network, choices.data.accounts, identity.current.get()));
        setError(""); setStep(1); return;
      }
      if (!review) throw new Error("Review the import request first.");
      locked.current = true; setBusy(true); setError("");
      const result = await api.cluster.importCluster(review);
      if (!result.cluster?.id) throw new Error("No imported cluster ID was returned. Retry unchanged inputs with the same request ID.");
      onImported(result.cluster.id);
    } catch (cause) { setError(errorMessage(cause)); requestAnimationFrame(() => errorRef.current?.focus()); }
    finally { locked.current = false; setBusy(false); }
  }
  return <ResourceWorkflowDialog title="Import cluster" isOpen
    onOpenChange={(open) => { if (!open && !locked.current) onClose(); }}
    method={{ label: "Import method", value: mode, onChange: (next) => { if (next === "yaml") setManifestVisited(true); setMode(next); } }}
    description={`Register an existing cluster for read-only discovery in tenant ${tenantId}.`}
    steps={["Existing infrastructure", "Review import"]} step={step} busy={busy || manifestBusy}
    placement={<NetworkPlacement network={network} choices={choices} />}
    yamlContent={manifestVisited && <ManifestCreateDialog kind="ClusterImport" embedded isOpen={mode === "yaml"} onBusyChange={setManifestBusy} onOpenChange={open => { if (!open) onClose(); }} onCreated={resource => onImported(resource.id)} />}
    footer={<HStack gap={3} wrap="wrap">
      <Button type="button" label="Cancel import" variant="secondary" onClick={onClose} isDisabled={busy} />
      {step === 1 && <Button type="button" label="Back" variant="secondary" onClick={() => { setStep(0); setError(""); }} isDisabled={busy} />}
      <Button label={step === 1 ? "Import cluster" : "Review import"} variant="primary" type="submit" form={formId} isLoading={busy} />
    </HStack>}>
    <form id={formId} method="post" aria-label="Cluster import settings" onSubmit={(event) => { event.preventDefault(); void submit(); }}>
      <VStack gap={5}>
        {error && <VStack ref={errorRef} tabIndex={-1} role="alert"><ErrorNotice message={error} /></VStack>}
        {step === 0 && <>
          <NetworkPicker operation="cluster.import" choices={choices} value={form.networkId} disabled={busy} onChange={(next) => change("networkId", next)} />
          <Divider />
          <VStack gap={3}>
            <Heading level={4}>Resources to discover</Heading>
            <Text color="secondary">Enter the names as they appear in EKS.</Text>
            <FormLayout>
              <TextInput label="Existing cluster name" placeholder="production-eks" value={form.name} onChange={(next) => change("name", next)} isRequired isDisabled={busy} autoComplete="off" autoCapitalize="none" hasAutoCorrect={false} hasSpellCheck={false} />
              <TextInput label="Existing default node group name" placeholder="platform-workers" value={form.defaultNodeGroupName} onChange={(next) => change("defaultNodeGroupName", next)} isRequired isDisabled={busy} description="Only this group is registered as the default pool." autoComplete="off" autoCapitalize="none" hasAutoCorrect={false} hasSpellCheck={false} />
            </FormLayout>
          </VStack>
          <TextInput label="Display name" value={form.displayName} onChange={(next) => change("displayName", next)} isOptional isDisabled={busy} />
        </>}
        {step === 1 && <>
          <MetadataList columns="multi">
            <MetadataListItem label="Cluster name">{form.name}</MetadataListItem>
            {form.displayName && <MetadataListItem label="Display name">{form.displayName}</MetadataListItem>}
            <MetadataListItem label="Default node group">{form.defaultNodeGroupName}</MetadataListItem>
            <MetadataListItem label="Cloud account">{accountDescription(choices.data?.accounts.find((account) => account.id === network?.spec?.bootstrapId))}</MetadataListItem>
            <MetadataListItem label="Network">{network?.displayName || network?.name}</MetadataListItem>
            <MetadataListItem label="Region">{network?.spec?.region}</MetadataListItem>
            <MetadataListItem label="Inherited tier">{tierLabel(network?.spec?.tier)}</MetadataListItem>
          </MetadataList>
          <Banner status="info" title="Read-only discovery" description="Track discovery on the cluster detail page. Taking ownership is a separate action after discovery." />
          <Collapsible trigger="Advanced · exact import request" defaultIsOpen={false}><VStack as="pre" isScrollable><Text type="code">{review && toJsonString(ImportClusterRequestSchema, review, { prettySpaces: 2 })}</Text></VStack></Collapsible>
        </>}
      </VStack>
    </form>
  </ResourceWorkflowDialog>;
}

function ClusterDetails({ clusterId }: { clusterId: string }) {
  const { api, tenantId } = usePlatform();
  const [polling, setPolling] = useState(true);
  const [editing, setEditing] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [retrying, setRetrying] = useState(false);
  const [message, setMessage] = useState("");
  const [eventTokens, setEventTokens] = useState([""]);
  const [activityFilters, setActivityFilters] = useState<ActivityFilters>({ search: "", kind: "all", onlyWarnings: false });
  const filteringActivity = !!activityFilters.search.trim() || activityFilters.kind !== "all" || activityFilters.onlyWarnings;
  const [requestedEventKinds, setRequestedEventKinds] = useState(false);

  const load = useCallback(async (signal: AbortSignal) => {
    const result = await api.cluster.getCluster({ tenantId, clusterId }, { signal });
    if (!signal.aborted) setPolling(!!result.cluster && clusterIsConverging(result.cluster));
    return result;
  }, [api, tenantId, clusterId]);
  const detail = useAsyncQuery(load, { pollMs: polling ? 5000 : undefined });
  const cluster = detail.data?.cluster;
  const sections = clusterSections(!!cluster?.spec?.naas);
  const pages = sections.flatMap(section => section.pages);
  const navigation = useDetailSection(pages);
  const { active: activeTab, href: tabHref, open: openTab, panelId } = navigation;
  const activityOpen = activeTab === "activity" && !!cluster;
  const loadCompleteEvents = activityOpen && (filteringActivity || requestedEventKinds);
  const eventToken = eventTokens[eventTokens.length - 1];
  const loadEvents = useCallback((signal: AbortSignal) => activityOpen && !filteringActivity ? api.cluster.listClusterEvents({ tenantId, clusterId, pageSize: 25, pageToken: eventToken }, { signal }) : Promise.resolve(undefined), [api, tenantId, clusterId, eventToken, activityOpen, filteringActivity]);
  const events = useAsyncQuery(loadEvents, { pollMs: activityOpen && !filteringActivity && polling ? 5000 : undefined });
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
  const refresh = detail.refresh;
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
  const addons = desiredAws || aws ? awsAddonInventory(desiredAws?.addons, aws?.addons) : [];
  const pageEvents = events.data?.events.length ? events.data.events : eventToken ? [] : status?.recentEvents ?? [];
  const activityEvents = filteringActivity ? allEvents.data ?? [] : pageEvents;
  const filteredEvents = filterActivityEvents(activityEvents, activityFilters);
  const availableEventKinds = [...pageEvents, ...(allEvents.data ?? [])];
  const pause = cluster && clusterPause(cluster);
  const systemPool = status?.nodeGroups.find(group => group.platformManaged && group.name === "system");
  const failedComponents = status?.platformComponents.filter(component => component.phase === ComponentPhase.FAILED) ?? [];
  const rollout = status?.rollout;
  const failedUpgrade = rollout?.result === ClusterRolloutResult.FAILED || rollout?.result === ClusterRolloutResult.TIMED_OUT;
  return <VStack gap={6} as="section" aria-label="Cluster details">
    {!cluster && <DetailHeader title={detail.loading ? "Loading cluster" : "Cluster details"} />}
    <ErrorNotice message={detail.error} />
    {detail.loading && <Text role="status">Loading cluster…</Text>}
    {cluster && <>
      <DetailHeader
        title={cluster.displayName || cluster.name}
        description={`${cluster.spec?.provider.case?.toUpperCase() || "Provider pending"} · ${cluster.spec?.region || "Region pending"}`}
        metadata={<HStack gap={4} wrap="wrap" vAlign="center"><ProviderIcon provider={cluster.spec?.provider.case ?? ""} size="sm" /><CopyValue value={cluster.name} label="cluster name" isSecondary /><CopyValue value={cluster.id} label="cluster ID" isSecondary /></HStack>}
        status={<>{statusValue(ClusterState, status?.state)}{pause && pause.key !== "not-paused" && <StatusValue label={[pause.label, status?.freeze?.scope].filter(Boolean).join(" · ")} tone={pause.tone} />}</>}
        actions={<HStack gap={2} wrap="wrap" vAlign="center">
          <ClusterAccessButton cluster={cluster} />
          {<Button label="Edit cluster" variant="secondary" onClick={() => { setEditing(true); setMessage(""); }} isDisabled={status?.state === ClusterState.DELETING} />}
          {<ClusterActions cluster={cluster} onDelete={() => setDeleting(true)}
            onAccepted={() => { setMessage("Takeover request accepted. Ownership and convergence continue asynchronously."); refresh(); }} />}
        </HStack>}
      />
      {message && <VStack role="status"><Banner status="info" title="Request recorded" description={message} /></VStack>}
      <DetailNavigation resource="Cluster" pages={pages} groups={sections} selection={navigation} />
      {activeTab === "overview" && <VStack id={panelId("overview")} role="tabpanel" aria-label="Overview" tabIndex={0} gap={6}>
        <HStack hAlign="between" vAlign="center"><Heading level={2}>Cluster summary</Heading><RefreshButton label="Refresh overview" onClick={() => { refresh(); network.refresh(); }} isLoading={detail.refreshing || network.refreshing} /></HStack>
        {pause && pause.key !== "not-paused" && <Banner status={pause.tone === "warning" ? "warning" : "info"} title={pause.label} description={pause.detail} />}
        <DetailSummary items={[
          { label: "Kubernetes", value: <HStack gap={2} vAlign="center"><KubernetesIcon /><Text type="large" weight="semibold" hasCapsize>{status?.currentKubernetesVersion || "Resolving"}</Text></HStack>, note: status?.kubernetes?.available ? `${status.kubernetes.available} available` : "Observed control plane" },
          { label: "Resources ready", value: resources.length ? `${ready} / ${resources.length}` : "Not reported", note: resources.some((resource) => resource.paused) ? `${resources.filter((resource) => resource.paused).length} paused` : "Managed cloud inventory" },
          { label: "Default pool limits", value: pool?.scalingConfig ? `${pool.scalingConfig.minSize}–${pool.scalingConfig.maxSize}` : "Pending", note: [systemPool && `${systemPool.nodes} observed`, pool?.instanceTypes.join(", ")].filter(Boolean).join(" · ") || "Configured minimum–maximum" },
          { label: "Release channel", value: channelSlug ? <Link href={`/channels/${channelSlug}`}>{channel}</Link> : channel, note: cluster.spec?.upgradeMode === UpgradeMode.AUTOMATIC ? "Automatic upgrades" : "Manual upgrades" },
        ]} />
        {status?.message && <Text color="secondary">{status.message}</Text>}
        <ClusterWorkloadSummary status={status} />
        <Grid columns={{ minWidth: 300, max: 2 }} gap={8}>
          <VStack gap={4}>
            <Heading level={3}>Deployment</Heading>
            <MetadataList>
              <MetadataListItem label="Network">{networkId ? <CopyReference href={`/networks/${encodeURIComponent(networkId)}`} value={networkId} display={network.data?.network?.displayName || network.data?.network?.name} label="network ID" /> : "Not reported"}</MetadataListItem>
              <MetadataListItem label="Cloud account">{network.data?.network?.spec?.bootstrapId ? <CopyReference href={`/accounts/${encodeURIComponent(network.data.network.spec.bootstrapId)}`} value={network.data.network.spec.bootstrapId} label="cloud account ID" /> : "Inherited from network"}</MetadataListItem>
              <MetadataListItem label="Rollout tier">{tierLabel(status?.tier)}</MetadataListItem>
              <MetadataListItem label="Generation">{String(status?.observedGeneration ?? 0)} observed / {String(cluster.generation)} desired</MetadataListItem>
              <MetadataListItem label="Platform release">{status?.platformRelease ? <Link href={`/releases/${encodeURIComponent(status.platformRelease)}`}>{status.platformRelease}</Link> : "Not reported"}</MetadataListItem>
              <MetadataListItem label="Namespace as a Service (NaaS)"><VStack gap={1}><Text>{cluster.spec?.naas ? "Enabled" : "Disabled"}</Text>{cluster.spec?.naas && <Link href={tabHref("subtenants")}>View Sub Tenants</Link>}</VStack></MetadataListItem>
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
            {(failedComponents.length > 0 || failedUpgrade) && <VStack gap={2}>
              <Banner status="error" title="Failed work" description={[failedUpgrade && (rollout?.message || "Automatic upgrade failed"), ...failedComponents.map(component => component.name)].filter(Boolean).join(" · ")} />
              {<HStack><Button label="Retry failed work" variant="secondary" onClick={() => setRetrying(true)} /></HStack>}
            </VStack>}
          </VStack>
        </Grid>
        <Divider />
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
        <VStack gap={4}>
          <HStack hAlign="between" gap={3} wrap="wrap"><Heading level={3}>Related resources</Heading><Link href={tabHref("resources")}>View infrastructure inventory</Link></HStack>
          <DetailRows title="Related cluster resources" rows={[
            ...(status?.nodeGroups ?? []).filter(group => !(group.platformManaged && group.name === "platform")).map(group => ({
              name: group.name, copyName: group.name, state: statusValue(NodeGroupPhase, group.phase),
              detail: group.message || `${group.nodes} observed nodes`,
              href: `/clusters/${encodeURIComponent(clusterId)}/node-groups/${encodeURIComponent(group.name)}`,
            })),
            ...(!status?.nodeGroups.length ? nodeGroups.map(resource => ({
              name: resource.name, copyName: resource.name, state: statusValue(ManagedResourceState, resource.state),
              detail: resource.externalName ? <CopyValue value={resource.externalName} label="node group cloud ID" /> : resource.message || "Node group",
              href: tabHref("node-groups"),
            })) : []),
            ...(aws?.addons ?? []).map((addon) => { const display = addons.find((item) => item.name === addon.name)?.status; return { name: addon.name, state: display ? <StatusValue label={display.label} tone={display.tone} /> : statusValue(ManagedResourceState, addon.state), detail: `Add-on · ${addon.version}`, href: tabHref("components") }; }),
          ]} />
        </VStack>
      </VStack>}
      {activeTab === "configuration" && <VStack id={panelId("configuration")} role="tabpanel" aria-label="Configuration" tabIndex={0} gap={6}><ClusterConfiguration cluster={cluster} actions={<HStack gap={2} vAlign="center"><ResourceYamlButton kind="Cluster" resourceId={clusterId} /><RefreshButton label="Refresh configuration" onClick={refresh} isLoading={detail.refreshing} /></HStack>} /></VStack>}
      {activeTab === "node-groups" && <VStack id={panelId("node-groups")} role="tabpanel" aria-label="Node groups" tabIndex={0} gap={6}><ClusterNodeGroups cluster={cluster} refreshing={detail.refreshing} onRefresh={detail.refresh} onApplied={(note) => { setMessage(note); refresh(); }} /></VStack>}
      {activeTab === "topology" && <VStack id={panelId("topology")} role="tabpanel" aria-label="Topology" tabIndex={0} gap={6}><ClusterTopology cluster={cluster} network={network.data?.network} /></VStack>}
      {activeTab === "resources" && <VStack id={panelId("resources")} role="tabpanel" aria-label="Infrastructure inventory" tabIndex={0} gap={6}>
        <ManagedInventory resources={resources} title="Cluster managed resource inventory" onRefresh={refresh} refreshing={detail.refreshing} />
        {status?.import && <Collapsible trigger={`Import discovery · ${label(ImportPhase, status.import.phase)}`} defaultIsOpen={false}>
          <DetailRows title="Cluster import resource inventory" rows={status.import.resources.map((resource) => ({ name: `${resource.kind} / ${resource.name}`, copyName: resource.name, state: statusValue(Disposition, resource.disposition), detail: <VStack gap={1}>{resource.externalName && <CopyValue value={resource.externalName} label="imported resource cloud ID" />}<Text>{resource.apiVersion} · {resource.detail}{resource.requiresReplacement ? " · Requires replacement" : ""}</Text></VStack> }))} />
        </Collapsible>}
        {!!status?.carriedOver.length && <Collapsible trigger="Carried-over resources" defaultIsOpen={false}><DetailRows title="Carried-over cluster resources" rows={status.carriedOver.map((resource) => ({ name: `${resource.kind} / ${resource.name}`, copyName: resource.name, state: statusValue(Disposition, resource.disposition), detail: <VStack gap={1}>{resource.externalName && <CopyValue value={resource.externalName} label="carried-over resource cloud ID" />}<Text>{resource.detail}</Text></VStack> }))} /></Collapsible>}
      </VStack>}
      {activeTab === "activity" && <VStack id={panelId("activity")} role="tabpanel" aria-label="Activity" tabIndex={0} gap={5}>
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
      {activeTab === "components" && <VStack id={panelId("components")} role="tabpanel" aria-label="Components &amp; add-ons" tabIndex={0} gap={6}>
        <ClusterComponentsPanel clusterId={clusterId} clusterRefreshing={detail.refreshing} onRefreshCluster={refresh} onRetry={() => setRetrying(true)}>
          <VStack gap={3}>
            <Heading level={3}>EKS add-ons</Heading>
            <Text type="supporting">Target is the resolved desired version; running is the last observed version. AWS resolves add-ons for the Kubernetes minor unless an explicit version was requested. Release channels do not pin add-on versions.</Text>
            {addons.length ? <Table aria-label="Cluster EKS add-on target and running versions" density="compact" hasHover textOverflow="wrap" idKey="name" data={addons} columns={[
              { key: "name", header: "Add-on", width: proportional(2, { minWidth: 80 }) },
              { key: "target", header: "Target (resolved)", width: proportional(1, { minWidth: 80 }) },
              { key: "version", header: "Running", width: proportional(1, { minWidth: 80 }), renderCell: (row) => <Text>{row.running?.version || (row.status.key === "disabled" ? "—" : "Not observed")}</Text> },
              { key: "state", header: "State", width: proportional(1, { minWidth: 80 }), renderCell: (row) => <StatusValue label={row.status.label} tone={row.status.tone} /> },
              { key: "message", header: "Message", width: proportional(2, { minWidth: 80 }), renderCell: (row) => <Text type="supporting">{row.message}</Text> },
            ]} /> : <Text type="supporting">No EKS add-on targets or running versions reported.</Text>}
          </VStack>
          <VStack gap={3}>
            <HStack gap={3} wrap="wrap" hAlign="between" vAlign="center"><Heading level={3}>Platform components</Heading>{status?.platformRelease && <Link href={`/releases/${encodeURIComponent(status.platformRelease)}`}>Release {status.platformRelease}</Link>}</HStack>
            <Text type="supporting">The platform release pins chart versions. Installed versions show the last observed ready version and can lag during rollout.</Text>
            <PlatformComponentInventory components={status?.platformComponents ?? []} naasEnabled={cluster.spec?.naas ?? false} />
          </VStack>
        </ClusterComponentsPanel>
      </VStack>}
      {activeTab === "violations" && <VStack id={panelId("violations")} role="tabpanel" aria-label="Policy violations" tabIndex={0} gap={8}>
        <ClusterViolationsPanel clusterId={clusterId} />
      </VStack>}
      {activeTab === "deprecations" && <VStack id={panelId("deprecations")} role="tabpanel" aria-label="Deprecated APIs" tabIndex={0} gap={6}><ClusterDeprecationsPanel clusterId={clusterId} /></VStack>}
      {activeTab === "cloud-resources" && <VStack id={panelId("cloud-resources")} role="tabpanel" aria-label="Platform cloud resources" tabIndex={0} gap={4}>
        <ManagedResourcePreviews key={cluster.id} ownership="platform" clusterId={cluster.id}
          owner={cluster.displayName || cluster.name || cluster.id} region={cluster.spec?.region ?? ""}
          provider={cluster.spec?.provider.case || cluster.status?.provider.case} deleting={cluster.status?.state === ClusterState.DELETING} />
      </VStack>}
      {activeTab === "subtenants" && cluster.spec?.naas && <VStack id={panelId("subtenants")} role="tabpanel" aria-label="Sub Tenants" tabIndex={0} gap={6}><ClusterSubTenants clusterId={clusterId} /></VStack>}
      {activeTab === "recommendations" && <VStack id={panelId("recommendations")} role="tabpanel" aria-label="Recommendations" tabIndex={0} gap={6}><ClusterRecommendationsPanel clusterId={clusterId} /></VStack>}
      {editing && <EditCluster network={network.data?.network} clusterId={clusterId} currentEtag={cluster.etag} onClose={() => setEditing(false)} onApplied={() => { setEditing(false); setMessage("Update recorded. The controller is converging the desired state."); refresh(); }} />}
      {deleting && <DeleteCluster cluster={cluster} onCancel={() => setDeleting(false)} onDeleted={() => { setDeleting(false); setMessage("Deletion requested. Resources are removed asynchronously."); refresh(); }} />}
      {retrying && <RetryCluster clusterId={clusterId} onClose={() => setRetrying(false)} onRetried={refresh} />}
    </>}
  </VStack>;
}

function CopyReference({ value, href, display, label: copyLabel }: { value: string; href: string; display?: string; label: string }) {
  return <HStack gap={2} wrap="wrap" vAlign="center"><Link href={href}>{display || value}</Link><CopyButton value={value} label={copyLabel} /></HStack>;
}

function PlatformComponentInventory({ components, naasEnabled }: { components: PlatformComponent[]; naasEnabled: boolean }) {
  const columns: TableColumn<PlatformComponent>[] = [
    { key: "name", header: "Component", width: proportional(2, { minWidth: 80 }) },
    { key: "version", header: "Release-pinned version", width: proportional(1, { minWidth: 80 }), renderCell: (component) => <Text>{component.version || "No release pin reported"}</Text> },
    { key: "installedVersion", header: "Installed version", width: proportional(1, { minWidth: 80 }), renderCell: (component) => <Text>{component.installedVersion || "Not observed"}</Text> },
    { key: "phase", header: "State", width: proportional(1, { minWidth: 80 }), renderCell: (component) => { const state = platformInstallationStatus(component, component.name !== "capsule" || naasEnabled); return <StatusValue label={state.label} tone={state.tone} />; } },
    { key: "message", header: "Details", width: proportional(2, { minWidth: 80 }), renderCell: (component) => <VStack gap={1}><Text type="supporting">{component.message || component.helmState || "No message reported"}</Text>{component.lastError && <Text type="supporting">{component.lastError}</Text>}</VStack> },
  ];
  return components.length ? <Table aria-label="Cluster platform component pinned and installed versions" data={components} columns={columns} idKey="name" density="compact" hasHover textOverflow="wrap" /> : <Text type="supporting">No platform components reported.</Text>;
}

function ManagedInventory({ resources, title, onRefresh, refreshing }: { resources: ManagedResource[]; title: string; onRefresh: () => void; refreshing: boolean }) {
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
    { key: "state", header: "Status", width: pixel(152), renderCell: (resource) => statusValue(ManagedResourceState, resource.state) },
    { key: "kind", header: "Kind", width: proportional(1, { minWidth: 80 }) },
    { key: "externalName", header: "Cloud ID", width: proportional(2, { minWidth: 80 }), renderCell: (resource) => resource.externalName ? <CopyValue value={resource.externalName} label="managed resource cloud ID" /> : <Text>Not assigned</Text> },
    { key: "paused", header: "Paused", width: proportional(1, { minWidth: 80 }), renderCell: (resource) => <Text maxLines={1}>{resource.paused ? "Paused" : "Active"}</Text> },
    { key: "ready", header: "Ready", width: proportional(1, { minWidth: 80 }), renderCell: (resource) => <Text maxLines={1}>{label(ConditionStatus, resource.ready)}</Text> },
    { key: "synced", header: "Synced", width: proportional(1, { minWidth: 80 }), renderCell: (resource) => <Text maxLines={1}>{label(ConditionStatus, resource.synced)}</Text> },
    { key: "reason", header: "Reason", width: proportional(1, { minWidth: 80 }), renderCell: (resource) => <Text maxLines={1}>{resource.reason || "—"}</Text> },
    { key: "message", header: "Message", width: proportional(2, { minWidth: 80 }), renderCell: (resource) => <Text maxLines={1}>{resource.message || "No message reported"}</Text> },
  ];
  return <VStack gap={4}>
    <HStack hAlign="between" vAlign="center" gap={3} width="100%">
      <VStack gap={1}><Heading level={3}>Infrastructure inventory</Heading><Text type="supporting" role="status">{matching.length} of {resources.length} resources · {resources.filter((resource) => resource.paused).length} paused</Text></VStack>
      <RefreshButton label="Refresh infrastructure inventory" onClick={onRefresh} isLoading={refreshing} />
    </HStack>
    <SearchFilters label="Resource filters" values={[kind, state, health, paused]}
      search={<TextInput label="Search resources" placeholder="Name, kind or cloud ID" type="search" value={search} onChange={setSearch} hasClear autoComplete="off" />}>
      <Selector label="Resource kind" hasSearch value={kind} onChange={setKind} options={[{ value: "all", label: "All kinds" }, ...kinds.map((value) => ({ value, label: value || "Unspecified" }))]} />
      <Selector label="State" value={state} onChange={setState} options={[{ value: "all", label: "All states" }, ...states.map((value) => ({ value: String(value), label: label(ManagedResourceState, value) }))]} />
      <Selector label="Health" value={health} onChange={setHealth} options={[{ value: "all", label: "All health" }, { value: "ready", label: "Ready and synced" }, { value: "attention", label: "Needs attention" }]} />
      <Selector label="Paused" value={paused} onChange={setPaused} options={[{ value: "all", label: "All resources" }, { value: "paused", label: "Paused" }, { value: "active", label: "Active" }]} />
      <Button label="Clear resource filters" variant="ghost" onClick={() => { setSearch(""); setKind("all"); setState("all"); setHealth("all"); setPaused("all"); }} />
    </SearchFilters>
    {matching.length ? <Table aria-label={title} data={matching} columns={columns} idKey={(resource) => `${resource.kind}:${resource.name}`} /> : <Text>{resources.length ? "No resources match these filters." : "No managed resources reported yet."}</Text>}
  </VStack>;
}

type RelatedResourceRow = { name: string; state: ReactNode; detail: ReactNode; href?: string; copyName?: string };

function DetailRows({ title, rows }: { title: string; rows: RelatedResourceRow[] }) {
  const columns: TableColumn<RelatedResourceRow & { id: number }>[] = [
    { key: "name", header: "Name", width: proportional(2, { minWidth: 80 }), renderCell: (row) => <HStack gap={2} wrap="wrap" vAlign="center">{row.href ? <Link href={row.href}>{row.name}</Link> : <Text>{row.name}</Text>}{row.copyName && <CopyButton value={row.copyName} label="resource name" />}</HStack> },
    { key: "state", header: "Status", width: proportional(1, { minWidth: 80 }), renderCell: (row) => typeof row.state === "string" ? <Text>{row.state}</Text> : row.state },
    { key: "detail", header: "Details", width: proportional(3, { minWidth: 80 }), renderCell: (row) => typeof row.detail === "string" ? <Text>{row.detail}</Text> : row.detail },
  ];
  return rows.length ? <Table aria-label={title} data={rows.map((row, id) => ({ ...row, id }))} columns={columns} idKey="id" textOverflow="wrap" density="compact" hasHover /> : <Text type="supporting">No {title.toLowerCase()} reported.</Text>;
}

function EditCluster({ clusterId, currentEtag, network, onClose, onApplied }: { network?: Network; clusterId: string; currentEtag: string; onClose: () => void; onApplied: () => void }) {
  const { api, tenantId } = usePlatform();
  const load = useCallback((signal: AbortSignal) => api.cluster.getCluster({ tenantId, clusterId, asRequested: true }, { signal }), [api, tenantId, clusterId]);
  const requested = useAsyncQuery(load);
  if (requested.data?.cluster) return <ClusterEditor network={network} key={requested.data.cluster.etag} baseline={requested.data.cluster} asRequested={requested.data.asRequested} currentEtag={currentEtag} onApplied={onApplied} onClose={onClose} onReload={requested.refresh} />;
  return <ResourceWorkflowDialog isOpen onOpenChange={(open) => { if (!open) onClose(); }} title="Edit cluster" steps={clusterWizardSteps} step={0}
    footer={<HStack gap={3}><Button label="Cancel editing" variant="secondary" onClick={onClose} />{!requested.loading && <Button label="Reload editable cluster" onClick={requested.refresh} />}</HStack>}>
    <ErrorNotice message={requested.error} />
    {requested.loading ? <Text role="status">Loading cluster settings…</Text> : !requested.error && <Text>The server returned no cluster settings to edit.</Text>}
  </ResourceWorkflowDialog>;
}

function ClusterEditor({ baseline, network, asRequested, currentEtag, onApplied, onClose, onReload }: { baseline: Cluster; network?: Network; asRequested: boolean; currentEtag: string; onApplied: () => void; onClose: () => void; onReload: () => void }) {
  const { api, tenantId } = usePlatform();
  const loadSubTenants = useCallback(async (signal: AbortSignal) => baseline.spec?.naas ? api.subtenant.listSubTenants({ tenantId, clusterId: baseline.id, pageSize: 1 }, { signal }) : undefined, [api, tenantId, baseline.id, baseline.spec?.naas]);
  const allocations = useAsyncQuery(loadSubTenants);
  const naasLockReason = !baseline.spec?.naas ? undefined : allocations.loading ? "Checking existing Sub Tenants before allowing NaaS to be disabled…" : allocations.error ? "Sub Tenants could not be checked. Reload before disabling NaaS." : allocations.data?.subtenants.length || allocations.data?.nextPageToken ? "Delete all Sub Tenants, including those still deleting, before disabling NaaS." : undefined;
  const [form, setForm] = useState(() => clusterForm(baseline));
  const rows = useClusterFieldRows(form);
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
    if (["desiredSize", "zoneIds", "serviceCidr", "awsAdditionalSecurityGroups", "awsAuthenticationMode", ...platformManagedClusterFields].includes(field)) return;
    if (field === "naas" && value === false && naasLockReason) return;
    revision.current++;
    setReview(undefined); setError("");
    setForm((old) => ({ ...old, [field]: value }));
    setTouched((old) => new Set([...old, field]));
  }
  async function submit(apply: boolean) {
    if (locked.current) return;
    if (!apply && step < 2) {
      try { rows.validate(step); validateClusterFormStep(form, step, touched); setStep(step + 1); setError(""); }
      catch (cause) { setError(errorMessage(cause)); requestAnimationFrame(() => errorRef.current?.focus()); }
      return;
    }
    locked.current = true; setBusy(true); setError("");
    const inputRevision = revision.current;
    try {
      if (baseline.etag !== currentEtag) throw new Error("The cluster changed. Reload editable cluster and review a new plan.");
      if (baseline.spec?.naas && !form.naas && naasLockReason) throw new Error(naasLockReason);
      rows.validate(step);
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
  return <ResourceWorkflowDialog isOpen onOpenChange={(open) => { if (!open && !locked.current) onClose(); }} title="Edit cluster" steps={clusterWizardSteps} step={step} busy={busy}
    description={`Review changes to ${baseline.name} in tenant ${tenantId} before applying.`}
    placement={<NetworkPlacement network={network} cluster={baseline} />}
    footer={<HStack gap={3} wrap="wrap">
      <Button label="Cancel editing" variant="secondary" onClick={onClose} isDisabled={busy} />
      {step > 0 && <Button label="Back" variant="secondary" onClick={() => { setStep(step - 1); setError(""); }} isDisabled={busy} />}
      <Button label={step === 3 ? "Apply reviewed plan" : step === 2 ? "Preview update plan" : "Next"} type="submit" form={formId} variant="primary" isLoading={busy} isDisabled={(step === 3 && (!review || !clusterPlanApplicable(review.plan) || baseline.etag !== currentEtag))} />
    </HStack>}>
    {!asRequested && <Banner status="info" title="Original input unavailable" description="The server returned resolved defaults. Only fields you edit will be included in the update mask." />}
    {baseline.etag !== currentEtag && <Banner status="warning" title="Cluster changed" description="Reload editable cluster before previewing or applying changes." />}
    {baseline.etag !== currentEtag && <Button label="Reload editable cluster" onClick={onReload} isDisabled={busy} />}
    {error && <VStack ref={errorRef} tabIndex={-1} role="alert"><ErrorNotice message={error} /></VStack>}
    <form id={formId} method="post" aria-label="Cluster update settings" onSubmit={(event) => { event.preventDefault(); void submit(step === 3); }}>
      <VStack gap={6}>
        {step < 3 && <ClusterFields step={step} value={form} onChange={change} rows={rows} baseline={baseline} naasLockReason={naasLockReason} disabled={busy} networkName={network?.displayName || network?.name} region={baseline.spec?.region ?? ""} tier={network?.spec?.tier || baseline.status?.tier} />}
    {step === 3 && review && <VStack gap={4}>
      <PlanReview plan={review.plan} />
      <ClusterExtraReview form={form} touched={touched} />
      {touched.has("naas") && <Banner status={form.naas ? "info" : "warning"} title={form.naas ? "Enable Namespace as a Service" : "Disable Namespace as a Service"} description={form.naas ? "Enable support for team namespaces with separate access, quotas, and policies. Setup continues after you apply this change." : "Turn off namespace tenancy controls for this cluster. Existing namespaces, workloads, and data will remain. Review the impact on teams using these controls before applying."} />}
      <MetadataList><MetadataListItem label="Edited settings">{[...touched].filter((field) => !(field === "kubernetesVersion" && Number(form.upgradeMode) === UpgradeMode.AUTOMATIC)).map((field) => field.replace(/([A-Z])/g, " $1").toLowerCase()).join(", ")}</MetadataListItem><MetadataListItem label="Concurrency token">{baseline.etag}</MetadataListItem></MetadataList>
      <Collapsible trigger="Advanced · exact reviewed request" defaultIsOpen={false}><VStack as="pre" isScrollable><Text type="code">{toJsonString(UpdateClusterRequestSchema, review.request, { prettySpaces: 2 })}</Text></VStack></Collapsible>
    </VStack>}
      </VStack>
    </form>
  </ResourceWorkflowDialog>;
}

function DeleteCluster({ cluster, onCancel, onDeleted }: { cluster: Cluster; onCancel: () => void; onDeleted: () => void }) {
  const { api, tenantId } = usePlatform();
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
      if (locked.current) return;
      if (confirmation !== cluster.name) { setError(`Type ${cluster.name} exactly to confirm deletion.`); return; }
      if (!cluster.etag) { setError("Reload details before deleting: the cluster has no etag."); return; }
      locked.current = true; setBusy(true); setError("");
      void api.cluster.deleteCluster({ tenantId, clusterId: cluster.id, etag: cluster.etag, force }).then(onDeleted).catch((cause) => setError(errorMessage(cause))).finally(() => { locked.current = false; setBusy(false); });
    }}>
      <VStack gap={4}>
        <VStack role="alert"><ErrorNotice message={error} /></VStack>
        <CheckboxInput label="Force deletion" value={force} onChange={(next) => { setForce(next); setConfirmation(""); setError(""); }} isDisabled={busy} description="For an unreachable Kubernetes API: skip Helm uninstall. Chart-created load balancers or volumes may remain in the cloud. Force is sticky once deletion starts." />
        <TextInput label={`Type ${cluster.name} to confirm`} value={confirmation} onChange={(next) => { setConfirmation(next); setError(""); }} isDisabled={busy} autoComplete="off" />
        <HStack gap={3} wrap="wrap"><Button label="Cancel deletion" variant="secondary" onClick={onCancel} isDisabled={busy} /><Button label={force ? "Confirm forced deletion" : "Confirm normal deletion"} variant="destructive" type="submit" isLoading={busy} /></HStack>
      </VStack>
    </form>
    </VStack>
  </Dialog>;
}

function RetryCluster({ clusterId, onRetried, onClose }: { clusterId: string; onRetried: () => void; onClose: () => void }) {
  const { api, tenantId } = usePlatform();
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
      if (locked.current) return;
      locked.current = true; setBusy(true); setError(""); setResult([]);
      void api.cluster.retryCluster({ tenantId, clusterId, components: names.split(/[\s,]+/).filter(Boolean), upgrade })
        .then((response) => { setResult(response.items.map((item) => `${item.name}: ${label(RetryAction, item.action)} · ${item.message}`)); onRetried(); })
        .catch((cause) => setError(errorMessage(cause))).finally(() => { locked.current = false; setBusy(false); });
    }}>
      <VStack gap={4}>
        <VStack role="alert"><ErrorNotice message={error} /></VStack>
        <TextInput label="Components to retry" description="Optional comma-separated names. Empty with upgrade unchecked retries all failed work." value={names} onChange={setNames} isDisabled={busy} />
        <CheckboxInput label="Retry failed automatic upgrade" value={upgrade} onChange={setUpgrade} isDisabled={busy} />
        <Button label="Request retry" type="submit" isLoading={busy} />
        <Button label="Close retry" variant="secondary" onClick={onClose} isDisabled={busy} />
        <VStack role="status">{result.map((item, index) => <Text key={index}>{item}</Text>)}</VStack>
      </VStack>
    </form>
    </VStack>
  </Dialog>;
}
