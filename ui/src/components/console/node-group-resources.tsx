"use client";

import { ProgressBar } from "@astryxdesign/core/ProgressBar";
import { VStack } from "@astryxdesign/core/Layout";
import { Text } from "@astryxdesign/core/Text";
import type { GroupResources, Share } from "@/lib/workflows/node-group-nodes";
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
