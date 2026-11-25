"use client";

import { useState } from "react";
import { VStack } from "@astryxdesign/core/Layout";
import { Text } from "@astryxdesign/core/Text";
import type { ClusterForm } from "@/lib/workflows/clusters";
import { ValuesEditor } from "./values-editor";

export type ClusterEditorMode = "guided" | "yaml";
export const clusterModeLabels = { guided: "Form", yaml: "YAML" } as const;

/**
 * Form and YAML views of one draft. Switching to YAML renders the form; switching back (or
 * reviewing from YAML) parses the text and replaces the form with it. A parse error keeps
 * the YAML view open with the message, which names the line.
 */
export function useClusterYamlMode({ toYaml, fromYaml, onApplied, onEdit, onError }: {
  toYaml: () => string;
  fromYaml: (text: string) => ClusterForm;
  onApplied: (form: ClusterForm) => void;
  onEdit: () => void;
  onError: (error: Error) => void;
}) {
  const [mode, setMode] = useState<ClusterEditorMode>("guided");
  const [text, setText] = useState("");
  const [error, setError] = useState("");
  /** Parses the YAML into the form; throws (and shows the error) when it cannot. */
  function apply(): ClusterForm {
    try {
      const form = fromYaml(text);
      setError("");
      onApplied(form);
      return form;
    } catch (cause) {
      const message = cause instanceof Error ? cause.message : String(cause);
      setError(message);
      throw cause;
    }
  }
  function switchMode(next: ClusterEditorMode) {
    if (next === mode) return;
    if (next === "yaml") {
      try { setText(toYaml()); setError(""); setMode("yaml"); onEdit(); }
      catch (cause) { onError(new Error(`Fix this before switching to YAML: ${cause instanceof Error ? cause.message : String(cause)}`)); }
      return;
    }
    try { apply(); setMode("guided"); onEdit(); }
    catch (cause) { onError(new Error(`The YAML could not be applied to the form. ${cause instanceof Error ? cause.message : String(cause)}`)); }
  }
  function edit(next: string) { setText(next); setError(""); onEdit(); }
  return { mode, text, error, apply, switchMode, edit };
}

export function ClusterYamlPanel({ value, onChange, error, disabled, editing }: { value: string; onChange: (value: string) => void; error: string; disabled: boolean; editing: boolean }) {
  return <VStack gap={3} data-cluster-yaml="">
    <Text color="secondary">
      The whole cluster as a cluster-vending.io/v1 manifest, the same the CLI applies. Edits replace the form when you switch back or {editing ? "preview the plan" : "review"}.
      {editing ? " The update sends only the fields that differ from the cluster as it opened; the name, region, network and fields set at creation cannot change." : " Leave a field out for the server default."}
    </Text>
    <ValuesEditor label="Cluster YAML" value={value} onChange={onChange} disabled={disabled} error={error || undefined} language="yaml" height="24rem"
      description="At most 64 KiB. Unknown fields, custom tags and recursive aliases are rejected; errors name their line." />
  </VStack>;
}
