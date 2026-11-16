"use client";

import type { SVGProps } from "react";
import { Button, type ButtonProps } from "@astryxdesign/core/Button";
import { Icon } from "@astryxdesign/core/Icon";

// This installed Astryx version has no arrow-clockwise registry entry.
// Component-mode Icon supplies the glyph; Button owns loading motion and reduced-motion support.
function RefreshGlyph(props: SVGProps<SVGSVGElement>) {
  return <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" {...props}>
    <path d="M20 7v5h-5M20 12a8 8 0 1 0-2.34 5.66" />
  </svg>;
}

export type RefreshButtonProps = Omit<ButtonProps, "icon" | "isIconOnly" | "children" | "tooltip" | "href" | "as">;

/** Keep the contextual accessible name while showing one consistent refresh control. */
export function RefreshButton({ label, variant = "ghost", ...props }: RefreshButtonProps) {
  return <Button {...props} label={label} aria-label={props["aria-label"] ?? label} variant={variant} isIconOnly icon={<Icon icon={RefreshGlyph} />} tooltip="Refresh" />;
}
