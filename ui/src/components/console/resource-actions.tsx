"use client";

import { Button } from "@astryxdesign/core/Button";
import { DropdownMenu, type DropdownMenuItemData } from "@astryxdesign/core/DropdownMenu";

export function ResourceActions({ resource, items }: { resource: string; items: DropdownMenuItemData[] }) {
  return <DropdownMenu presentation="adaptive" alignment="end" aria-label={`${resource} actions`} items={items}
    renderTrigger={props => <Button {...props} label="Actions" variant="secondary" />} />;
}
