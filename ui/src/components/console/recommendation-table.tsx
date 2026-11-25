"use client";

import { useLayoutEffect, useState } from "react";
import { Link } from "@astryxdesign/core/Link";
import { VStack } from "@astryxdesign/core/Layout";
import { proportional, type TableColumn } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import type { ResourceRecommendation } from "@/gen/cloud/v1/recommendation_pb";
import { changeView, sourceLabel, sourceTone, type ResourceField } from "@/lib/workflows/recommendations";
import { ConsoleTable } from "./console-table";
import { StatusValue } from "./detail-layout";

export interface RecommendationRow extends Record<string, unknown> {
  id: number;
  recommendation: ResourceRecommendation;
}

/** The table's own width, read through a callback ref so a table mounted later is still measured. */
function useElementWidth() {
  const [element, setElement] = useState<HTMLElement | null>(null);
  const [width, setWidth] = useState(0);
  useLayoutEffect(() => {    if (!element) return;
    const measure = () => setWidth(Math.floor(element.getBoundingClientRect().width));
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(element);
    return () => observer.disconnect();
  }, [element]);
  return [setElement, width] as const;
}

/**
 * A flexible column whose weight follows its floor, so the table's minimum width is exactly the sum of the floors
 * the column budget counts and no column is squeezed below its content.
 */
const fit = (minWidth: number) => proportional(minWidth / 100, { minWidth });

const NARROW_TABLE = 420;
const TINY_TABLE = 300;
const NAME_MIN_TINY = 110;
const NAME_MIN_NARROW = 130;
const NAME_MIN = 220;
const CHANGE_MIN = 200;
const REQUESTS_MIN = 150;
const SOURCE_MIN = 140;
const WORKLOAD_MIN = 200;
const REPLICAS_MIN = 90;

function Stacked({ value, detail, tone }: { value: string; detail: string; tone?: "accent" | "success" | "warning" | "error" | "neutral" }) {
  return <VStack gap={1}>
    <Text maxLines={1} hasTruncateTooltip>{value}</Text>
    {tone && tone !== "neutral" ? <StatusValue label={detail} tone={tone} /> : <Text type="supporting" maxLines={1} hasTruncateTooltip>{detail}</Text>}
  </VStack>;
}

function ChangeCell({ row, field }: { row: ResourceRecommendation; field: ResourceField }) {
  const view = changeView(row, field);
  return <Stacked value={view.value} detail={view.detail} tone={view.tone} />;
}

/** CPU and memory in one cell for narrow tables. */
function RequestsCell({ row }: { row: ResourceRecommendation }) {
  const cpu = changeView(row, "cpuMillis");
  const memory = changeView(row, "memoryBytes");
  return <VStack gap={1}>
    <Text maxLines={1} hasTruncateTooltip>{cpu.value}</Text>
    <Text type="supporting" maxLines={1} hasTruncateTooltip>{`CPU · ${cpu.detail}`}</Text>
    <Text maxLines={1} hasTruncateTooltip>{memory.value}</Text>
    <Text type="supporting" maxLines={1} hasTruncateTooltip>{`Memory · ${memory.detail}`}</Text>
  </VStack>;
}

function recommendationColumns(width: number, onSelect: (row: ResourceRecommendation) => void): TableColumn<RecommendationRow>[] {
  const nameMin = width < TINY_TABLE ? NAME_MIN_TINY : width < NARROW_TABLE ? NAME_MIN_NARROW : NAME_MIN;
  const split = width >= nameMin + CHANGE_MIN * 2;
  const workloadSplit = split && width >= nameMin + CHANGE_MIN * 2 + SOURCE_MIN + WORKLOAD_MIN;
  const name: TableColumn<RecommendationRow> = { key: "container", header: workloadSplit ? "Container" : "Workload", resizable: false, width: fit(nameMin), renderCell: ({ recommendation: row }) => <VStack gap={1}>
    <Link onClick={() => onSelect(row)} weight="semibold" maxLines={1} tooltip={`Inspect ${row.container}`}>{row.container}</Link>
    {!workloadSplit && <Text type="supporting" maxLines={1} hasTruncateTooltip>{`${row.workloadName} · ${row.namespace} · ${row.workloadKind}`}</Text>}
  </VStack> };
  if (!split) {
    return [name, { key: "requests", header: "Requests", resizable: false, width: fit(REQUESTS_MIN), renderCell: ({ recommendation: row }) => <RequestsCell row={row} /> }];
  }
  const fixed: TableColumn<RecommendationRow>[] = [
    name,
    { key: "cpu", header: "CPU request", resizable: false, width: fit(CHANGE_MIN), renderCell: ({ recommendation: row }) => <ChangeCell row={row} field="cpuMillis" /> },
    { key: "memory", header: "Memory request", resizable: false, width: fit(CHANGE_MIN), renderCell: ({ recommendation: row }) => <ChangeCell row={row} field="memoryBytes" /> },
  ];
  const optional: [number, TableColumn<RecommendationRow>][] = [
    [SOURCE_MIN, { key: "source", header: "Source", resizable: false, width: fit(SOURCE_MIN), renderCell: ({ recommendation: row }) => <StatusValue label={sourceLabel(row.source)} tone={sourceTone(row.source)} /> }],
    [WORKLOAD_MIN, { key: "workload", header: "Workload", resizable: false, width: fit(WORKLOAD_MIN), renderCell: ({ recommendation: row }) => <VStack gap={1}>
      <Text maxLines={1} hasTruncateTooltip>{row.workloadName}</Text>
      <Text type="supporting" maxLines={1} hasTruncateTooltip>{`${row.namespace} · ${row.workloadKind}`}</Text>
    </VStack> }],
    [REPLICAS_MIN, { key: "replicas", header: "Replicas", align: "end", resizable: false, width: fit(REPLICAS_MIN), renderCell: ({ recommendation: row }) => <Text>{row.replicas}</Text> }],
  ];
  const { columns } = optional.reduce((acc, [min, column]) => {
    if (column.key === "workload" && !workloadSplit) return acc;
    return acc.used + min <= width ? { used: acc.used + min, columns: [...acc.columns, column] } : acc;
  }, { used: nameMin + CHANGE_MIN * 2, columns: fixed });
  // Keep the workload next to its container when it is shown.
  return [...columns.filter(column => column.key !== "workload").slice(0, 1), ...columns.filter(column => column.key === "workload"), ...columns.filter(column => column.key !== "workload").slice(1)];
}

export function RecommendationTable({ rows, rowIndexStart, rowCount, emptyState, onSelect }: {
  rows: RecommendationRow[]; rowIndexStart: number; rowCount: number; emptyState: string; onSelect: (row: ResourceRecommendation) => void;
}) {
  const [container, width] = useElementWidth();
  return <VStack ref={container} width="100%">
    {width > 0 && <ConsoleTable aria-label="Container resource recommendations" hasFullDetails data={rows} columns={recommendationColumns(width, onSelect)} idKey="id"
      rowIndexStart={rowIndexStart} rowCount={rowCount} emptyState={emptyState} />}
  </VStack>;
}
