"use client";

import { useMemo } from "react";
import { Tokenizer } from "@astryxdesign/core/Tokenizer";
import { createStaticSource } from "@astryxdesign/core/Typeahead";

/** Free-form list of short values (tags) with optional suggestions. */
export function TokenListInput({ label, values, onChange, suggestions = [], placeholder, description, error, disabled, maxEntries }: {
  label: string; values: readonly string[]; onChange: (values: string[]) => void; suggestions?: readonly string[];
  placeholder?: string; description?: string; error?: string; disabled?: boolean; maxEntries?: number;
}) {
  const source = useMemo(() => createStaticSource(suggestions.map(value => ({ id: value, label: value }))), [suggestions]);
  return <Tokenizer label={label} value={values.map(value => ({ id: value, label: value }))} searchSource={source}
    onChange={items => onChange(items.map(item => item.label.trim()).filter(Boolean))}
    hasCreate hasEntriesOnFocus hasClear={false} placeholder={placeholder} description={description} isDisabled={disabled} maxEntries={maxEntries}
    status={error ? { type: "error", message: error } : undefined} />;
}
