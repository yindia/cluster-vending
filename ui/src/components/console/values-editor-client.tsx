"use client";

import { useImperativeHandle, useMemo, useRef } from "react";
import CodeMirror, { type ReactCodeMirrorRef } from "@uiw/react-codemirror";
import { EditorView } from "@codemirror/view";
import { HighlightStyle, syntaxHighlighting } from "@codemirror/language";
import { tags } from "@lezer/highlight";
import { yaml } from "@codemirror/lang-yaml";
import { json } from "@codemirror/lang-json";
import type { ValuesEditorProps } from "./values-editor";

// Third-party editor styling uses the same tokens as the owning Astryx theme.
const editorTheme = EditorView.theme({
  "&": { backgroundColor: "var(--color-syntax-background)", color: "var(--color-text-primary)", borderRadius: "var(--radius-container)" },
  ".cm-scroller": { fontFamily: "var(--font-family-code)", lineHeight: "var(--text-body-leading)", overscrollBehavior: "contain" },
  ".cm-content": { padding: "var(--spacing-3) 0", caretColor: "var(--color-text-primary)" },
  ".cm-line": { padding: "0 var(--spacing-3)" },
  ".cm-gutters": { backgroundColor: "var(--color-background-surface)", color: "var(--color-text-secondary)", borderRight: "thin solid var(--color-border)" },
  ".cm-activeLine, .cm-activeLineGutter": { backgroundColor: "var(--color-background-gray)" },
  "&.cm-focused": { outline: "0.125rem solid var(--color-accent)", outlineOffset: "-0.125rem" },
  // Match the default drawSelection selector's specificity. Its light-theme
  // fallback otherwise wins even when the surrounding Astryx theme is dark.
  "& > .cm-scroller > .cm-selectionLayer .cm-selectionBackground, &.cm-focused > .cm-scroller > .cm-selectionLayer .cm-selectionBackground": { backgroundColor: "var(--color-accent-muted)" },
  // drawSelection hides native backgrounds with !important. Restore a matching
  // pair for browser selections too; selected syntax spans use readable text.
  ".cm-content::selection, .cm-content .cm-line::selection, .cm-content .cm-line ::selection": {
    backgroundColor: "var(--color-accent-muted) !important", color: "var(--color-text-primary)",
  },
  "@media (forced-colors: active)": {
    "& > .cm-scroller > .cm-selectionLayer .cm-selectionBackground, &.cm-focused > .cm-scroller > .cm-selectionLayer .cm-selectionBackground": { backgroundColor: "transparent" },
    ".cm-content::selection, .cm-content .cm-line::selection, .cm-content .cm-line ::selection": {
      backgroundColor: "Highlight !important", color: "HighlightText",
    },
  },
  ".cm-panels, .cm-tooltip": { backgroundColor: "var(--color-background-popover)", color: "var(--color-text-primary)" },
  ".cm-cursor": { borderLeftColor: "var(--color-text-primary)" },
});
const highlighting = HighlightStyle.define([
  { tag: tags.keyword, color: "var(--color-syntax-keyword)" },
  { tag: [tags.string, tags.special(tags.string)], color: "var(--color-syntax-string)" },
  { tag: [tags.number, tags.bool, tags.null], color: "var(--color-syntax-number)" },
  { tag: tags.comment, color: "var(--color-syntax-comment)" },
  { tag: [tags.propertyName, tags.attributeName], color: "var(--color-syntax-property)" },
  { tag: tags.punctuation, color: "var(--color-syntax-punctuation)" },
]);

export default function ValuesEditorClient({ value, label, language = "yaml", readOnly = false, disabled = false, onChange, error, height = readOnly ? "auto" : "20rem", ref, descriptionId, errorId, placeholder }: ValuesEditorProps & { descriptionId: string; errorId: string }) {
  const editor = useRef<ReactCodeMirrorRef>(null);
  useImperativeHandle(ref, () => ({ focus: () => editor.current?.view?.focus() }), []);
  const extensions = useMemo(() => [
    language === "yaml" ? yaml() : json(),
    editorTheme, syntaxHighlighting(highlighting), EditorView.lineWrapping,
    EditorView.contentAttributes.of({
      role: "textbox", "aria-label": label, "aria-multiline": "true", "aria-readonly": String(readOnly || disabled),
      "aria-invalid": String(!!error), "aria-describedby": `${descriptionId}${error ? ` ${errorId}` : ""}`,
      tabindex: disabled ? "-1" : "0",
    }),
  ], [language, label, readOnly, disabled, error, descriptionId, errorId]);
  return <CodeMirror ref={editor} value={value} placeholder={placeholder} height={height} minHeight={readOnly ? "8rem" : undefined} maxHeight="24rem" maxWidth="100%" theme="none" extensions={extensions}
    readOnly={readOnly || disabled} editable={!disabled} indentWithTab={false}
    basicSetup={{ lineNumbers: true, foldGutter: true, syntaxHighlighting: false, highlightActiveLine: !readOnly, highlightActiveLineGutter: !readOnly }}
    onChange={(next) => { if (!readOnly && !disabled) onChange?.(next); }} />;
}
