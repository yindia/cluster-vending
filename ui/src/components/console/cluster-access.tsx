"use client";

import { useState } from "react";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Dialog, DialogHeader } from "@astryxdesign/core/Dialog";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { Text } from "@astryxdesign/core/Text";
import { usePlatform } from "@/components/platform-provider";
import type { Cluster } from "@/gen/cloud/v1/cluster_pb";
import { supportsCapability } from "@/lib/providers/registry";
import { CopyButton } from "./copy-value";
import { ProviderIcon, KubernetesIcon } from "./provider-icons";

/** POSIX shell quoting: values never become shell operators or substitutions. */
function shellArgument(value: string) { return `'${value.replaceAll("'", "'\\''")}'`; }

export function ClusterAccessButton({ cluster }: { cluster: Cluster }) {
  const { tenantId, scopeKey } = usePlatform();
  return <ClusterAccess key={`${scopeKey}:${cluster.id}`} cluster={cluster} tenantId={tenantId} />;
}

function ClusterAccess({ cluster, tenantId }: { cluster: Cluster; tenantId: string }) {
  const [open, setOpen] = useState(false);
  const supported = supportsCapability(cluster.spec?.provider.case ?? "", "cluster.kubeconfig");
  const command = `cluster-vending cluster kubeconfig ${shellArgument(cluster.id)} --tenant ${shellArgument(tenantId)}`;
  const oidc = cluster.spec?.access?.oidc;
  const configuredOidc = !!(oidc?.entra?.tenantId && oidc.entra.clientId || oidc?.issuerUrl && oidc.clientId);
  const commands = [
    { label: "Install kubeconfig", value: command, description: "Install the cluster-vending and AWS CLIs, configure your API connection, and sign in with your own AWS session. The CLI reads the current EKS name from the recorded cluster ARN (or the validated cluster name) and region, then runs aws eks update-kubeconfig. This updates your local kubeconfig and current context." },
    { label: "Print kubeconfig", value: `${command} --print`, description: "Prints kubeconfig with an exec authentication entry using your own AWS session; it requires a recorded endpoint and certificate authority." },
    ...(configuredOidc ? [{ label: "Print OIDC kubeconfig", value: `${command} --oidc`, description: "Uses this cluster’s configured OIDC provider through kubectl oidc-login. Run locally; the plugin performs your sign-in." }] : []),
  ];
  return <>
    <Button label="Get kubeconfig" onClick={() => setOpen(true)} isDisabled={!supported || !tenantId || !cluster.id || cluster.tenantId !== tenantId} tooltip={!supported ? "This provider does not support kubeconfig access." : undefined} aria-haspopup="dialog" />
    {supported && <Dialog isOpen={open} onOpenChange={setOpen} purpose="form" width="min(48rem, calc(100vw - 2rem))" maxHeight="90dvh" padding={6}>
      <VStack gap={5}>
        <DialogHeader title="Get kubeconfig" subtitle={cluster.displayName || cluster.name} onOpenChange={setOpen} />
        <HStack gap={2} vAlign="center"><ProviderIcon provider={cluster.spec?.provider.case ?? ""} /><KubernetesIcon /><Text>Kubernetes access</Text></HStack>
        <Banner status="info" title="Run on your own machine" description="Copy a command and run it in your terminal. This page does not run commands, download credentials, or change cloud resources. API permission to view a cluster does not grant Kubernetes access." />
        {commands.map(item => <VStack key={item.label} gap={2}><Text type="label">{item.label}</Text><Text type="code" style={{ overflowWrap: "anywhere", whiteSpace: "pre-wrap" }}>{item.value}</Text><HStack><CopyButton value={item.value} label={item.label.toLowerCase() + " command"} /></HStack><Text type="supporting">{item.description}</Text></VStack>)}
        {!configuredOidc && <Text type="supporting">No OIDC command is available because this cluster has no configured OIDC identity provider.</Text>}
        <HStack hAlign="end"><Button label="Close" onClick={() => setOpen(false)} /></HStack>
      </VStack>
    </Dialog>}
  </>;
}
