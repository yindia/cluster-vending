"use client";

import dynamic from "next/dynamic";
import { useId, type Ref } from "react";
import { Banner } from "@astryxdesign/core/Banner";
import { Card } from "@astryxdesign/core/Card";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { Text } from "@astryxdesign/core/Text";
import { Divider } from "@astryxdesign/core/Divider";
import { CopyButton } from "./copy-value";

export interface ValuesEditorHandle { focus: () => void }
export interface ValuesEditorProps {
  value: string; label: string; language?: "yaml" | "json";
  readOnly?: boolean; disabled?: boolean; onChange?: (value: string) => void;
  error?: string; description?: string; height?: string; placeholder?: string;
  /** The 1-based line `error` points at: marked in the editor and scrolled into view. */
  errorLine?: number;
  ref?: Ref<ValuesEditorHandle>;
}

const Editor = dynamic(() => import("./values-editor-client"), {
  ssr: false,
  loading: () => <VStack padding={4} minHeight="14rem"><Text role="status">Loading code editor…</Text></VStack>,
});

export function ValuesEditor({ label, description, error, language = "yaml", ...props }: ValuesEditorProps) {
  const id = useId();
  return <VStack gap={2}>
    <Card padding={0}>
      <VStack gap={0}>
        <HStack padding={3} gap={3} hAlign="between" vAlign="center" wrap="wrap">
          <HStack gap={2} vAlign="center"><Text type="label">{label}</Text><Text type="supporting">{language.toUpperCase()}{props.readOnly ? " · Read only" : ""}</Text></HStack>
          <CopyButton value={props.value} label={label} />
        </HStack>
        <Divider />
        <Editor {...props} label={label} language={language} error={error} descriptionId={`${id}-description`} errorId={`${id}-error`} />
      </VStack>
    </Card>
    <Text id={`${id}-description`} type="supporting">{description || (props.readOnly ? "Select text or use Copy to copy the complete values." : "Tab moves to the next control. Use Ctrl/Cmd+F to search the values.")}</Text>
    {error && <VStack id={`${id}-error`}><Banner status="error" role="alert" title={error} /></VStack>}
  </VStack>;
}
