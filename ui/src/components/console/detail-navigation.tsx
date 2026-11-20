"use client";

import { useId, type ReactNode } from "react";
import { usePathname, useSearchParams } from "next/navigation";
import { StackItem } from "@astryxdesign/core/Stack";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { Selector } from "@astryxdesign/core/Selector";
import { Tab, TabList } from "@astryxdesign/core/TabList";

type DetailPage = { value: string; label: string };
type DetailGroup = { label: string; pages: DetailPage[] };

export function useDetailSection(pages: readonly DetailPage[]) {
  const params = useSearchParams();
  const pathname = usePathname();
  const prefix = useId();
  const active = pages.find(page => page.value === params.get("tab")) ?? pages[0];

  function href(value: string) {
    const next = new URLSearchParams(params.toString());
    if (value === pages[0].value) next.delete("tab");
    else next.set("tab", value);
    return next.size ? `${pathname}?${next}` : pathname;
  }
  function open(value: string) {
    if (value !== active.value) window.history.pushState(null, "", href(value));
  }
  return { active: active.value, label: active.label, open, href, panelId: (value: string) => `${prefix}-${value}-panel` };
}

export function DetailNavigation({ resource, pages, selection, groups, actions }: {
  resource: string; pages: readonly DetailPage[]; selection: ReturnType<typeof useDetailSection>; groups?: DetailGroup[]; actions?: ReactNode;
}) {
  const { active, open, panelId } = selection;
  const current = groups?.find(group => group.pages.some(page => page.value === active));
  const primary = groups?.map(group => ({ value: group.pages[0].value, label: group.label })) ?? pages;
  const mobileOptions = groups?.map(group => group.pages.length === 1 ? group.pages[0] : { type: "section" as const, title: group.label, options: group.pages });
  return <VStack as="nav" aria-label={`${resource} navigation`} className="cv-detail-navigation">
    <VStack className="cv-detail-desktop-nav" gap={2}>
      <HStack gap={2} hAlign="between" vAlign="center" width="100%">
        <StackItem size="fill"><TabList value={current?.pages[0].value ?? active} onChange={open} role="tablist" aria-label={`${resource} sections`} hasDivider>
          {primary.map(page => <Tab key={page.value} value={page.value} label={page.label}
            panelId={panelId(current?.pages[0].value === page.value ? active : page.value)} />)}
        </TabList></StackItem>
        {actions}
      </HStack>
      {current && current.pages.length > 1 && <TabList value={active} onChange={open} role="tablist" aria-label={current.label}>
        {current.pages.map(page => <Tab key={page.value} value={page.value} label={page.label} panelId={panelId(page.value)} />)}
      </TabList>}
    </VStack>
    <HStack className="cv-detail-mobile-nav" gap={2} vAlign="end">
      <Selector label={`${resource} section`} presentation="adaptive" value={active} onChange={open} options={mobileOptions ?? [...pages]} />
      {actions}
    </HStack>
  </VStack>;
}
