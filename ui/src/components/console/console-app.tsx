"use client";

import { useCallback, useEffect, useId, useState, type ReactNode } from "react";
import { useRouter } from "next/navigation";
import { AppShell } from "@astryxdesign/core/AppShell";
import { TopNav, TopNavHeading } from "@astryxdesign/core/TopNav";
import { SideNav, SideNavItem, SideNavSection } from "@astryxdesign/core/SideNav";
import { Layout, LayoutContent, LayoutFooter, VStack, HStack } from "@astryxdesign/core/Layout";
import { Heading, Text } from "@astryxdesign/core/Text";
import { Button } from "@astryxdesign/core/Button";
import { CheckboxInput } from "@astryxdesign/core/CheckboxInput";
import { Link } from "@astryxdesign/core/Link";
import { Dialog, DialogHeader } from "@astryxdesign/core/Dialog";
import { Banner } from "@astryxdesign/core/Banner";
import { Divider } from "@astryxdesign/core/Divider";
import { SegmentedControl, SegmentedControlItem } from "@astryxdesign/core/SegmentedControl";
import { useAppearance } from "@/app/providers";
import { usePlatform } from "@/components/platform-provider";
import { useBrowserAuth } from "@/components/auth-boundary";
import { useAsyncQuery } from "@/lib/api/use-query";
import type { Section } from "@/lib/navigation";
import { tenantSwitchPath } from "@/lib/workflows/tenants";
import { ConnectionPanel } from "./connection-panel";
import { TenantSelector } from "./tenant-selector";
import { RefreshButton } from "./refresh-button";
import { OverviewPage } from "./overview-page";
import { AccountsPage } from "./accounts-page";
import { NetworksPage } from "./networks-page";
import { NodeGroupDetail } from "./cluster-node-group-detail";
import { SubTenantDetail } from "./subtenant-detail";
import { ClustersPage } from "./clusters-page";
import { ReleasesPage } from "./releases-page";
import { ComponentsPage } from "./components-page";
import { FreezePage, type FreezeSelection } from "./freeze-page";

const navigation = [
  { section: "overview", label: "Overview" },
  { section: "accounts", label: "Accounts" },
  { section: "networks", label: "Networks" },
  { section: "clusters", label: "Clusters" },
  { section: "components", label: "Component catalog" },
  { section: "releases", label: "Releases" },
  { section: "freeze", label: "Freeze controls" },
] as const;

function SessionGate({ children, onConfigure }: { children: ReactNode; onConfigure: () => void }) {
  const { api, setIdentity, identity } = usePlatform();
  const load = useCallback((signal: AbortSignal) => api.tenant.whoAmI({}, { signal }), [api]);
  const query = useAsyncQuery(load);
  useEffect(() => { if (query.data) setIdentity(query.data); }, [query.data, setIdentity]);
  if (query.loading) return <Text role="status">Connecting to the platform API…</Text>;
  if (query.error) return <VStack gap={4}><Banner status="error" role="alert" title="Platform connection unavailable" description={query.error} /><HStack gap={2} hAlign="end" vAlign="center" width="100%" wrap="wrap"><Button label="Configure connection" onClick={onConfigure} /><RefreshButton label="Retry connection" onClick={query.refresh} isLoading={query.refreshing} /></HStack></VStack>;
  if (!identity) return null;
  return children;
}

function SectionPage({ section, resourceId, subtenantId, nodeGroup, channelId, componentClusterId, freezeSelection }: { section: Section; resourceId?: string; subtenantId?: string; nodeGroup?: string; channelId?: "alpha" | "beta" | "stable"; componentClusterId?: string; freezeSelection?: FreezeSelection }) {
  const { tenantId } = usePlatform();
  if (!tenantId) return <ConnectionPanel />;
  switch (section) {
    case "overview": return <OverviewPage />;
    case "accounts": return <AccountsPage resourceId={resourceId} />;
    case "networks": return <NetworksPage resourceId={resourceId} />;
    case "clusters": return resourceId && nodeGroup ? <NodeGroupDetail key={nodeGroup} clusterId={resourceId} group={nodeGroup} /> : resourceId && subtenantId ? <SubTenantDetail key={subtenantId} clusterId={resourceId} subtenantId={subtenantId} /> : <ClustersPage resourceId={resourceId} />;
    case "components": return <ComponentsPage resourceId={resourceId} clusterId={componentClusterId} />;
    case "releases": return <ReleasesPage resourceId={resourceId} channelId={channelId} />;
    case "freeze": return <FreezePage initialSelection={freezeSelection} />;
  }
}

export default function ConsoleApp({ section = "overview", resourceId, subtenantId, nodeGroup, channelId, componentClusterId, freezeSelection }: { section?: Section; resourceId?: string; subtenantId?: string; nodeGroup?: string; channelId?: "alpha" | "beta" | "stable"; componentClusterId?: string; freezeSelection?: FreezeSelection }) {
  const { mode, setMode } = useAppearance();
  const browserAuth = useBrowserAuth();
  const { scopeKey, identity, liveUpdates, setLiveUpdates, connect } = usePlatform();
  const router = useRouter();
  const routeContext = JSON.stringify([section, resourceId, subtenantId, nodeGroup, channelId, componentClusterId, freezeSelection?.scope, freezeSelection?.id]);
  const [leavingResource, setLeavingResource] = useState<string>();
  // Clear the guard as soon as new route props arrive, including when React reuses this instance.
  if (leavingResource !== undefined && leavingResource !== routeContext) setLeavingResource(undefined);
  const hasResourceContext = !!(resourceId || channelId || componentClusterId || freezeSelection?.id);
  const switchTenant = (id: string) => {
    if (hasResourceContext) setLeavingResource(routeContext);
    setConnectionOpen(false);
    connect(id);
    router.replace(tenantSwitchPath(section, hasResourceContext));
  };
  const [connectionOpen, setConnectionOpen] = useState(false);
  const settingsId = useId();
  const current = navigation.find((item) => item.section === section)!;
  return <AppShell height="fill" variant="section" contentPadding={0}
    topNav={<TopNav label="Workspace" heading={<TopNavHeading heading="Cluster Vending" headingHref="/" />} endContent={<TenantSelector key={scopeKey} onSwitch={switchTenant}><Button label="Settings" variant="ghost" onClick={() => setConnectionOpen((open) => !open)} aria-haspopup="dialog" /></TenantSelector>} />}
    sideNav={<SideNav aria-label="Primary" footer={<Text type="supporting">{identity ? identity.anonymous ? "Local API session" : identity.username || identity.subject : "Connecting…"}</Text>}>
      <SideNavSection title="Workspace">{navigation.map((item) => <SideNavItem key={item.section} label={item.label} href={item.section === "overview" ? "/" : `/${item.section}`} isSelected={item.section === section} />)}</SideNavSection>
    </SideNav>}
  ><Layout height="fill" content={<LayoutContent padding={6}><VStack gap={6}>
    <HStack as="nav" aria-label="Breadcrumb" gap={2} vAlign="center" wrap="wrap">
      <Link href="/">Workspace</Link><Text color="secondary" aria-hidden="true">/</Text>
      {resourceId || channelId ? <>
        <Link href={`/${section}`}>{current.label}</Link>
        {(subtenantId || nodeGroup) && resourceId && <>
          <Text color="secondary" aria-hidden="true">/</Text>
          <Link href={`/clusters/${encodeURIComponent(resourceId)}?tab=${nodeGroup ? "node-groups" : "subtenants"}`}>{nodeGroup ? "Node groups" : "Sub Tenants"}</Link>
        </>}
        <Text color="secondary" aria-hidden="true">/</Text>
        <Text color="secondary" aria-current="page">{nodeGroup ? "Node group" : subtenantId ? "Sub Tenant" : channelId ? "Channel" : "Details"}</Text>
      </> : <Text color="secondary" aria-current="page">{current.label}</Text>}
    </HStack>
    <Dialog isOpen={connectionOpen} onOpenChange={setConnectionOpen} purpose="form" width="min(40rem, calc(100vw - 2rem))" maxHeight="90dvh" padding={0}>
      <Layout padding={6}
        header={<DialogHeader title="Settings" subtitle="Manage your workspace connection and display preferences." onOpenChange={setConnectionOpen} />}
        content={<LayoutContent padding={6}><VStack gap={6}>
          <VStack as="section" gap={4} width="100%" aria-labelledby={`${settingsId}-connection`}>
            <Heading level={2} id={`${settingsId}-connection`}>{browserAuth.enabled ? "Your session" : "Workspace connection"}</Heading>
            <ConnectionPanel formId={`${settingsId}-form`} showHeading={false} showConnectAction={false} onConnect={() => setConnectionOpen(false)} />
          </VStack>
          <Divider />
          <VStack as="section" gap={4} width="100%" aria-labelledby={`${settingsId}-preferences`}>
            <VStack gap={1}><Heading level={2} id={`${settingsId}-preferences`}>Preferences</Heading><Text color="secondary">Preference changes apply immediately.</Text></VStack>
            <CheckboxInput label="Live updates" value={liveUpdates} onChange={setLiveUpdates} description="Off by default. Enable background status polling for this tab. Submitted operations are tracked separately." />
            <VStack gap={2}>
              <Heading level={3}>Appearance</Heading>
              <SegmentedControl label="Appearance" value={mode} onChange={setMode} size="md" layout="fill"><SegmentedControlItem value="light" label="Light" /><SegmentedControlItem value="dark" label="Dark" /><SegmentedControlItem value="system" label="System" /></SegmentedControl>
            </VStack>
          </VStack>
        </VStack></LayoutContent>}
        footer={<LayoutFooter padding={6} hasDivider><HStack gap={2} hAlign="end" wrap="wrap"><Button label="Close" onClick={() => setConnectionOpen(false)} />{!browserAuth.enabled && <Button label="Connect" variant="primary" type="submit" form={`${settingsId}-form`} />}</HStack></LayoutFooter>}
      />
    </Dialog>
    {leavingResource === routeContext && hasResourceContext ? <Text role="status">Switching tenant and opening Overview…</Text> : <SessionGate key={scopeKey} onConfigure={() => setConnectionOpen(true)}><SectionPage key={`${section}:${channelId ?? resourceId ?? "list"}:${nodeGroup ?? ""}:${componentClusterId ?? ""}:${freezeSelection?.scope ?? ""}:${freezeSelection?.id ?? ""}`} section={section} resourceId={resourceId} subtenantId={subtenantId} nodeGroup={nodeGroup} channelId={channelId} componentClusterId={componentClusterId} freezeSelection={freezeSelection} /></SessionGate>}
  </VStack></LayoutContent>} /></AppShell>;
}
