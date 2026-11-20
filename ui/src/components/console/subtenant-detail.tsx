"use client";

import { useCallback } from "react";
import { Banner } from "@astryxdesign/core/Banner";
import { Heading } from "@astryxdesign/core/Heading";
import { VStack } from "@astryxdesign/core/Layout";
import { Link } from "@astryxdesign/core/Link";
import { Text } from "@astryxdesign/core/Text";
import { usePlatform } from "@/components/platform-provider";
import { SubTenantState } from "@/gen/cloud/v1/subtenant_pb";
import { useAsyncQuery } from "@/lib/api/use-query";
import { scopedSubTenant } from "@/lib/workflows/subtenants";
import { SubTenantProperties, subTenantRow } from "./cluster-subtenants";
import { DetailNavigation, useDetailSection } from "./detail-navigation";
import { CopyValue } from "./copy-value";
import { DetailHeader, StatusValue } from "./detail-layout";
import { ManagedResourcePreviews } from "./managed-resource-previews";
import { RefreshButton } from "./refresh-button";

const pages = [{ value: "overview", label: "Overview" }, { value: "cloud-resources", label: "Cloud resources" }];

export function SubTenantDetail({ clusterId, subtenantId }: { clusterId: string; subtenantId: string }) {
  const navigation = useDetailSection(pages);
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
    <DetailNavigation resource="Sub Tenant" pages={pages} selection={navigation} actions={navigation.active === "overview" && refresh} />
    <VStack id={navigation.panelId(navigation.active)} role="tabpanel" aria-label={navigation.label} tabIndex={0} gap={4}>
      {navigation.active === "overview" ? <>
        <Heading level={2}>Allocation details</Heading>
        <SubTenantProperties resource={resource} />
      </> : <ManagedResourcePreviews ownership="subtenant" clusterId={clusterId} subtenantId={subtenantId} owner={row.name} region={cluster.spec?.region ?? ""} provider={cluster.spec?.provider.case || cluster.status?.provider.case} deleting={resource.status?.state === SubTenantState.DELETING} />}
    </VStack>
  </VStack>;
}
