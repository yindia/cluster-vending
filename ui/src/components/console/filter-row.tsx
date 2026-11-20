import type { ComponentProps } from "react";
import { HStack } from "@astryxdesign/core/Layout";

type FilterRowProps = Pick<ComponentProps<typeof HStack>, "children" | "padding" | "onFocusCapture" | "onPointerDownCapture"> & {
  label: string;
};

export function FilterRow({ label, ...props }: FilterRowProps) {
  return <HStack {...props} as="search" aria-label={label} className="cv-filter-row" gap={3} vAlign="end" width="100%" />;
}
