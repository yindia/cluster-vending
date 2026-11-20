"use client";

import { useId, useState, type ComponentProps, type ReactNode } from "react";
import { Button } from "@astryxdesign/core/Button";
import { HStack, VStack } from "@astryxdesign/core/Layout";

type FilterRowProps = Pick<ComponentProps<typeof HStack>, "children" | "padding" | "onFocusCapture" | "onPointerDownCapture"> & {
  label: string;
};

export function FilterRow({ label, ...props }: FilterRowProps) {
  return <HStack {...props} as="search" aria-label={label} className="cv-filter-row" gap={3} vAlign="end" width="100%" />;
}

/** Search stays available when the section is too narrow for all filters. */
export function SearchFilters({ label, search, values, children }: {
  label: string; search: ReactNode; values: readonly string[]; children: ReactNode;
}) {
  const id = useId();
  const [open, setOpen] = useState(false);
  const count = values.filter(value => value !== "all").length;

  return <VStack className="cv-search-filters">
    <HStack as="search" aria-label={label} className="cv-search-controls" gap={2} vAlign="end">
      {search}
      <Button className="cv-search-filter-toggle" variant="secondary" label={count ? `Filters (${count})` : "Filters"}
        aria-controls={id} aria-expanded={open} onClick={() => setOpen(value => !value)} />
      <HStack id={id} className="cv-search-filter-fields" data-open={open} gap={2} vAlign="end">{children}</HStack>
    </HStack>
  </VStack>;
}
