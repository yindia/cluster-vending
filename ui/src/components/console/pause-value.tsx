"use client";

import { Tooltip } from "@astryxdesign/core/Tooltip";
import type { Freeze } from "@/gen/cloud/v1/freeze_pb";
import { StatusValue } from "./detail-layout";

/** Only effective API policy determines whether the whole resource is paused. */
export function pauseState(freeze?: Freeze): "paused" | "not-paused" | "let-through" {
  return !freeze ? "not-paused" : freeze.letThrough ? "let-through" : "paused";
}

export function PauseValue({ freeze }: { freeze?: Freeze }) {
  const state = pauseState(freeze);
  const label = state === "paused" ? "Paused" : state === "let-through" ? "Let through" : "Not paused";
  const detail = freeze ? [freeze.letThrough ? "Effective policy allows reconciliation" : "Effective policy pauses reconciliation", freeze.scope, freeze.reason].filter(Boolean).join(" · ") : "No effective reconciliation pause";
  return <Tooltip content={detail}><StatusValue label={label} tone={state === "paused" ? "warning" : "neutral"} /></Tooltip>;
}
