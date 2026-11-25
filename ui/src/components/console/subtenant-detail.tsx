"use client";

import { useCallback } from "react";
import { Banner } from "@astryxdesign/core/Banner";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { Link } from "@astryxdesign/core/Link";
import { Text } from "@astryxdesign/core/Text";
import { usePlatform } from "@/components/platform-provider";
import { useAsyncQuery } from "@/lib/api/use-query";
import { scopedSubTenant } from "@/lib/workflows/subtenants";
import { SubTenantProperties, subTenantRow } from "./cluster-subtenants";
import { CopyValue } from "./copy-value";
import { DetailHeader, StatusValue } from "./detail-layout";
import { RefreshButton } from "./refresh-button";

export function SubTenantDetail({ clusterId, subtenantId }: { clusterId: string; subtenantId: string }) {
  const { api, tenantId } = usePlatform();
  const load = useCallback(async (signal: AbortSignal) => {
    const { cluster } = await api.cluster.getCluster({ tenantId, clusterId }, { signal });
    if (!cluster || cluster.id !== clusterId || cluster.tenantId !== tenantId) throw new Error("The cluster is unavailable in the selected tenant.");
    if (!cluster.spec?.naas) throw new Error("Namespace as a Service is disabled on this cluster.");
    const response = await api.subtenant.getSubTenant({ tenantId, clusterId, subtenantId }, { signal });
    return { cluster, resource: scopedSubTenant(response.subtenant, { tenantId, clusterId }, subtenantId) };
  }, [api, tenantId, clusterId, subtenantId]);
  const detail = useAsyncQuery(load);
  const back = <Link href={`/clusters/${encodeURIComponent(clusterId)}?tab=subtenants`}>Cluster Sub Tenants</Link>;
  const refresh = <RefreshButton label="Refresh Sub Tenant details" onClick={detail.refresh} isLoading={detail.refreshing} />;
  if (detail.error) return <VStack gap={3}>{back}<Banner status="error" title="Sub Tenant unavailable" description={detail.error} />{refresh}</VStack>;
  if (!detail.data) return <Text role="status">Loading Sub Tenant…</Text>;
  const { cluster, resource } = detail.data;
  const row = subTenantRow(resource);
  return <VStack as="section" aria-label="Sub Tenant details" gap={6}>
    <DetailHeader title={row.name} description={`Sub Tenant in ${cluster.displayName || cluster.name || clusterId}`} status={<StatusValue label={row.state} tone={row.tone} />} metadata={<>{back}<CopyValue value={subtenantId} label="Sub Tenant ID" isSecondary /></>} />
    <HStack hAlign="between" vAlign="center" gap={3} wrap="wrap">
      <Heading level={2}>Allocation details</Heading>
      {refresh}
    </HStack>
    <SubTenantProperties resource={resource} />
  </VStack>;
}
