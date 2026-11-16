"use client";

import { useCallback, useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { Banner } from "@astryxdesign/core/Banner";
import { Dialog, DialogHeader } from "@astryxdesign/core/Dialog";
import { HStack, Layout, LayoutContent, LayoutFooter, VStack } from "@astryxdesign/core/Layout";
import { Text } from "@astryxdesign/core/Text";
import { usePlatform } from "@/components/platform-provider";
import { useAsyncQuery } from "@/lib/api/use-query";
import { resourceYaml, type YamlResource } from "@/lib/workflows/resource-yaml";
import { ValuesEditor } from "./values-editor";

type Props = { kind: YamlResource["kind"]; resourceId: string };
export function ResourceYamlButton(props: Props) {
  const { tenantId, scopeKey } = usePlatform();
  return <ScopedYamlButton key={`${scopeKey}:${props.kind}:${props.resourceId}`} {...props} tenantId={tenantId} />;
}
function ScopedYamlButton({ kind, resourceId, tenantId }: Props & { tenantId: string }) {
  const [open, setOpen] = useState(false);
  return <>
    <Button label="View YAML" variant="secondary" aria-haspopup="dialog" isDisabled={!tenantId || !resourceId} onClick={() => setOpen(true)} />
    {open && <YamlDialog kind={kind} resourceId={resourceId} onClose={() => setOpen(false)} />}
  </>;
}
function YamlDialog({ kind, resourceId, onClose }: Props & { onClose: () => void }) {
  const { api, tenantId } = usePlatform();
  const load = useCallback(async (signal: AbortSignal) => {
    let input: YamlResource;
    let requested = true;
    if (kind === "CloudAccount") {
      const response = await api.tenant.getCloudAccount({ tenantId, id: resourceId }, { signal });
      if (!response.account) throw new Error("The account configuration was not returned.");
      input = { kind, resource: response.account };
    } else if (kind === "Network") {
      const response = await api.network.getNetwork({ tenantId, networkId: resourceId, asRequested: true }, { signal });
      if (!response.network) throw new Error("The network configuration was not returned.");
      input = { kind, resource: response.network }; requested = response.asRequested;
    } else {
      const response = await api.cluster.getCluster({ tenantId, clusterId: resourceId, asRequested: true }, { signal });
      if (!response.cluster) throw new Error("The cluster configuration was not returned.");
      input = { kind, resource: response.cluster }; requested = response.asRequested;
    }
    signal.throwIfAborted();
    if (input.resource.id !== resourceId || input.resource.tenantId !== tenantId) throw new Error("The returned configuration does not match this resource and tenant.");
    return { yaml: resourceYaml(input), requested };
  }, [api, tenantId, kind, resourceId]);
  const query = useAsyncQuery(load);
  return <Dialog isOpen onOpenChange={value => { if (!value) onClose(); }} purpose="form" width="min(56rem, calc(100vw - 2rem))" maxHeight="90dvh" padding={0}>
    <Layout padding={6}
      header={<DialogHeader title={`${kind} YAML`} subtitle={`Tenant ${tenantId} · ${resourceId}`} onOpenChange={value => { if (!value) onClose(); }} />}
      content={<LayoutContent padding={6}><VStack gap={4}>
        {query.loading && <Text role="status">Loading YAML configuration…</Text>}
        {query.error && <Banner status="error" title="YAML could not be loaded" description={query.error} />}
        {query.data && <>
          {!query.data.requested && <Banner status="info" title="Resolved configuration" description="The original input is unavailable. This YAML includes the returned configuration and any resolved defaults." />}
          <ValuesEditor label={`${kind} YAML configuration`} language="yaml" readOnly value={query.data.yaml} height="min(28rem, 50dvh)" description="Configuration snapshot for this existing resource. IDs identify the resource; observed status and API etags are omitted." />
        </>}
      </VStack></LayoutContent>}
      footer={<LayoutFooter padding={5} hasDivider><HStack hAlign="end" gap={2}><Button label="Refresh YAML" isLoading={query.refreshing} onClick={query.refresh} /><Button label="Close YAML" onClick={onClose} /></HStack></LayoutFooter>}
    />
  </Dialog>;
}
