"use client";

import { useEffect, useId, useRef, useState } from "react";
import { HStack } from "@astryxdesign/core/Layout";
import { Text, type TextProps } from "@astryxdesign/core/Text";
import { Button } from "@astryxdesign/core/Button";
import { Icon } from "@astryxdesign/core/Icon";
import { useClipboard } from "@astryxdesign/core/hooks";

export function CopyButton({ value, label = "value" }: { value: string; label?: string }) {
  const id = useId();
  const target = useRef<HTMLElement>(null);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const { copy, isCopied } = useClipboard();
  const [message, setMessage] = useState("");
  const last = useRef("");
  const perform = useRef<() => void>(() => {});
  useEffect(() => {
    perform.current = () => {
      void copy(value).then((ok) => {
        const next = ok ? "Copied" : "Copy failed";
        setMessage(last.current === next ? `${next}\u00a0` : next);
        last.current = last.current === next ? `${next}\u00a0` : next;
        clearTimeout(timer.current);
        timer.current = setTimeout(() => setMessage(""), 2000);
      });
    };
  }, [copy, value]);
  useEffect(() => {
    const element = target.current;
    const command = (event: Event) => { if ((event as Event & { command?: string }).command === "--copy") perform.current(); };
    element?.addEventListener("command", command);
    return () => { element?.removeEventListener("command", command); clearTimeout(timer.current); };
  }, []);
  return <HStack ref={target} id={id} gap={1} vAlign="center" className="cv-copy-control" data-feedback={!!message}>
    <Button label={`Copy ${label}`} tooltip={message || `Copy ${label}`} icon={<Icon icon={message.trim() === "Copy failed" ? "error" : isCopied ? "check" : "copy"} color={message.trim() === "Copy failed" ? "error" : undefined} size="sm" />} isIconOnly variant="ghost" size="sm" type="button" isDisabled={!value}
      {...{ command: "--copy", commandfor: id }}
      onClick={() => { if (!("CommandEvent" in window)) perform.current(); }} />
    <Text role="status" className="cv-copy-feedback" type="supporting">{message}</Text>
  </HStack>;
}

export function CopyValue({ value, label = "value", display, isSecondary = false, type }: { value: string; label?: string; display?: string; isSecondary?: boolean; type?: TextProps["type"] }) {
  return <HStack gap={1} className="cv-copy-target" vAlign="center" minHeight="1.5rem" maxWidth="100%"><Text maxLines={1} type={type ?? (isSecondary ? "supporting" : "body")} weight={type === "large" ? "semibold" : undefined} color={isSecondary ? "secondary" : "primary"}>{display ?? (value || "Not reported")}</Text>{value && <CopyButton key={value} value={value} label={label} />}</HStack>;
}
