"use client";

import { useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { Table, useTableColumnResize, type TableColumn, type TableProps } from "@astryxdesign/core/Table";

type ConsoleTableProps<T extends Record<string, unknown>> = TableProps<T> & {
  hasFullDetails?: boolean;
  /** Only for complete, unpaginated datasets. Paginated views filter before slicing. */
  searchable?: boolean;
  searchText?: (row: T) => string;
};

/** Search the loaded configuration without serializing protobuf bigint values. */
function searchableText(value: unknown): string {
  if (value == null) return "";
  if (typeof value !== "object") return String(value);
  if (Array.isArray(value)) return value.map(searchableText).join(" ");
  return Object.entries(value).filter(([key]) => !key.startsWith("$")).map(([, item]) => searchableText(item)).join(" ");
}

/** Shared density, keyboard/pointer resizing and optional complete-dataset search. */
export function ConsoleTable<T extends Record<string, unknown>>(props: ConsoleTableProps<T>) {
  // A changed schema is a different table; keep its column sizing and search separate.
  return <ConsoleTableContent key={`${props["aria-label"]}:${props.columns?.map(column => column.key).join(":")}`} {...props} />;
}

function ConsoleTableContent<T extends Record<string, unknown>>({ hasFullDetails = false, searchable = false, searchText = searchableText, columns, data, plugins, ...props }: ConsoleTableProps<T>) {
  const [columnWidths, setColumnWidths] = useState<Record<string, number>>({});
  const [search, setSearch] = useState("");
  const resize = useTableColumnResize<T>({
    columns: columns as TableColumn<Record<string, unknown>>[] | undefined,
    columnWidths,
    onColumnResizeEnd: updates => setColumnWidths(previous => ({ ...previous, ...updates })),
  });
  const terms = search.trim().toLowerCase().split(/\s+/).filter(Boolean);
  const filtered = searchable && terms.length ? data?.filter(row => {
    const text = searchText(row).toLowerCase();
    return terms.every(term => text.includes(term));
  }) : data;
  const label = props["aria-label"] || "table";
  const resizable = (columns?.filter(column => column.resizable !== false).length ?? 0) > 1;
  return <Card padding={0}><VStack gap={0}>
    {searchable && <HStack padding={3} gap={3} vAlign="end" wrap="wrap">
      <TextInput label={`Search ${label}`} type="search" value={search} onChange={setSearch} placeholder="Search this table" hasClear autoComplete="off" width="min(100%, 24rem)" />
      {search && <Button label={`Clear ${label} search`} variant="ghost" onClick={() => setSearch("")} />}
      <Text type="supporting" role="status">{filtered?.length ?? 0} of {data?.length ?? 0} rows</Text>
    </HStack>}
    <Table {...props} columns={columns} data={filtered} plugins={{ ...plugins, consoleColumnResize: resize }} density="balanced" verticalAlign="middle" textOverflow={hasFullDetails ? "wrap" : "truncate"} dividers="rows" hasHover
      emptyState={search ? "No rows match this search. Clear the search to show all rows." : props.emptyState} />
    {resizable && <HStack padding={2} gap={3} hAlign="between" vAlign="center" wrap="wrap">
      <Text type="supporting" color="secondary">Drag column borders to resize · Focus a border and use ← / →</Text>
      {!!Object.keys(columnWidths).length && <Button label="Reset widths" aria-label={`Reset ${label} column widths`} variant="ghost" size="sm" onClick={() => setColumnWidths({})} />}
    </HStack>}
  </VStack>
  </Card>;
}
