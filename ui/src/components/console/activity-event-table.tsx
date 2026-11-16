"use client";

import { useRef } from "react";
import { Text } from "@astryxdesign/core/Text";
import { Tooltip } from "@astryxdesign/core/Tooltip";
import { Button } from "@astryxdesign/core/Button";
import { CheckboxInput } from "@astryxdesign/core/CheckboxInput";
import { HStack } from "@astryxdesign/core/Layout";
import { Selector } from "@astryxdesign/core/Selector";
import { TextInput } from "@astryxdesign/core/TextInput";
import { proportional, type TableColumn } from "@astryxdesign/core/Table";
import { EventType, type Event } from "@/gen/cloud/v1/events_pb";
import { ConsoleTable } from "./console-table";
import { CopyValue } from "./copy-value";
import { StatusValue } from "./detail-layout";

export type ActivityFilters = { search: string; kind: string; onlyWarnings: boolean };
export function filterActivityEvents(events: readonly Event[], filters: ActivityFilters) {
  const terms = filters.search.trim().toLowerCase().split(/\s+/).filter(Boolean);
  return events.filter(event => {
    const text = [event.reason, event.message, event.objectKind, event.objectName, EventType[event.type]].join(" ").toLowerCase();
    return terms.every(term => text.includes(term)) && (filters.kind === "all" || event.objectKind === filters.kind)
      && (!filters.onlyWarnings || event.type === EventType.WARNING);
  });
}

export function ActivityEventFilters({ value, onChange, events, onRequestKinds }: { value: ActivityFilters; onChange: (value: ActivityFilters) => void; events: readonly Event[]; onRequestKinds: () => void }) {
  const kinds = [...new Set([...events.map(event => event.objectKind), ...(value.kind === "all" ? [] : [value.kind])])].sort();
  return <HStack gap={3} wrap="wrap" vAlign="end" onFocusCapture={onRequestKinds} onPointerDownCapture={onRequestKinds}>
    <TextInput label="Search events" type="search" placeholder="Message, reason, kind or resource" value={value.search} onChange={search => onChange({ ...value, search })} hasClear autoComplete="off" width="min(100%, 24rem)" />
    <Selector label="Event kind" value={value.kind} onChange={kind => onChange({ ...value, kind })} options={[{ value: "all", label: "All kinds" }, ...kinds.map(kind => ({ value: kind, label: kind || "Unspecified kind" }))]} width="min(100%, 12rem)" />
    <CheckboxInput label="Warnings only" value={value.onlyWarnings} onChange={onlyWarnings => onChange({ ...value, onlyWarnings })} />
    <Button label="Clear activity filters" variant="ghost" onClick={() => onChange({ search: "", kind: "all", onlyWarnings: false })} />
  </HStack>;
}

function EventMessage({ message }: { message: string }) {
  const anchor = useRef<HTMLElement>(null);
  return <>
    <Text ref={anchor} maxLines={1} hasTruncateTooltip={false} tabIndex={0}>{message || "No details reported"}</Text>
    <Tooltip anchorRef={anchor} isEnabled={!!message} focusTrigger="always" touchTrigger="tap" content={<Text type="inherit" color="inherit" aria-hidden="true">{message}</Text>} />
  </>;
}

const columns: TableColumn<Event & { rowId: number }>[] = [
  { key: "time", header: "Time (UTC)", width: proportional(2.5, { minWidth: 160 }), renderCell: event => <Text maxLines={1} hasTruncateTooltip>{event.time ? new Date(Number(event.time.seconds) * 1000).toISOString().slice(0, 19).replace("T", " ") : "Not reported"}</Text> },
  { key: "type", header: "Type", width: proportional(1, { minWidth: 80 }), renderCell: event => <StatusValue label={event.type === EventType.WARNING ? "Warning" : event.type === EventType.NORMAL ? "Normal" : "Unknown"} tone={event.type === EventType.WARNING ? "warning" : "neutral"} /> },
  { key: "reason", header: "Reason", width: proportional(1.5, { minWidth: 100 }), renderCell: event => <Text maxLines={1} hasTruncateTooltip>{event.reason || "Not reported"}</Text> },
  { key: "message", header: "Message", width: proportional(3, { minWidth: 160 }), renderCell: event => <EventMessage message={event.message} /> },
  { key: "objectKind", header: "Kind", width: proportional(1, { minWidth: 80 }), renderCell: event => <Text maxLines={1} hasTruncateTooltip>{event.objectKind || "Not reported"}</Text> },
  { key: "objectName", header: "Resource", width: proportional(2, { minWidth: 120 }), renderCell: event => <CopyValue value={event.objectName} label="Event resource name" /> },
  { key: "count", header: "Count", width: proportional(0.75, { minWidth: 60 }), renderCell: event => <Text>{event.count}</Text> },
];

export function ActivityEventTable({ events, title, emptyMessage = "No events were reported." }: { events: readonly Event[]; title: string; emptyMessage?: string }) {
  return events.length ? <ConsoleTable aria-label={title} data={events.map((event, rowId) => ({ ...event, rowId }))} columns={columns} idKey="rowId" /> : <Text color="secondary">{emptyMessage}</Text>;
}
