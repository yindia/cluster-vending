"use client";

import { useCallback } from "react";
import { Banner } from "@astryxdesign/core/Banner";
import { Divider } from "@astryxdesign/core/Divider";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack } from "@astryxdesign/core/HStack";
import { MetadataList, MetadataListItem } from "@astryxdesign/core/MetadataList";
import { Selector } from "@astryxdesign/core/Selector";
import { StackItem } from "@astryxdesign/core/Stack";
import { Text } from "@astryxdesign/core/Text";
import { VStack } from "@astryxdesign/core/VStack";
import { usePlatform } from "@/components/platform-provider";
import type { Cluster } from "@/gen/cloud/v1/cluster_pb";
import type { Network } from "@/gen/cloud/v1/network_pb";
import { collectPages } from "@/lib/api/pagination";
import { useAsyncQuery } from "@/lib/api/use-query";
import { supportsCapability } from "@/lib/providers/registry";
import { accountDescription } from "@/lib/workflows/cluster-placement";
import { RefreshButton } from "./refresh-button";

export function ErrorNotice({ message }: { message: string }) {
  return message ? <Banner status="error" title="Request failed" description={message} /> : null;
}

export function tierLabel(tier?: number) {
  return tier ? `Tier ${tier - 1} (from network)` : "Network default";
}

export function useClusterNetworks() {
  const { api, tenantId } = usePlatform();
  const load = useCallback(async (signal: AbortSignal) => {
    const [accounts, networks] = await Promise.all([
      api.tenant.listCloudAccounts({ tenantId }, { signal }),
      collectPages(async (pageToken) => {
        const page = await api.network.listNetworks({ tenantId, pageSize: 200, pageToken }, { signal });
        return { items: page.networks, nextPageToken: page.nextPageToken };
      }, signal),
    ]);
    return { networks, accounts: accounts.accounts };
  }, [api, tenantId]);
  return useAsyncQuery(load);
}
export type ClusterNetworkChoices = ReturnType<typeof useClusterNetworks>;

export function NetworkPlacement({ network, choices, cluster }: { network?: Network; choices?: ClusterNetworkChoices; cluster?: Cluster }) {
  const networkName = network?.displayName || network?.name || cluster?.spec?.network?.networkId;
  if (!networkName) return null;
  const account = choices?.data?.accounts.find((item) => item.id === network?.spec?.bootstrapId);
  const tier = network?.spec?.tier || cluster?.status?.tier;
  return <VStack gap={3}>
    <Divider />
    <VStack gap={1}><Text type="supporting" color="secondary">Selected network</Text><Heading level={4}>{networkName}</Heading></VStack>
    <MetadataList label={{ position: "top" }}>
      {choices && <MetadataListItem label="Cloud account">{accountDescription(account)}</MetadataListItem>}
      <MetadataListItem label="Region">{network?.spec?.region || cluster?.spec?.region}</MetadataListItem>
      <MetadataListItem label="Inherited tier">{tier ? `Tier ${tier - 1}` : "Network default"}</MetadataListItem>
    </MetadataList>
  </VStack>;
}

export function NetworkPicker({ choices, value, disabled, onChange, operation }: { choices: ClusterNetworkChoices; value: string; disabled: boolean; onChange: (value: string) => void; operation: "cluster.create" | "cluster.import" }) {
  const networks = choices.data?.networks ?? [];
  const selected = networks.find((item) => item.id === value);
  const accountFor = (item: Network) => choices.data?.accounts.find((account) => account.id === item.spec?.bootstrapId);
  const options = networks.map((item) => {
    const account = accountFor(item);
    const supported = supportsCapability(account?.provider.case ?? "", operation);
    return { value: item.id, label: `${item.displayName || item.name} · ${item.spec?.region || "Unknown region"}`, disabled: !supported, description: supported ? accountDescription(account) : "Unsupported or missing cloud account" };
  });
  return <VStack gap={3}>
    <ErrorNotice message={choices.error} />
    <HStack gap={2} vAlign="end" width="100%">
      <StackItem size="fill"><Selector label="Existing network" hasSearch value={value} isLoading={choices.loading} isDisabled={disabled || choices.loading || !options.length} placeholder="Choose a network" onChange={onChange} options={options} /></StackItem>
      <RefreshButton label="Refresh networks and accounts" onClick={choices.refresh} isDisabled={disabled} isLoading={choices.refreshing} />
    </HStack>
    {selected && <VStack data-placement-inline=""><Text type="supporting">{accountDescription(accountFor(selected))} · {tierLabel(selected.spec?.tier)}</Text></VStack>}
    {choices.data && !choices.error && !options.some((option) => !option.disabled) &&
      <Text type="supporting">{options.length ? "This tenant has no networks with a supported cloud account." : "Create or import a network in this tenant, then refresh this list."}</Text>}
  </VStack>;
}
