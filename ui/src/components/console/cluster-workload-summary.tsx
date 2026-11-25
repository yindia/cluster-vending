"use client";

import { useEffect, useRef, useState, type ReactNode } from "react";
import { Banner } from "@astryxdesign/core/Banner";
import { Heading } from "@astryxdesign/core/Heading";
import { VStack } from "@astryxdesign/core/Layout";
import { Link } from "@astryxdesign/core/Link";
import { ProgressBar } from "@astryxdesign/core/ProgressBar";
import { proportional } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import type { ClusterStatus } from "@/gen/cloud/v1/cluster_pb";
import {
  clusterCapacity, clusterSummaryView, NOT_READ, nodeGroupSummaryRows, severityTone, utilizationTone,
  type CapacityView, type NodeGroupSummaryRow, type ResourceShare, type SummaryItem,
} from "@/lib/workflows/cluster-summary";
import { ConsoleTable } from "./console-table";
import { DetailSummary, StatusValue } from "./detail-layout";

interface ClusterWorkloadSummaryProps {
  status: ClusterStatus | undefined;
  // Links each node group row to its detail page when set.
  clusterId?: string;
}

// How often the ages, and the "old" marks, are worked out again.
const CLOCK_TICK_MS = 60_000;
// Below this container width the node group table folds into two columns.
const WIDE_TABLE_MIN_PX = 720;

// ClusterWorkloadSummary is the cluster status summary the controller's
// passes recorded (PROP-02 5.2): health (nodes, API reachability, policy
// violations and events with their breakdowns), cluster-wide requested and
// allocatable capacity, and a row per node group. Old counts are marked;
// unknown values are never shown as zeros.
export function ClusterWorkloadSummary({ status, clusterId }: ClusterWorkloadSummaryProps) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), CLOCK_TICK_MS);
    return () => clearInterval(timer);
  }, []);
  const view = clusterSummaryView(status, now);
  const capacity = clusterCapacity(status, now);
  const rows = nodeGroupSummaryRows(status?.nodeGroups ?? []);
  const [nodes, violations, events] = view.items;
  return <VStack gap={4} as="section" aria-label="Workload summary">
    <VStack gap={1}>
      <Heading level={3}>Workload summary</Heading>
      <Text type="supporting">Requests only, as the controller&apos;s node and policy passes read them; no live usage.</Text>
    </VStack>
    {view.notice && <Banner status="warning" title="The workload cluster cannot be reached" description={view.notice} />}
    <DetailSummary items={[
      countTile(nodes),
      { label: "API reachability", value: <StatusValue label={view.reachability.value} tone={view.reachability.tone} />, note: view.reachability.note },
      countTile(violations, severityTone),
      countTile(events),
    ]} />
    <DetailSummary items={capacityTiles(capacity, rows.length)} />
    {rows.length > 0 && <NodeGroupSummaryTable rows={rows} clusterId={clusterId} />}
  </VStack>;
}

function oldNote(note: string, old: boolean): ReactNode {
  return old ? <StatusValue label={note} tone="warning" /> : note;
}

function countTile(item: SummaryItem, tone: (name: string) => "success" | "warning" | "error" | "neutral" = () => "neutral") {
  const breakdown = item.breakdown?.length
    ? <VStack gap={1}>
      <Text type="large" weight="semibold">{item.value}</Text>
      {item.breakdown.map(entry => <StatusValue key={entry.name} label={`${entry.name} ${entry.count}`} tone={tone(entry.name)} />)}
    </VStack>
    : item.value;
  return { label: item.label, value: breakdown, note: oldNote(item.note, item.old) };
}

function Meter({ label, share }: { label: string; share: ResourceShare }) {
  return <ProgressBar label={label} isLabelHidden value={Math.min(share.percent, 100)} variant={utilizationTone(share.percent)} />;
}

function shareTile(label: string, share: ResourceShare | undefined, note: string, old: boolean) {
  if (!share) return { label, value: "Not reported", note: oldNote(note, old) };
  return {
    label,
    value: <VStack gap={2}>
      <Text type="large" weight="semibold">{`${share.percent}%`}</Text>
      <Meter label={`${label} ${share.percent}%`} share={share} />
    </VStack>,
    note: oldNote(`${share.requested} of ${share.allocatable}${note ? ` · ${note}` : ""}`, old),
  };
}

function capacityTiles(capacity: CapacityView | undefined, groups: number) {
  if (!capacity) {
    return [
      { label: "Node groups", value: groups ? String(groups) : NOT_READ, note: "Resources not read yet" },
      { label: "CPU requested", value: NOT_READ, note: "Summed by the node pass" },
      { label: "Memory requested", value: NOT_READ, note: "Summed by the node pass" },
      { label: "Pods", value: NOT_READ, note: "Running pods against allocatable" },
    ];
  }
  const user = capacity.groups - capacity.platformGroups;
  const groupNote = [`${capacity.platformGroups} platform · ${user} yours`, capacity.readGroups < capacity.groups ? `${capacity.groups - capacity.readGroups} not read yet` : ""].filter(Boolean).join(" · ");
  const pods = capacity.pods ? `${capacity.podCount} / ${capacity.pods.allocatable}` : String(capacity.podCount);
  const withoutRequests = capacity.podsWithoutRequests;
  return [
    { label: "Node groups", value: String(capacity.groups), note: groupNote },
    shareTile("CPU requested", capacity.cpu, "", capacity.old),
    shareTile("Memory requested", capacity.memory, "", capacity.old),
    {
      label: "Pods",
      value: <VStack gap={2}>
        <Text type="large" weight="semibold">{pods}</Text>
        {capacity.pods && <Meter label={`Pods ${capacity.pods.percent}% of allocatable`} share={capacity.pods} />}
      </VStack>,
      note: withoutRequests > 0
        ? <StatusValue label={`${withoutRequests} without requests`} tone="warning" />
        : oldNote("All set resource requests", capacity.old),
    },
  ];
}

function GroupName({ row, clusterId }: { row: NodeGroupSummaryRow; clusterId?: string }) {
  return <VStack gap={1}>
    {clusterId
      ? <Link href={`/clusters/${encodeURIComponent(clusterId)}/node-groups/${encodeURIComponent(row.name)}`} aria-label={`Open node group ${row.name}`}>{row.name}</Link>
      : <Text weight="semibold">{row.name}</Text>}
    {row.platformManaged && <Text type="supporting">Managed by platform</Text>}
  </VStack>;
}

function ShareCell({ label, share, read }: { label: string; share?: ResourceShare; read: boolean }) {
  if (!share) return <Text type="supporting" color="secondary">{read ? "Not reported" : NOT_READ}</Text>;
  return <VStack gap={1}>
    <Meter label={`${label} ${share.percent}%`} share={share} />
    <Text type="supporting">{share.text}</Text>
  </VStack>;
}

function PodsCell({ row }: { row: NodeGroupSummaryRow }) {
  if (!row.read) return <Text type="supporting" color="secondary">{NOT_READ}</Text>;
  return <VStack gap={1}>
    <Text>{row.pods ? `${row.podCount} / ${row.pods.allocatable}` : String(row.podCount)}</Text>
    {!!row.podsWithoutRequests && <Text type="supporting">{`${row.podsWithoutRequests} without requests`}</Text>}
  </VStack>;
}

function StatusCell({ row }: { row: NodeGroupSummaryRow }) {
  return <VStack gap={1}>
    <StatusValue label={row.phaseLabel} tone={row.tone} />
    <Text type="supporting">{row.nodes}</Text>
    {row.notReady > 0 && <Text type="supporting">{`${row.notReady} not ready`}</Text>}
  </VStack>;
}

function NodeGroupSummaryTable({ rows, clusterId }: { rows: NodeGroupSummaryRow[]; clusterId?: string }) {
  const container = useRef<HTMLElement>(null);
  const [width, setWidth] = useState(0);
  // The columns follow the container width, not the viewport.
  useEffect(() => {
    const element = container.current;
    if (!element) return;
    const measure = () => setWidth(Math.floor(element.getBoundingClientRect().width));
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(element);
    return () => observer.disconnect();
  }, []);
  const wide = width >= WIDE_TABLE_MIN_PX;
  const name = { key: "name", header: "Node group", resizable: false, width: proportional(1.4, { minWidth: 110 }), renderCell: (row: NodeGroupSummaryRow) => <GroupName row={row} clusterId={clusterId} /> };
  const columns = wide ? [
    name,
    { key: "status", header: "Status", resizable: false, width: proportional(1.2, { minWidth: 110 }), renderCell: (row: NodeGroupSummaryRow) => <StatusCell row={row} /> },
    { key: "cpu", header: "CPU requested", resizable: false, width: proportional(1.6, { minWidth: 140 }), renderCell: (row: NodeGroupSummaryRow) => <ShareCell label={`${row.name} CPU requested`} share={row.cpu} read={row.read} /> },
    { key: "memory", header: "Memory requested", resizable: false, width: proportional(1.6, { minWidth: 140 }), renderCell: (row: NodeGroupSummaryRow) => <ShareCell label={`${row.name} memory requested`} share={row.memory} read={row.read} /> },
    { key: "pods", header: "Pods", resizable: false, align: "end" as const, width: proportional(0.9, { minWidth: 90 }), renderCell: (row: NodeGroupSummaryRow) => <PodsCell row={row} /> },
  ] : [
    { ...name, renderCell: (row: NodeGroupSummaryRow) => <VStack gap={2}><GroupName row={row} clusterId={clusterId} /><StatusCell row={row} /></VStack> },
    { key: "requests", header: "Requests", resizable: false, width: proportional(1.6, { minWidth: 140 }), renderCell: (row: NodeGroupSummaryRow) => <VStack gap={2}>
      <Text type="supporting">CPU</Text>
      <ShareCell label={`${row.name} CPU requested`} share={row.cpu} read={row.read} />
      <Text type="supporting">Memory</Text>
      <ShareCell label={`${row.name} memory requested`} share={row.memory} read={row.read} />
      <Text type="supporting">Pods</Text>
      <PodsCell row={row} />
    </VStack> },
  ];
  return <VStack ref={container} width="100%">
    <ConsoleTable aria-label="Node group capacity" hasFullDetails idKey="name" data={rows} columns={columns}
      emptyState="Node groups will appear after the controller reports them." />
  </VStack>;
}
