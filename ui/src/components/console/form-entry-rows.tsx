"use client";

import { useId, useLayoutEffect, useRef } from "react";
import { Button } from "@astryxdesign/core/Button";
import { Heading } from "@astryxdesign/core/Heading";
import { Icon } from "@astryxdesign/core/Icon";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { Selector } from "@astryxdesign/core/Selector";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";

export type FormEntry = { id: string; key: string; value: string; choice: string };
export function keyIssues(rows: FormEntry[], withEffect = false) {
  const seen = new Map<string, number>();
  const issues = new Map<string, string>();
  rows.forEach((row, index) => {
    if (!row.key.trim()) {
      issues.set(row.id, "Enter a key or remove this row.");
      return;
    }
    const identity = withEffect ? JSON.stringify([row.key, row.choice]) : row.key;
    const first = seen.get(identity);
    if (first === undefined) seen.set(identity, index + 1);
    else issues.set(row.id, withEffect ? `This key and effect already appear in row ${first}.` : `This key already appears in row ${first}.`);
  });
  return issues;
}

type Props = {
  label: string;
  rowLabel: string;
  hint?: string;
  rows: FormEntry[];
  onChange: (rows: FormEntry[]) => void;
  disabled: boolean;
  limit?: number;
  issues?: ReadonlyMap<string, string>;
  columns?: { key: string; value: string };
  select?: { label: string; initial: string; options: { label: string; value: string }[] };
};

export function FormEntryRows({ rows, label, rowLabel, hint, onChange: update, disabled, issues, limit = Infinity, columns = { key: "Key", value: "Value" }, select }: Props) {
  const addId = useId();
  const focusAfterChange = useRef<string | null>(null);
  useLayoutEffect(() => {
    if (focusAfterChange.current) document.getElementById(focusAfterChange.current)?.focus();
    focusAfterChange.current = null;
  }, [rows]);
  function change(id: string, patch: Partial<FormEntry>) { update(rows.map((row) => row.id === id ? { ...row, ...patch } : row)); }
  function add() {
    const id = crypto.randomUUID();
    focusAfterChange.current = `entry-${id}-key`;
    update([...rows, { id, key: "", value: "", choice: select?.initial ?? "" }]);
  }
  function remove(index: number) {
    const adjacent = rows[index + 1] ?? rows[index - 1];
    focusAfterChange.current = adjacent ? `entry-${adjacent.id}-key` : addId;
    update(rows.filter((_, i) => i !== index));
  }
  return <VStack gap={2} data-row-editor={select ? "with-choice" : "pair"} role="group" aria-label={label}>
    <HStack gap={3} hAlign="between" vAlign="center" wrap="wrap" width="100%">
      <VStack data-entry-intro="">{hint ? <Text type="supporting">{hint}</Text> : <Heading level={4}>{label}</Heading>}</VStack>
      <Button id={addId} label={`Add ${rowLabel}`} type="button" variant="secondary" onClick={add} isDisabled={disabled || rows.length >= limit} />
    </HStack>
    {!rows.length ? <Text type="supporting">No {label.toLowerCase()} configured.</Text> : <HStack data-entry-head="" aria-hidden="true">
      <Text type="supporting">{columns.key}</Text><Text type="supporting">{columns.value}</Text>{select && <Text type="supporting">{select.label}</Text>}
    </HStack>}
    {rows.map((row, index) => {
      const name = `${rowLabel[0].toUpperCase() + rowLabel.slice(1)} ${index + 1}`;
      const issue = issues?.get(row.id);
      return <HStack key={row.id} role="group" aria-label={name} data-entry-row="">
        {(["key", "value"] as const).map(column => {
          const id = `entry-${row.id}-${column}`;
          return <VStack key={column} gap={1} data-entry-cell={column}>
            <Text type="supporting" data-entry-caption="" aria-hidden="true">{columns[column]}</Text>
            <TextInput id={id} htmlName={id} label={`${name} ${columns[column].toLowerCase()}`} isLabelHidden value={row[column]} onChange={value => change(row.id, { [column]: value })}
              status={column === "key" && issue ? { type: "error", message: issue } : undefined}
              isDisabled={disabled} autoComplete="off" autoCapitalize="none" hasAutoCorrect={false} hasSpellCheck={false} />
          </VStack>;
        })}
        {select && <VStack gap={1} data-entry-cell="choice">
          <Text type="supporting" data-entry-caption="" aria-hidden="true">{select.label}</Text>
          <Selector label={`${name} ${select.label.toLowerCase()}`} isLabelHidden value={row.choice} onChange={(choice) => change(row.id, { choice })} options={select.options} isDisabled={disabled} />
        </VStack>}
        <HStack data-entry-remove="">
          <Button label={`Remove ${rowLabel} ${index + 1}`} tooltip={`Remove ${rowLabel} ${index + 1}`} isIconOnly icon={<Icon icon="close" />} type="button" variant="ghost" onClick={() => remove(index)} isDisabled={disabled} />
        </HStack>
      </HStack>;
    })}
  </VStack>;
}
