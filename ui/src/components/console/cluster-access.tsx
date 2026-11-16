"use client";

import { useCallback, useState } from "react";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Dialog, DialogHeader } from "@astryxdesign/core/Dialog";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { StackItem } from "@astryxdesign/core/Stack";
import { Text } from "@astryxdesign/core/Text";
import { usePlatform } from "@/components/platform-provider";
import type { Cluster } from "@/gen/cloud/v1/cluster_pb";
import { AccessRole } from "@/gen/cloud/v1/tenant_service_pb";
import { useAsyncQuery } from "@/lib/api/use-query";
import { supportsCapability } from "@/lib/providers/registry";
import { clusterAccessCommand } from "@/lib/workflows/cluster-access";
import { CopyButton } from "./copy-value";
import { ProviderIcon, KubernetesIcon } from "./provider-icons";
import { RefreshButton } from "./refresh-button";

export function ClusterAccessButton({ cluster }: { cluster: Cluster }) {
  const { tenantId, scopeKey, identity } = usePlatform();
  const canRead = !!identity && (identity.anonymous || identity.grants.some((grant) =>
    grant.role === AccessRole.PLATFORM_ADMIN ||
    ([AccessRole.TENANT_ADMIN, AccessRole.TENANT_VIEWER].includes(grant.role) && (grant.tenants.includes(tenantId) || grant.tenants.includes("*")))));
  return <ClusterAccess key={`${scopeKey}:${cluster.id}`} cluster={cluster} tenantId={tenantId} canRead={canRead} />;
}

function ClusterAccess({ cluster, tenantId, canRead }: { cluster: Cluster; tenantId: string; canRead: boolean }) {
  const [open, setOpen] = useState(false);
  const supported = supportsCapability(cluster.spec?.provider.case ?? "", "cluster.kubeconfig");
  const available = supported && canRead && !!tenantId && !!cluster.id && cluster.tenantId === tenantId;
  return <>
    <Button label="Get kubeconfig" onClick={() => setOpen(true)} isDisabled={!available} tooltip={!supported ? "This provider does not support kubeconfig access." : undefined} aria-haspopup="dialog" />
    {open && available && <ClusterAccessDialog clusterId={cluster.id} tenantId={tenantId} name={cluster.displayName || cluster.name} onOpenChange={setOpen} />}
  </>;
}

function ClusterAccessDialog({ clusterId, tenantId, name, onOpenChange }: { clusterId: string; tenantId: string; name: string; onOpenChange: (open: boolean) => void }) {
  const { api } = usePlatform();
  const load = useCallback(async (signal: AbortSignal) => {
    const response = await api.cluster.getCluster({ tenantId, clusterId, live: true }, { signal });
    if (!response.cluster) throw new Error("The API did not return cluster details.");
    return response.cluster;
  }, [api, tenantId, clusterId]);
  const query = useAsyncQuery(load);
  const busy = query.loading || query.refreshing;
  // Never offer a previous command after a failed refresh or while revalidating.
  const access = !busy && !query.error && query.data ? clusterAccessCommand(query.data, tenantId, clusterId) : undefined;
  return <Dialog isOpen onOpenChange={onOpenChange} purpose="info" width="min(48rem, calc(100vw - 2rem))" maxHeight="90dvh" padding={6}>
    <VStack gap={5}>
      <DialogHeader title="Get kubeconfig" subtitle={query.data?.id === clusterId && query.data.tenantId === tenantId ? query.data.displayName || query.data.name : name} onOpenChange={onOpenChange}
        endContent={<RefreshButton label="Refresh cluster access details" isLoading={busy} onClick={query.refresh} />} />
      <HStack gap={2} vAlign="center">{access?.state === "ready" && <ProviderIcon provider={access.provider} />}<KubernetesIcon /><Text>Kubernetes access</Text></HStack>
      {busy && <Text role="status">Loading current cluster details…</Text>}
      {query.error && !busy && <Banner status="error" title="Could not load cluster access" description={query.error} />}
      {access && access.state !== "ready" && <Banner status={access.state === "invalid" ? "error" : "info"} title={access.state === "pending" ? "Cluster access pending" : access.state === "unsupported" ? "Provider not supported" : "Cluster identity could not be verified"} description={access.message} />}
      {access?.state === "ready" && <VStack gap={3}>
        <Text type="label">AWS CLI command</Text>
        <HStack gap={3} vAlign="center" className="cv-copy-target" padding={3} style={{ background: "var(--color-background-muted)", borderRadius: "var(--radius-container)" }}>
          <StackItem size="fill">
            <pre aria-label="AWS kubeconfig command" style={{ margin: 0, whiteSpace: "pre-wrap", overflowWrap: "anywhere" }}><Text type="code" hasCapsize={false} style={{ whiteSpace: "pre-wrap", overflowWrap: "anywhere" }}>{access.command}</Text></pre>
          </StackItem>
          <StackItem size="static"><CopyButton value={access.command} label="AWS kubeconfig command" /></StackItem>
        </HStack>
        <Text type="supporting">Run this command in your terminal with the AWS CLI installed and your own AWS session{access.accountId ? ` for account ${access.accountId}` : " for the cluster’s AWS account"}. It updates your local kubeconfig and current context. Your AWS identity must have EKS and Kubernetes access.</Text>
      </VStack>}
      <HStack hAlign="end"><Button label="Close" onClick={() => onOpenChange(false)} /></HStack>
    </VStack>
  </Dialog>;
}
