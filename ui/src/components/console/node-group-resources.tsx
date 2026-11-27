"use client";

import { Banner } from "@astryxdesign/core/Banner";
import { ProgressBar } from "@astryxdesign/core/ProgressBar";
import { VStack } from "@astryxdesign/core/Layout";
import { Text } from "@astryxdesign/core/Text";
import type { GroupResources, Share } from "@/lib/workflows/node-group-nodes";
import type { NodeTotals, NodeTotalsState } from "@/lib/workflows/node-resource-totals";
import { DetailSummary } from "./detail-layout";

/** Above this share of allocatable, requests leave little room to schedule: the meter turns to a warning. */
const BUSY_PERCENT = 85;

function ShareValue({ label, share }: { label: string; share: Share | undefined }) {
  if (!share) return <Text type="large" weight="semibold">—</Text>;
  return <VStack gap={2}>
    <Text type="large" weight="semibold">{`${share.percent}%`}</Text>
    <ProgressBar label={label} isLabelHidden value={Math.min(share.percent, 100)} variant={share.percent >= BUSY_PERCENT ? "warning" : "accent"} />
  </VStack>;
}

const readyNote = (resources: GroupResources | undefined) => {
  if (!resources) return undefined;
  return resources.notReady ? `${resources.ready} ready · ${resources.notReady} not ready` : `${resources.ready} ready`;
};

/** The group at a glance: how many nodes, and how much of their CPU, memory and pod room is requested. */
export function NodeGroupResources({ nodes, resources }: { nodes: number | undefined; resources: GroupResources | undefined }) {
  const missing = "Not reported yet";
  return <DetailSummary items={[
    { label: "Nodes", value: nodes === undefined ? "—" : nodes.toLocaleString("en-US"), note: nodes === undefined ? missing : readyNote(resources) },
    { label: "CPU requested", value: <ShareValue label="CPU requested" share={resources?.cpu} />, note: resources?.cpu?.label ?? missing },
    { label: "Memory requested", value: <ShareValue label="Memory requested" share={resources?.memory} />, note: resources?.memory?.label ?? missing },
    { label: "Pods", value: <ShareValue label="Pods scheduled" share={resources?.pods} />,
      note: resources ? [resources.pods?.label ?? `${resources.podCount} pods`, resources.podsWithoutRequests ? `${resources.podsWithoutRequests} without requests` : ""].filter(Boolean).join(" · ") : missing },
  ]} />;
}

/** Live totals in the summary's shape. */
export function totalsResources(totals: NodeTotals): GroupResources {
  return { source: "live", ready: totals.ready, notReady: totals.notReady, cpu: totals.cpu, memory: totals.memory, pods: totals.pods, podCount: totals.podCount, podsWithoutRequests: totals.podsWithoutRequests };
}

/** Where the totals above come from: the live reader, or why it gave none. */
export function NodeTotalsNote({ state, loading, error }: { state: NodeTotalsState | undefined; loading: boolean; error: string }) {
  if (loading) return <Text type="supporting" role="status">Reading live totals…</Text>;
  if (error) return <Banner status="error" title="Live totals unavailable" description={error} />;
  if (!state) return null;
  if (state.kind === "unavailable") return <Banner status="info" title="Live totals unavailable" description={state.message} />;
  if (state.kind === "empty") return <Text type="supporting" role="status">Live as of {state.liveAt}: the live read finds no nodes in this group right now.</Text>;
  const used = [state.totals.usedCpu && `CPU ${state.totals.usedCpu.label}`, state.totals.usedMemory && `memory ${state.totals.usedMemory.label}`].filter(Boolean).join(" · ");
  return <VStack gap={1}>
    <Text type="supporting" role="status">Live totals as of {state.liveAt}{state.complete ? "" : " · partial: the read stopped at a limit"}.</Text>
    <Text type="supporting">{used ? `In use now: ${used}.` : "Current usage is not readable (no metrics API); requests and allocatable still show."}</Text>
  </VStack>;
}
