"use client";

import { useCallback, useMemo } from "react";
import { Banner } from "@astryxdesign/core/Banner";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { Link } from "@astryxdesign/core/Link";
import { pixel, proportional } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import { usePlatform } from "@/components/platform-provider";
import type { Cluster } from "@/gen/cloud/v1/cluster_pb";
import { useAsyncQuery } from "@/lib/api/use-query";
import { PLATFORM_MANAGED_LABEL, nodeGroupRows, type NodeGroupRow } from "@/lib/workflows/node-groups";
import { ConsoleTable } from "./console-table";
import { StatusValue } from "./detail-layout";
import { NodeGroupYamlButton } from "./node-group-yaml-button";
import { RefreshButton } from "./refresh-button";

/**
 * The cluster's node groups, read-only: the platform creates the system node group and the Karpenter platform pool
 * from its own configuration, so there is nothing to add, edit or remove here. Each group links to its nodes and usage.
 */
export function ClusterNodeGroups({ cluster, onRefresh, refreshing }: { cluster: Cluster; onRefresh: () => void; refreshing: boolean }) {
  const { api, tenantId } = usePlatform();
  // The exact Karpenter objects the controller applies, rendered from the stored cluster; they change with its etag.
  const clusterId = cluster.id, etag = cluster.etag;
  const loadManifests = useCallback(async (signal: AbortSignal) => {
    void etag;
    return (await api.cluster.listClusterNodeGroupManifests({ tenantId, clusterId }, { signal })).nodeGroups;
  }, [api, tenantId, clusterId, etag]);
  const manifests = useAsyncQuery(loadManifests);
  const statusGroups = useMemo(() => cluster.status?.nodeGroups ?? [], [cluster.status?.nodeGroups]);
  const rows = useMemo(() => nodeGroupRows(statusGroups, manifests.data), [statusGroups, manifests.data]);
  return <VStack gap={4} as="section" aria-label="Node groups" width="100%">
    <HStack gap={3} hAlign="between" vAlign="center" wrap="wrap" width="100%">
      <VStack gap={1}>
        <Heading level={3}>Node groups</Heading>
        <Text type="supporting" data-platform-managed="">{PLATFORM_MANAGED_LABEL}. The system node group runs Karpenter and the EKS add-ons; Karpenter scales the platform pool for everything else. Both come from the platform&apos;s configuration and cannot be changed here.</Text>
      </VStack>
      <RefreshButton label="Refresh node groups" onClick={() => { onRefresh(); manifests.refresh(); }} isLoading={refreshing || manifests.refreshing} />
    </HStack>
    {manifests.error && <Banner status="warning" title="YAML is unavailable" description={manifests.error} />}
    <NodeGroupTable clusterId={clusterId} rows={rows} />
  </VStack>;
}

function NodeGroupTable({ clusterId, rows }: { clusterId: string; rows: NodeGroupRow[] }) {
  return <ConsoleTable aria-label="Cluster node groups" hasFullDetails idKey="name" data={rows} emptyState="Node groups will appear after the controller reports them." columns={[
    { key: "name", header: "Node group", width: proportional(2, { minWidth: 110 }), renderCell: row => <VStack gap={1}>
      <Link href={`/clusters/${encodeURIComponent(clusterId)}/node-groups/${encodeURIComponent(row.name)}`} aria-label={`Open node group ${row.name}`}>{row.name}</Link>
      {row.platformManaged && <Text type="supporting">{PLATFORM_MANAGED_LABEL}</Text>}
    </VStack> },
    { key: "status", header: "Status", width: proportional(2, { minWidth: 120 }), renderCell: row => <VStack gap={1}>
      <StatusValue label={row.phaseLabel} tone={row.tone} />
      {row.message && <Text type="supporting">{row.message}</Text>}
    </VStack> },
    { key: "nodes", header: "Nodes", align: "end", width: proportional(0.7, { minWidth: 64 }), renderCell: row => <Text>{row.nodes === undefined ? "—" : row.nodes}</Text> },
    { key: "yaml", header: "YAML", align: "center" as const, width: pixel(120), resizable: false, renderCell: (row: NodeGroupRow) => row.manifest ? <NodeGroupYamlButton manifest={row.manifest} /> : <Text type="supporting" color="secondary">—</Text> },
  ]} />;
}
