"use client";

import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Dialog, DialogHeader } from "@astryxdesign/core/Dialog";
import { Divider } from "@astryxdesign/core/Divider";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { MetadataList, MetadataListItem } from "@astryxdesign/core/MetadataList";
import { proportional } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import { RecommendationSource, type ResourceRecommendation } from "@/gen/cloud/v1/recommendation_pb";
import {
  amount, canRecommend, current, differenceText, hasRequest, percentText, recommended, sourceLabel, sourceTone, usageText,
} from "@/lib/workflows/recommendations";
import { ConsoleTable } from "./console-table";
import { CopyValue } from "./copy-value";
import { StatusValue } from "./detail-layout";

/** Read-only detail of one container: requests, limits, recorded usage and the exact byte quantities. */
export function RecommendationInspector({ recommendation: row, onClose }: { recommendation: ResourceRecommendation; onClose: () => void }) {
  const memoryLimit = canRecommend(row) && (row.recommendedLimits?.memoryBytes ?? 0n) > 0n ? amount(row.recommendedLimits!.memoryBytes, "memoryBytes") : "No recommendation";
  const close = (open: boolean) => { if (!open) onClose(); };
  return <Dialog isOpen onOpenChange={close} purpose="info" width="min(54rem, calc(100vw - 2rem))" maxHeight="90dvh" padding={0}>
    <DialogHeader title={`${row.workloadName} / ${row.container}`} subtitle="Read-only container resource recommendation" onOpenChange={close} />
    <VStack gap={5} padding={6}>
      <MetadataList orientation="horizontal">
        <MetadataListItem label="Namespace"><CopyValue value={row.namespace} label="namespace" /></MetadataListItem>
        <MetadataListItem label="Workload kind">{row.workloadKind}</MetadataListItem>
        <MetadataListItem label="Workload"><CopyValue value={row.workloadName} label="workload name" /></MetadataListItem>
        <MetadataListItem label="Container"><CopyValue value={row.container} label="container name" /></MetadataListItem>
        <MetadataListItem label="Source"><StatusValue label={sourceLabel(row.source)} tone={sourceTone(row.source)} /></MetadataListItem>
        <MetadataListItem label="Replicas">{row.replicas}</MetadataListItem>
        <MetadataListItem label="Platform workload">{row.platform ? "Yes" : "No"}</MetadataListItem>
        <MetadataListItem label="Samples / recorded hours">{row.samples.toLocaleString("en-US")} samples / {row.dataHours} hours</MetadataListItem>
      </MetadataList>
      {!canRecommend(row) && <Banner status="info" title={sourceLabel(row.source)} description={row.source === RecommendationSource.INSUFFICIENT_DATA ? "Less than one day of usage has been recorded. There is no usage-based recommendation yet." : "No supported recommendation is available. Missing values do not mean that this container needs zero CPU or memory."} />}
      <ConsoleTable aria-label="Container requests limits and usage" idKey="metric" data={[
        { metric: "Current requests", cpu: current(row, "cpuMillis"), memory: current(row, "memoryBytes") },
        { metric: "Recommended requests", cpu: recommended(row, "cpuMillis"), memory: recommended(row, "memoryBytes") },
        { metric: "Request difference", cpu: differenceText(row, "cpuMillis"), memory: differenceText(row, "memoryBytes") },
        { metric: "Request difference (%)", cpu: percentText(row, "cpuMillis"), memory: percentText(row, "memoryBytes") },
        { metric: "Current limits", cpu: current(row, "cpuMillis", true), memory: current(row, "memoryBytes", true) },
        { metric: "Recommended limits", cpu: "CPU limits are never recommended", memory: memoryLimit },
        { metric: "Peak recorded usage", cpu: usageText(row, row.usagePeak, "cpuMillis"), memory: usageText(row, row.usagePeak, "memoryBytes") },
        { metric: "Average recorded usage", cpu: usageText(row, row.usageAverage, "cpuMillis"), memory: usageText(row, row.usageAverage, "memoryBytes") },
      ]} columns={[
        { key: "metric", header: "Metric", width: proportional(2, { minWidth: 120 }) },
        { key: "cpu", header: "CPU", width: proportional(2, { minWidth: 120 }) },
        { key: "memory", header: "Memory", width: proportional(2, { minWidth: 120 }) },
      ]} />
      <Text type="supporting">Memory displays marked ≈ are truncated to two decimal places. Exact byte quantities are preserved below. No values are applied to the workload.</Text>
      <Divider />
      <MetadataList title={<Heading level={3}>Exact memory quantities</Heading>}>
        <MetadataListItem label="Current request (bytes)">{row.currentRequests?.memoryBytes ? <CopyValue value={row.currentRequests.memoryBytes.toString()} label="current memory request in bytes" /> : "Not set"}</MetadataListItem>
        <MetadataListItem label="Recommended request (bytes)">{hasRequest(row, "memoryBytes") ? <CopyValue value={row.recommendedRequests!.memoryBytes.toString()} label="recommended memory request in bytes" /> : "No recommendation"}</MetadataListItem>
        <MetadataListItem label="Current limit (bytes)">{row.currentLimits?.memoryBytes ? <CopyValue value={row.currentLimits.memoryBytes.toString()} label="current memory limit in bytes" /> : "Not set"}</MetadataListItem>
        <MetadataListItem label="Recommended limit (bytes)">{canRecommend(row) && row.recommendedLimits?.memoryBytes ? <CopyValue value={row.recommendedLimits.memoryBytes.toString()} label="recommended memory limit in bytes" /> : "No recommendation"}</MetadataListItem>
      </MetadataList>
      <HStack hAlign="end"><Button label="Close recommendation details" variant="secondary" onClick={onClose} /></HStack>
    </VStack>
  </Dialog>;
}
