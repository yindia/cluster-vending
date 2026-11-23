"use client";

import { Banner } from "@astryxdesign/core/Banner";
import { Heading } from "@astryxdesign/core/Heading";
import { MetadataList, MetadataListItem } from "@astryxdesign/core/MetadataList";
import { Text } from "@astryxdesign/core/Text";
import { VStack } from "@astryxdesign/core/VStack";
import type { ClusterStatus } from "@/gen/cloud/v1/cluster_pb";
import { clusterSummaryView, nodeGroupTotals } from "@/lib/workflows/cluster-summary";
import { DetailSummary, StatusValue } from "./detail-layout";

interface ClusterWorkloadSummaryProps {
  status: ClusterStatus | undefined;
}

// ClusterWorkloadSummary is the cluster status summary the controller's
// passes recorded (PROP-02 5.2): node, violation and event counts with their
// ages, the node groups' requested and allocatable totals, and whether the
// platform can read the cluster. Old counts are marked; none are zeros.
export function ClusterWorkloadSummary({ status }: ClusterWorkloadSummaryProps) {
  const view = clusterSummaryView(status, Date.now());
  const groups = (status?.nodeGroups ?? []).map(group => ({ group, totals: nodeGroupTotals(group) })).filter(entry => entry.totals);
  return <VStack gap={4}>
    <Heading level={3}>Workload summary</Heading>
    {view.notice && <Banner status="warning" title="The workload cluster cannot be reached" description={view.notice} />}
    <DetailSummary items={view.items.map(item => ({
      label: item.label,
      value: item.value,
      note: item.old ? <StatusValue label={item.note} tone="warning" /> : item.note,
    }))} />
    {groups.length > 0 && <MetadataList>
      {groups.map(({ group, totals }) => <MetadataListItem key={group.name} label={`Node group ${group.name}`}>
        <Text>{`CPU requested ${totals!.cpu} · memory requested ${totals!.memory} · ${totals!.pods} pods`}</Text>
      </MetadataListItem>)}
    </MetadataList>}
  </VStack>;
}
