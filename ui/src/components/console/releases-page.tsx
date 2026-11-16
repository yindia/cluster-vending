"use client";

import { useCallback, useId, useState, type ReactNode } from "react";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack } from "@astryxdesign/core/HStack";
import { Link } from "@astryxdesign/core/Link";
import { Pagination } from "@astryxdesign/core/Pagination";
import { Selector } from "@astryxdesign/core/Selector";
import { Tab, TabList } from "@astryxdesign/core/TabList";
import { proportional } from "@astryxdesign/core/Table";
import { ConsoleTable as Table } from "./console-table";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { VStack } from "@astryxdesign/core/VStack";
import { stringify } from "yaml";
import { usePlatform } from "@/components/platform-provider";
import { RefreshButton } from "./refresh-button";
import { AwsIcon } from "./provider-icons";
import { useAsyncQuery } from "@/lib/api/use-query";
import { collectPages } from "@/lib/api/pagination";
import { ClusterTier, ReleaseChannel } from "@/gen/cloud/v1/cluster_pb";
import { RolloutState, type GetReleaseChannelResponse, type ReleaseChannelInfo, type ReleaseMinor } from "@/gen/cloud/v1/admin_release_pb";
import { CopyButton, CopyValue } from "./copy-value";
import { DetailHeader, DetailSummary, StatusValue } from "./detail-layout";
import { ValuesEditor } from "./values-editor";
import { ChannelClusters } from "./channel-clusters";

type ChannelSlug = "alpha" | "beta" | "stable";
const channelIds: Record<ChannelSlug, ReleaseChannel> = { alpha: ReleaseChannel.ALPHA, beta: ReleaseChannel.BETA, stable: ReleaseChannel.STABLE };
function label(value: string | undefined, number: number) {
  return value ? value.toLowerCase().replaceAll("_", " ").replace(/^./, letter => letter.toUpperCase()) : `Unknown (${number})`;
}
function channelLabel(value: ReleaseChannel) { return label(ReleaseChannel[value], value); }
function channelSlug(value: ReleaseChannel): ChannelSlug | undefined {
  return value === ReleaseChannel.ALPHA ? "alpha" : value === ReleaseChannel.BETA ? "beta" : value === ReleaseChannel.STABLE ? "stable" : undefined;
}
function currentChannels(name: string, channels: readonly ReleaseChannelInfo[] | undefined) {
  return [...new Set(channels?.filter((channel) => channel.release === name).map((channel) => channel.channel) ?? [])];
}
function offeringState(release: { name: string; withdrawn: boolean; valid: boolean }, channels: readonly ReleaseChannelInfo[] | undefined) {
  if (release.withdrawn) return "withdrawn";
  if (currentChannels(release.name, channels).length) return "active";
  if (!release.valid) return "invalid";
  return channels ? "available" : "unknown";
}
function OfferingStatus({ state }: { state: string }) {
  return <StatusValue label={state === "active" ? "Active · Current offering" : state === "withdrawn" ? "Withdrawn" : state === "invalid" ? "Invalid configuration" : state === "available" ? "Available" : "Offering unknown"} tone={state === "active" ? "accent" : state === "withdrawn" ? "warning" : state === "invalid" ? "error" : "neutral"} />;
}
function time(value?: { seconds: bigint }) { return value ? new Date(Number(value.seconds) * 1000).toISOString() : "Not reported"; }
function ReleaseLink({ name }: { name: string }) {
  return name ? <HStack gap={1} vAlign="center"><Link href={`/releases/${encodeURIComponent(name)}`}>{name}</Link><CopyButton value={name} label="release name" /></HStack> : <Text>Not reported</Text>;
}
function ChannelLink({ channel }: { channel: ReleaseChannel }) {
  const slug = channelSlug(channel);
  return slug ? <Link href={`/channels/${slug}`}>{channelLabel(channel)}</Link> : <Text>{channelLabel(channel)}</Text>;
}

export function ReleasesPage({ resourceId, channelId }: { resourceId?: string; channelId?: ChannelSlug } = {}) {
  const { scopeKey } = usePlatform();
  if (channelId) return <ChannelDetail key={`${scopeKey}:channel:${channelId}`} channel={channelIds[channelId]} />;
  if (resourceId) return <ReleaseDetail key={`${scopeKey}:release:${resourceId}`} name={resourceId} />;
  return <ReleaseBrowser key={scopeKey} />;
}

function DetailTabs({ label: name, sections }: { label: string; sections: readonly { id: string; label: string; content: ReactNode }[] }) {
  const [selected, setSelected] = useState(sections[0].id);
  const id = useId();
  const active = sections.some(section => section.id === selected) ? selected : sections[0].id;
  return <VStack gap={6}>
    <TabList role="tablist" aria-label={name} value={active} onChange={setSelected} hasDivider>
      {sections.map(section => <Tab key={section.id} id={`${id}-${section.id}-tab`} value={section.id} label={section.label} panelId={`${id}-${section.id}-panel`} />)}
    </TabList>
    {sections.map(section => section.id === active
      ? <VStack key={section.id} id={`${id}-${section.id}-panel`} role="tabpanel" aria-labelledby={`${id}-${section.id}-tab`} tabIndex={0} gap={5}>{section.content}</VStack>
      : null)}
  </VStack>;
}

function ReleaseBrowser() {
  const { api, tenantId } = usePlatform();
  const loadChannels = useCallback((signal: AbortSignal) => api.tenant.listReleaseChannels({ tenantId }, { signal }), [api, tenantId]);
  const channels = useAsyncQuery(loadChannels);
  return <VStack gap={6}>
    <DetailHeader title="Releases" description="Published releases and channel offerings. Open a release or channel to inspect its versions and rollout details." />
    <HStack gap={3} hAlign="between" vAlign="center" wrap="wrap"><Heading level={2}>Release channels</Heading><RefreshButton label="Refresh channels" isLoading={channels.refreshing} onClick={channels.refresh} /></HStack>
    {channels.error && <Banner status="error" title="Channels could not be loaded" description={channels.error} />}
    {channels.loading && <Text role="status">Loading channels…</Text>}
    {channels.data && <Table searchable searchText={({ item }) => [channelLabel(item.channel), item.release, item.kubernetesMinor, item.promotion, item.paused ? "Paused" : "Not paused"].join(" ")} aria-label="Release channels" idKey="id" textOverflow="wrap" data={channels.data.channels.map(item => ({ id: item.channel, item }))} columns={[
      { key: "channel", header: "Channel", width: proportional(1, { minWidth: 100 }), renderCell: ({ item }) => <ChannelLink channel={item.channel} /> },
      { key: "release", header: "Current offering", width: proportional(1, { minWidth: 160 }), renderCell: ({ item }) => <ReleaseLink name={item.release} /> },
      { key: "minor", header: "Kubernetes", width: proportional(1, { minWidth: 140 }), renderCell: ({ item }) => <Text>{item.kubernetesMinor || "Not reported"}</Text> },
      { key: "promotion", header: "Promotion", width: proportional(1, { minWidth: 140 }), renderCell: ({ item }) => <Text>{item.promotion || "Not reported"}</Text> },
      { key: "state", header: "Rollout", width: proportional(1, { minWidth: 160 }), renderCell: ({ item }) => <StatusValue label={item.paused ? "Paused" : "Not paused"} tone={item.paused ? "warning" : "neutral"} /> },
    ]} />}
    {channels.data?.channels.length === 0 && <Text>No release channels returned.</Text>}
    <ReleaseInventory channels={channels.data?.channels} channelsError={channels.error} />
  </VStack>;
}

function ReleaseInventory({ channels, channelsError }: { channels?: readonly ReleaseChannelInfo[]; channelsError: string }) {
  const { api, tenantId } = usePlatform();
  const [search, setSearch] = useState("");
  const [channel, setChannel] = useState("all");
  const [state, setState] = useState("all");
  const [origin, setOrigin] = useState("all");
  const [page, setPage] = useState(1);
  const load = useCallback((signal: AbortSignal) => collectPages(async (pageToken) => {
    const response = await api.tenant.listReleases({ tenantId, pageSize: 200, pageToken }, { signal });
    return { items: response.releases, nextPageToken: response.nextPageToken };
  }, signal), [api, tenantId]);
  const query = useAsyncQuery(load);
  const releases = query.data ?? [];
  const terms = search.trim().toLowerCase().split(/\s+/).filter(Boolean);
  const filtered = releases.filter((item) => {
    const itemState = offeringState(item, channels);
    const references = currentChannels(item.name, channels);
    const text = [item.name, item.invalidReason, item.holds.join(" "), references.map(channelLabel).join(" "), itemState, item.valid ? "Valid" : "Invalid", item.shipped ? "Controller build" : "Operator published"].join(" ").toLowerCase();
    return terms.every((term) => text.includes(term)) &&
      (channel === "all" || !!channels && (channel === "none" && !references.length || references.some((value) => String(value) === channel))) &&
      (state === "all" || state === itemState) && (origin === "all" || item.shipped === (origin === "shipped"));
  });
  const currentPage = Math.min(Math.max(1, page), Math.max(1, Math.ceil(filtered.length / 25)));
  const visible = filtered.slice((currentPage - 1) * 25, currentPage * 25);
  const hasFilters = !!search || channel !== "all" || state !== "all" || origin !== "all";
  return <VStack gap={4}>
    <HStack gap={3} hAlign="between" vAlign="center" wrap="wrap"><Heading level={2}>Release inventory</Heading><RefreshButton label="Refresh releases" isLoading={query.refreshing} onClick={query.refresh} /></HStack>
    {query.error && <Banner status="error" title="Releases could not be loaded" description={query.error} />}
    {query.loading && <Text role="status">Loading the complete release inventory…</Text>}
    <HStack gap={3} wrap="wrap" vAlign="end">
      <TextInput label="Search releases" type="search" placeholder="Release, channel, origin or hold" value={search} onChange={(value) => { setSearch(value); setPage(1); }} autoComplete="off" hasClear width="min(100%, 24rem)" />
      <Selector label="Release channel" value={channel} onChange={(value) => { setChannel(value); setPage(1); }} options={[{ value: "all", label: "All channels" }, { value: "none", label: "No channel" }, ...[ReleaseChannel.ALPHA, ReleaseChannel.BETA, ReleaseChannel.STABLE].map((value) => ({ value: String(value), label: channelLabel(value) }))]} />
      <Selector label="Release state" value={state} onChange={(value) => { setState(value); setPage(1); }} options={[{ value: "all", label: "All states" }, { value: "active", label: "Active · Current offering" }, { value: "available", label: "Available" }, { value: "invalid", label: "Invalid configuration" }, { value: "withdrawn", label: "Withdrawn" }, { value: "unknown", label: "Offering unknown" }]} />
      <Selector label="Release origin" value={origin} onChange={(value) => { setOrigin(value); setPage(1); }} options={[{ value: "all", label: "All origins" }, { value: "shipped", label: "Controller build" }, { value: "published", label: "Operator published" }]} />
      {hasFilters && <Button label="Clear release filters" variant="ghost" onClick={() => { setSearch(""); setChannel("all"); setState("all"); setOrigin("all"); setPage(1); }} />}
    </HStack>
    {query.data && <Text role="status" color="secondary">{filtered.length} of {releases.length} releases{query.error ? " · Last successful load; refresh failed" : ""}</Text>}
    <Text color="secondary">Active marks the current offering reported for a channel. Other valid releases remain available; validation alone does not make a release current.{channelsError && channels ? " Channel references reflect the last successful load." : !channels ? " Current offerings are unavailable until channels load." : ""}</Text>
    {query.data && <Table aria-label="Releases" idKey="id" rowIndexStart={(currentPage - 1) * 25 + 1} rowCount={filtered.length} data={visible.map(item => ({ id: item.name, item }))} columns={[
      { key: "name", header: "Release", width: proportional(2, { minWidth: 80 }), renderCell: ({ item }) => <ReleaseLink name={item.name} /> },
      { key: "state", header: "State", width: proportional(2, { minWidth: 80 }), renderCell: ({ item }) => <OfferingStatus state={offeringState(item, channels)} /> },
      { key: "channels", header: "Current channels", width: proportional(2, { minWidth: 80 }), renderCell: ({ item }) => <HStack gap={3} wrap="wrap">{currentChannels(item.name, channels).map(channel => <ChannelLink key={channel} channel={channel} />)}{!currentChannels(item.name, channels).length && <Text>{channels ? "None" : "Not reported"}</Text>}</HStack> },
      { key: "origin", header: "Origin", width: proportional(2, { minWidth: 80 }), renderCell: ({ item }) => <Text>{item.shipped ? "Controller build" : "Operator published"}</Text> },
      { key: "holds", header: "Release holds", width: proportional(1, { minWidth: 80 }), renderCell: ({ item }) => <Text>{item.holds.length}</Text> },
    ]} />}
    {query.data && !query.error && !filtered.length && <Text>{releases.length ? "No releases match the current filters." : "No releases returned."}</Text>}
    {filtered.length > 25 && <Pagination label="Release pagination" page={currentPage} onChange={setPage} totalItems={filtered.length} pageSize={25} variant="count" />}
  </VStack>;
}

function MinorTable({ minors }: { minors: readonly ReleaseMinor[] }) {
  return <Table searchable searchText={({ item }) => [item.minor, item.default ? "Default without a channel" : "", item.channel === ReleaseChannel.UNSPECIFIED ? "No channel" : channelLabel(item.channel), item.upgradeTo, time(item.endOfSupport), item.ending ? "Ending soon" : ""].join(" ")} aria-label="Supported Kubernetes minors" idKey="id" textOverflow="wrap" data={minors.map((item, id) => ({ id, item }))} columns={[
    { key: "minor", header: "Kubernetes minor", renderCell: ({ item }) => <Text>{item.minor}{item.default ? " · Default without a channel" : ""}</Text> },
    { key: "channel", header: "Channel", renderCell: ({ item }) => item.channel === ReleaseChannel.UNSPECIFIED ? <Text>No channel</Text> : <ChannelLink channel={item.channel} /> },
    { key: "upgrade", header: "Next automatic upgrade", renderCell: ({ item }) => <Text>{item.upgradeTo || "No target"}</Text> },
    { key: "support", header: "Support ends (UTC)", renderCell: ({ item }) => <Text>{time(item.endOfSupport)}{item.ending ? " · Ending soon" : ""}</Text> },
    { key: "components", header: "Component pins", renderCell: ({ item }) => <Text>{item.components.length}</Text> },
  ]} />;
}
function AddonVersionSource() {
  return <VStack gap={2}><HStack gap={2} vAlign="center"><AwsIcon /><Heading level={3}>AWS EKS add-on versions</Heading></HStack><Text>EKS add-ons are not release component pins. AWS resolves compatible add-on versions against the cluster’s Kubernetes version; explicit cluster add-on version pins take precedence. This release API does not report those resolved versions.</Text></VStack>;
}
function ComponentPins({ minors, showAddonInfo = true }: { minors: readonly ReleaseMinor[]; showAddonInfo?: boolean }) {
  const [selected, setSelected] = useState("");
  const rows = minors.flatMap(minor => minor.components.map(item => ({ id: `${minor.minor}:${item.name}`, minor: minor.minor, item })));
  const selectedRow = rows.find(row => row.id === selected);
  return <VStack gap={5}>
    <VStack gap={2}><Heading level={2}>Platform component pins</Heading><Text>Each version belongs to one Kubernetes minor. Release Helm values are locked: tenants cannot override their paths.</Text></VStack>
    {rows.length ? <Table searchable searchText={(row) => [row.minor, row.item.name, row.item.version, row.item.values && Object.keys(row.item.values).length ? "Inspect locked values" : "No values set"].join(" ")} aria-label="Release component pins by Kubernetes minor" idKey="id" textOverflow="wrap" data={rows} columns={[
      { key: "minor", header: "Kubernetes", renderCell: row => <Text>{row.minor}</Text> },
      { key: "name", header: "Component", renderCell: ({ item }) => <CopyValue value={item.name} label="component name" /> },
      { key: "version", header: "Pinned version", renderCell: ({ item }) => item.version ? <CopyValue value={item.version} label="pinned version" /> : <Text>Not reported</Text> },
      { key: "values", header: "Locked Helm values", renderCell: row => <Button label={row.item.values && Object.keys(row.item.values).length ? "Inspect locked values" : "Inspect values (none set)"} onClick={() => setSelected(row.id)} /> },
    ]} /> : <Text>No platform component pins returned for these Kubernetes minors.</Text>}
    {selectedRow && <VStack gap={3}><Heading level={3}>{selectedRow.item.name} · Kubernetes {selectedRow.minor}</Heading><ValuesEditor label={`Locked Helm values for ${selectedRow.item.name} on Kubernetes ${selectedRow.minor}`} description="Read-only release values, merged over the definition’s platform values. Placeholders may be resolved by the platform." language="yaml" readOnly value={stringify(selectedRow.item.values ?? {})} height="18rem" /></VStack>}
    {showAddonInfo && <AddonVersionSource />}
  </VStack>;
}

function ReleaseDetail({ name }: { name: string }) {
  const { api, tenantId } = usePlatform();
  const load = useCallback((signal: AbortSignal) => api.tenant.getRelease({ tenantId, name }, { signal }), [api, tenantId, name]);
  const query = useAsyncQuery(load);
  const release = query.data?.release;
  const loadChannels = useCallback((signal: AbortSignal) => api.tenant.listReleaseChannels({ tenantId }, { signal }), [api, tenantId]);
  const channels = useAsyncQuery(loadChannels);
  const references = currentChannels(release?.name ?? name, channels.data?.channels);
  return <VStack gap={6}>
    <DetailHeader title={release?.name || name} description="Kubernetes support and platform component versions." metadata={<CopyValue value={release?.name || name} label="release name" isSecondary />} status={release ? <OfferingStatus state={offeringState(release, channels.data?.channels)} /> : <Text>Loading release</Text>} actions={<RefreshButton label="Refresh release details" isLoading={query.refreshing || channels.refreshing} onClick={() => { query.refresh(); channels.refresh(); }} />} />
    {query.error && <Banner status="error" title="Release details unavailable" description={query.error} />}
    {query.loading && <Text role="status">Loading release details…</Text>}
    {channels.error && <Banner status="warning" title="Current channel references unavailable" description={`${channels.error}${channels.data ? " References reflect the last successful load." : " Current offering status cannot be confirmed."}`} />}
    {query.data && !release && <Text>No release details returned.</Text>}
    {release && <>
      <DetailSummary items={[
        { label: "Kubernetes minors", value: release.kubernetes.length, note: "Supported by this release" },
        { label: "Offering channels", value: channels.data ? references.length : "Not reported", note: channels.data ? references.map(channelLabel).join(", ") || "None" : "Current channel offerings have not loaded" },
        { label: "Configuration validation", value: release.valid ? "Valid" : "Invalid", note: release.valid ? "Configuration passed validation; offering state is separate" : release.invalidReason || "No reason reported" },
        { label: "Release holds", value: release.holds.length, note: "References that retain this release" },
        { label: "Published", value: time(release.createTime), note: release.shipped ? "Shipped with the controller" : "Operator published" },
      ]} />
      {!release.valid && <Banner status="error" title="Release validation failed" description={release.invalidReason || "The server reported this release as invalid."} />}
      <DetailTabs label="Release detail sections" sections={[
        { id: "overview", label: "Overview", content: <><VStack gap={2}><Heading level={2}>Release notes</Heading><Text>{release.notes || "No release notes provided."}</Text></VStack><MinorTable minors={release.kubernetes} /><VStack gap={3}><Heading level={2}>Current references</Heading><HStack gap={3} wrap="wrap">{references.map(channel => <ChannelLink key={channel} channel={channel} />)}</HStack>{!channels.data && <Text>Current channel references have not loaded.</Text>}{release.holds.map((hold, index) => <CopyValue key={index} value={hold} label="release hold" />)}{channels.data && !references.length && !release.holds.length && <Text>No channel or hold references returned.</Text>}</VStack><AddonVersionSource /></> },
        { id: "components", label: "Components", content: <ComponentPins minors={release.kubernetes} /> },
      ]} />
    </>}
  </VStack>;
}

function OfferedComponents({ info }: { info: ReleaseChannelInfo }) {
  const { api, tenantId } = usePlatform();
  const load = useCallback((signal: AbortSignal) => api.tenant.getRelease({ tenantId, name: info.release }, { signal }), [api, tenantId, info.release]);
  const query = useAsyncQuery(load);
  const release = query.data?.release;
  const matches = release?.kubernetes.filter(minor => minor.minor === info.kubernetesMinor) ?? [];
  const matching = release?.name === info.release && matches.length === 1;
  return <VStack gap={5}>
    <Text>Current offering: {info.release} · Kubernetes {info.kubernetesMinor}. Pending releases are not used for this table.</Text>
    <HStack><RefreshButton label="Refresh offered component pins" isLoading={query.refreshing} onClick={query.refresh} /></HStack>
    {query.loading && <Text role="status">Loading the offered release…</Text>}
    {query.error && <Banner status="error" title="Offered release unavailable" description={query.error} />}
    {query.data && !matching && <Banner status="warning" title="Offering does not match the release" description="The exact offered release and Kubernetes minor were not returned. Versions cannot be inferred from a default or another minor." />}
    {matching && <ComponentPins minors={matches} showAddonInfo={false} />}
  </VStack>;
}

function ChannelRollout({ data }: { data: GetReleaseChannelResponse }) {
  const { tenantId } = usePlatform();
  const rollout = data.rollout;
  const members = rollout?.members.filter(item => !!tenantId && item.tenantId === tenantId) ?? [];
  const summary = rollout?.summary ?? data.channel?.rollout;
  return <VStack gap={5}>
    <Heading level={2}>Rollout progress</Heading>
    <Text>{summary ? summary.halted ? "Halted" : summary.done ? "Done" : summary.waiting || "In progress" : "No rollout summary returned."}</Text>
    {summary && <Text>Target Kubernetes minor: {summary.targetMinor || "Not reported"} · {summary.reportOnly ? "Report only" : "Active rollout policy"}</Text>}
    {!!summary?.tiers.length && <Table searchable searchText={({ item }) => [label(ClusterTier[item.tier], item.tier), item.pending, item.inProgress, item.succeeded, item.failed, item.blocked].join(" ")} aria-label="Rollout progress by tier" idKey="id" textOverflow="wrap" data={summary.tiers.map(item => ({ id: item.tier, item }))} columns={[
      { key: "tier", header: "Tier", renderCell: ({ item }) => <Text>{label(ClusterTier[item.tier], item.tier)}</Text> },
      { key: "pending", header: "Pending", renderCell: ({ item }) => <Text>{item.pending}</Text> },
      { key: "progress", header: "In progress", renderCell: ({ item }) => <Text>{item.inProgress}</Text> },
      { key: "succeeded", header: "Succeeded", renderCell: ({ item }) => <Text>{item.succeeded}</Text> },
      { key: "failed", header: "Failed / blocked", renderCell: ({ item }) => <Text>{item.failed} / {item.blocked}</Text> },
    ]} />}
    {members.length ? <Table searchable searchText={({ item }) => [item.clusterName, item.clusterId, label(RolloutState[item.state], item.state), item.currentVersion, item.targetVersion, item.gate, item.reason].join(" ")} aria-label="Channel rollout members" idKey="id" textOverflow="wrap" data={members.map((item, id) => ({ id, item }))} columns={[
      { key: "cluster", header: "Cluster", renderCell: ({ item }) => <VStack gap={1}>{item.clusterId ? <><Link href={`/clusters/${encodeURIComponent(item.clusterId)}`}>{item.clusterName || item.clusterId}</Link><CopyValue value={item.clusterId} label="cluster ID" isSecondary /></> : <Text>Not reported</Text>}</VStack> },
      { key: "state", header: "State", renderCell: ({ item }) => <StatusValue label={label(RolloutState[item.state], item.state)} tone={item.state === RolloutState.SUCCEEDED ? "success" : item.state === RolloutState.FAILED ? "error" : "neutral"} /> },
      { key: "version", header: "Current → target", renderCell: ({ item }) => <Text>{item.currentVersion || "Not reported"} → {item.targetVersion || "Not reported"}</Text> },
      { key: "reason", header: "Gate / reason", renderCell: ({ item }) => <Text>{item.gate || "No gate"} · {item.reason || "No reason reported"}</Text> },
    ]} /> : <Text>No rollout members returned.</Text>}
  </VStack>;
}

function ChannelDetail({ channel }: { channel: ReleaseChannel }) {
  const { api, tenantId } = usePlatform();
  const load = useCallback((signal: AbortSignal) => api.tenant.getReleaseChannel({ tenantId, channel }, { signal }), [api, tenantId, channel]);
  const query = useAsyncQuery(load);
  const info = query.data?.channel;
  const matches = info?.channel === channel;
  return <VStack gap={6}>
    <DetailHeader title={channelLabel(channel)} description="Current release offering, component pins and rollout history." metadata={<CopyValue value={channelSlug(channel) || String(channel)} label="channel name" isSecondary />} status={<StatusValue label={info ? info.paused ? "Rollout paused" : "Rollout not paused" : "Loading channel"} tone={info?.paused ? "warning" : "neutral"} />} actions={<RefreshButton label="Refresh channel details" isLoading={query.refreshing} onClick={query.refresh} />} />
    {query.error && <Banner status="error" title="Channel details unavailable" description={query.error} />}
    {query.loading && <Text role="status">Loading channel details…</Text>}
    {query.data && !matches && <Banner status="warning" title="Channel not returned" description="No matching channel details were returned by the server." />}
    {info && matches && <>
      <DetailSummary items={[
        { label: "Current release", value: <ReleaseLink name={info.release} />, note: "Offered now" },
        { label: "Kubernetes", value: info.kubernetesMinor || "Not reported", note: "Current offering minor" },
        { label: "Promotion", value: info.promotion || "Not reported", note: info.pinnedRelease ? `Pinned to ${info.pinnedRelease}` : "No release pin reported" },
        { label: "Rollout tier", value: label(ClusterTier[info.rolloutTier], info.rolloutTier), note: info.paused ? "Paused" : "Not paused" },
      ]} />
      <DetailTabs label="Channel detail sections" sections={[
        { id: "overview", label: "Overview", content: <><Heading level={2}>Channel offering</Heading><Text>Published release configuration for this channel. Cluster subscriptions and rollout observations are limited to tenant {tenantId}.</Text>{info.pending && <VStack gap={2}><Heading level={3}>Pending offering</Heading><ReleaseLink name={info.pending.release} /><Text>This is not the current offering. Since: {time(info.pending.since)} · Soak ends: {time(info.pending.until)}</Text></VStack>}{info.conditions.map((condition, index) => <Text key={index}>{condition.type}: {condition.message || condition.reason || "No message reported"}</Text>)}</> },
        { id: "components", label: "Components", content: info.release && info.kubernetesMinor ? <OfferedComponents key={`${info.release}:${info.kubernetesMinor}`} info={info} /> : <Text>The channel has no complete current offering. Versions cannot be inferred.</Text> },
        { id: "clusters", label: "Clusters", content: <ChannelClusters channel={channel} /> },
        { id: "rollout", label: "Rollout", content: <ChannelRollout data={query.data!} /> },
        { id: "history", label: "History", content: <><Heading level={2}>Release offering history</Heading>
          {info.history.length ? <Table searchable searchText={({ item }) => [item.release, time(item.from), item.to ? time(item.to) : "No end reported", item.reason].join(" ")} aria-label="Channel release history" idKey="id" textOverflow="wrap" data={info.history.map((item, id) => ({ id, item }))} columns={[
          { key: "release", header: "Release", renderCell: ({ item }) => <ReleaseLink name={item.release} /> },
          { key: "from", header: "From (UTC)", renderCell: ({ item }) => <Text>{time(item.from)}</Text> },
          { key: "to", header: "Until (UTC)", renderCell: ({ item }) => <Text>{item.to ? time(item.to) : "No end reported"}</Text> },
          { key: "reason", header: "Reason", renderCell: ({ item }) => <Text>{item.reason || "Not reported"}</Text> },
        ]} /> : <Text>No release offering history returned.</Text>}
        </> },
      ]} />
    </>}
  </VStack>;
}
