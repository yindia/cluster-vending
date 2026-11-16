// Adapted from the Astryx dashboard template: LayoutContent padding 6, section gap 6.
"use client";

import { AppShell } from "@astryxdesign/core/AppShell";
import { TopNav, TopNavHeading } from "@astryxdesign/core/TopNav";
import { SideNav, SideNavItem, SideNavSection } from "@astryxdesign/core/SideNav";
import { Layout, LayoutContent, VStack, HStack } from "@astryxdesign/core/Layout";
import { Heading, Text } from "@astryxdesign/core/Text";
import { Card } from "@astryxdesign/core/Card";
import { Grid } from "@astryxdesign/core/Grid";
import { Divider } from "@astryxdesign/core/Divider";
import { Button } from "@astryxdesign/core/Button";
import { Table, type TableColumn } from "@astryxdesign/core/Table";
import { List, ListItem } from "@astryxdesign/core/List";
import { SegmentedControl, SegmentedControlItem } from "@astryxdesign/core/SegmentedControl";
import { useAppearance } from "@/app/providers";

import type { Section } from "@/lib/navigation";

const navigation: { section: Section; label: string; description: string }[] = [
  { section: "overview", label: "Overview", description: "A clear view of your platform, from cloud accounts to cluster rollouts." },
  { section: "accounts", label: "Accounts", description: "Connect and verify the cloud accounts your workloads run in." },
  { section: "networks", label: "Networks", description: "Shared networking and the rollout tiers inherited by your clusters." },
  { section: "clusters", label: "Clusters", description: "Manage cluster configuration, release channels and upgrade progress." },
  { section: "components", label: "Component catalog", description: "Discover the components available to your clusters." },
  { section: "releases", label: "Releases", description: "Follow platform changes through Alpha, Beta and Stable." },
  { section: "freeze", label: "Freeze controls", description: "Pause changes at platform, tenant, network or cluster scope." },
];

type ResourceRow = { name: string; type: string; provider: string; context: string; status: string };
const resources: ResourceRow[] = [
  { name: "Production account", type: "Account", provider: "AWS", context: "Example tenant", status: "Verified" },
  { name: "prod-network", type: "Network", provider: "AWS", context: "Tier 1", status: "Ready" },
  { name: "payments-prod", type: "Cluster", provider: "AWS", context: "Stable · Tier 1", status: "Running" },
  { name: "platform-canary", type: "Cluster", provider: "AWS", context: "Alpha · Tier 1", status: "Upgrading" },
];
const columns: TableColumn<ResourceRow>[] = [
  { key: "name", header: "Resource" }, { key: "type", header: "Type" },
  { key: "provider", header: "Provider" }, { key: "context", header: "Context" }, { key: "status", header: "Status" },
];
const channels = [
  { name: "Alpha", audience: "Early validation", order: "First to receive releases" },
  { name: "Beta", audience: "Broader validation", order: "After Alpha" },
  { name: "Stable", audience: "Production workloads", order: "After Beta" },
];
const channelColumns: TableColumn<(typeof channels)[number]>[] = [
  { key: "name", header: "Channel" }, { key: "audience", header: "Purpose" }, { key: "order", header: "Promotion order" },
];

export default function ThemePreview({ section = "overview" }: { section?: Section }) {
  const { mode, setMode } = useAppearance();
  const current = navigation.find((item) => item.section === section)!;
  const rows = section === "overview" ? resources : resources.filter((row) => row.type.toLowerCase() + "s" === section);
  return (
    <AppShell
      height="fill"
      variant="section"
      contentPadding={0}
      topNav={<TopNav
        label="Workspace"
        heading={<TopNavHeading heading="Cluster Vending" subheading="Platform console" headingHref="/" />}
        endContent={<SegmentedControl label="Appearance" value={mode} onChange={setMode} size="sm">
          <SegmentedControlItem value="light" label="Light" />
          <SegmentedControlItem value="dark" label="Dark" />
          <SegmentedControlItem value="system" label="System" />
        </SegmentedControl>}
      />}
      sideNav={<SideNav aria-label="Primary" footer={<Text type="supporting">Theme preview · Example data</Text>}>
        <SideNavSection title="Workspace">
          {navigation.slice(0, 5).map((item) => <SideNavItem key={item.section} label={item.label} href={item.section === "overview" ? "/" : "/" + item.section} isSelected={item.section === section} />)}
        </SideNavSection>
        <SideNavSection title="Platform">
          {navigation.slice(5).map((item) => <SideNavItem key={item.section} label={item.label} href={"/" + item.section} isSelected={item.section === section} />)}
        </SideNavSection>
      </SideNav>}
    >
      <Layout height="fill" content={<LayoutContent padding={6}>
        <VStack gap={6}>
          <VStack gap={2}>
            <Text type="supporting">WORKSPACE / DESIGN PREVIEW</Text>
            <HStack hAlign="between" vAlign="center" gap={4} wrap="wrap">
              <Heading level={1}>{current.label}</Heading>
              <Button label="View clusters" href="/clusters" variant="primary" />
            </HStack>
            <Text color="secondary">{current.description}</Text>
            <Text type="supporting">Neutral theme · Sample resources only. This preview makes no backend requests.</Text>
          </VStack>

          {section === "overview" && <Grid columns={{ minWidth: 200, repeat: "fit" }} gap={4}>
            {[["Cloud accounts", "1"], ["Networks", "1"], ["Clusters", "2"], ["Active upgrades", "1"]].map(([name, count]) =>
              <Card key={name}><VStack gap={2}><Text color="secondary">{name}</Text><Text size="2xl" weight="semibold" hasTabularNumbers>{count}</Text></VStack></Card>
            )}
          </Grid>}

          <Divider />
          {(section === "overview" || section === "accounts" || section === "networks" || section === "clusters") && <VStack gap={4}>
            <Heading level={2}>{section === "overview" ? "Resource inventory" : current.label}</Heading>
            <Table aria-label="Example resource inventory" data={rows} columns={columns} idKey="name" density="balanced" />
            <Text type="supporting">Account → Network → Cluster. Provider-specific configuration will appear within each resource workflow.</Text>
          </VStack>}

          {(section === "overview" || section === "releases") && <VStack gap={4}>
            <Heading level={2}>Release channels</Heading>
            <Table aria-label="Release channel progression" data={channels} columns={channelColumns} idKey="name" />
            <Text type="supporting">Within each channel, rollout order is Tier 2 → Tier 1 → Tier 0. Each cluster inherits its network’s tier.</Text>
          </VStack>}

          {section === "components" && <VStack gap={4}>
            <Heading level={2}>Cluster capabilities</Heading>
            <List edge="flush" hasDividers header="Example catalog">
              <ListItem label="Platform components" description="Platform-managed components follow the cluster’s release." />
              <ListItem label="Tenant components" description="Enable and configure optional components on a cluster." />
              <ListItem label="Add-ons" description="Provider-specific add-ons are managed from cluster details." />
            </List>
          </VStack>}

          {section === "freeze" && <VStack gap={4}>
            <Heading level={2}>Control the scope of a pause</Heading>
            <List edge="flush" hasDividers header="Freeze scopes">
              <ListItem label="Entire platform" description="Pause changes across all tenants." />
              <ListItem label="Tenant" description="Pause one tenant’s workloads." />
              <ListItem label="Network" description="Pause the shared network and its clusters." />
              <ListItem label="Cluster" description="Pause a single cluster independently." />
            </List>
            <Text type="supporting">Policy confirmation and Crossplane pause status will be shown separately from the requested state.</Text>
          </VStack>}
        </VStack>
      </LayoutContent>} />
    </AppShell>
  );
}
