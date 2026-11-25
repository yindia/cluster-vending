"use client";

import { Banner } from "@astryxdesign/core/Banner";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { ProgressBar } from "@astryxdesign/core/ProgressBar";
import { Text } from "@astryxdesign/core/Text";
import type { ClusterRecommendations, ResourceRecommendation } from "@/gen/cloud/v1/recommendation_pb";
import {
  ageText, isActionable, metricsView, netChange, netChangeNote, netChangeText, utcTime, vpaView,
  type AvailabilityView, type RecommendationNotice, type ResourceField,
} from "@/lib/workflows/recommendations";
import { DetailSummary, StatusValue } from "./detail-layout";

interface SummaryProps {
  summary: ClusterRecommendations;
  rows: readonly ResourceRecommendation[];
  notices: readonly RecommendationNotice[];
  onlyChanges: boolean;
  stale: boolean;
  readAtNanos: bigint;
}

const plural = (count: number, noun: string) => `${count} ${noun}${count === 1 ? "" : "s"}`;

function changeTile(label: string, rows: readonly ResourceRecommendation[], field: ResourceField, actionable: number) {
  if (!actionable) return { label, value: "None yet", note: "Shown with the first recommendation" };
  const change = netChange(rows, field);
  return { label, value: netChangeText(change, field), note: netChangeNote(change) };
}

/** Workloads, coverage, the net request change and freshness as four compact cards. */
export function RecommendationSummaryTiles({ summary, rows, onlyChanges, stale, readAtNanos }: Omit<SummaryProps, "notices">) {
  const sampled = !!summary.sampledAt;
  const actionable = rows.filter(isActionable).length;
  const total = onlyChanges ? rows.length : summary.containers || rows.length;
  const percent = total ? Math.round(actionable / total * 100) : 0;
  return <DetailSummary minColumnWidth={160} items={[
    {
      label: "With recommendations",
      value: <VStack gap={2}>
        <Text type="large" weight="semibold">{sampled ? `${actionable} of ${total}` : "Not sampled"}</Text>
        {sampled && <ProgressBar label={`${actionable} of ${total} containers have recommendations`} isLabelHidden value={percent} variant={actionable ? "accent" : "neutral"} />}
      </VStack>,
      note: sampled ? `${plural(summary.workloads, "workload")} · ${plural(summary.containers, "container")}${onlyChanges ? " · ≥10% changes only" : ""}` : "The controller has not recorded a sample",
    },
    changeTile("CPU request change", rows, "cpuMillis", actionable),
    changeTile("Memory request change", rows, "memoryBytes", actionable),
    {
      label: "Last sampled",
      value: <VStack gap={1}>
        <Text type="large" weight="semibold">{ageText(summary.sampledAt, readAtNanos)}</Text>
        <StatusValue label={!sampled ? "Never sampled" : stale ? "May be stale" : "Current"} tone={!sampled ? "neutral" : stale ? "warning" : "success"} />
      </VStack>,
      note: `${utcTime(summary.sampledAt, "No sample")}${summary.windowDays ? ` · ${summary.windowDays}-day window` : ""}`,
    },
  ]} />;
}

function Availability({ label, view }: { label: string; view: AvailabilityView }) {
  return <HStack gap={2} vAlign="center">
    <Text type="supporting">{label}</Text>
    <StatusValue label={view.label} tone={view.tone} />
  </HStack>;
}

/** The usage sources as the data reports them, and the last attempt when it differs from the last sample. */
export function RecommendationSources({ summary, notices }: { summary: ClusterRecommendations; notices: readonly RecommendationNotice[] }) {
  const attempted = utcTime(summary.attemptedAt, "");
  const showAttempt = !!attempted && attempted !== utcTime(summary.sampledAt, "");
  return <HStack as="section" aria-label="Usage sources" gap={4} vAlign="center" wrap="wrap">
    <Availability label="Metrics API" view={metricsView(summary, notices)} />
    <Availability label="Vertical Pod Autoscaler" view={vpaView(summary, notices)} />
    {showAttempt && <HStack gap={2} vAlign="center"><Text type="supporting">Last attempt</Text><Text type="label">{attempted}</Text></HStack>}
  </HStack>;
}

/** One banner per server note, in the API's wording behind the toggle. */
export function RecommendationNotices({ notices }: { notices: readonly RecommendationNotice[] }) {
  if (!notices.length) return null;
  const order = { error: 0, warning: 1, info: 2 } as const;
  return <VStack gap={2}>
    {[...notices].sort((a, b) => order[a.status] - order[b.status]).map(notice => <Banner key={`${notice.kind}:${notice.raw}`} status={notice.status} title={notice.title} description={notice.description}>
      <Text type="supporting">{`Server message: ${notice.raw}`}</Text>
    </Banner>)}
  </VStack>;
}
