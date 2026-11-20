"use client";

import { useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { Dialog, DialogHeader } from "@astryxdesign/core/Dialog";
import { HStack, Layout, LayoutContent, LayoutFooter, VStack } from "@astryxdesign/core/Layout";
import { usePlatform } from "@/components/platform-provider";
import type { NodeGroupManifest } from "@/gen/cloud/v1/node_group_manifest_pb";
import { ValuesEditor } from "./values-editor";

/** Opens the exact EC2NodeClass and NodePool the controller applies for one node group (read-only, with copy). */
export function NodeGroupYamlButton({ manifest }: { manifest: NodeGroupManifest }) {
  const { tenantId } = usePlatform();
  const [open, setOpen] = useState(false);
  return <>
    <Button label={`View YAML for node group ${manifest.name}`} variant="ghost" size="sm" aria-haspopup="dialog" onClick={() => setOpen(true)}>View YAML</Button>
    {open && <NodeGroupYamlDialog manifest={manifest} tenantId={tenantId} onClose={() => setOpen(false)} />}
  </>;
}

function NodeGroupYamlDialog({ manifest, tenantId, onClose }: { manifest: NodeGroupManifest; tenantId: string; onClose: () => void }) {
  const title = `Node group ${manifest.name} YAML`;
  return <Dialog isOpen onOpenChange={value => { if (!value) onClose(); }} purpose="form" width="min(56rem, calc(100vw - 2rem))" maxHeight="90dvh" padding={0}>
    <Layout padding={6}
      header={<DialogHeader title={title} subtitle={`Tenant ${tenantId} · the Karpenter objects the controller applies to the cluster`} onOpenChange={value => { if (!value) onClose(); }} />}
      content={<LayoutContent padding={6}><VStack gap={5}>
        <ValuesEditor label="EC2NodeClass YAML" language="yaml" readOnly value={manifest.ec2NodeClassYaml} height="min(16rem, 30dvh)"
          description="Where the group's nodes run: role, subnets, security groups and the pinned AMI." />
        <ValuesEditor label="NodePool YAML" language="yaml" readOnly value={manifest.nodePoolYaml} height="min(24rem, 40dvh)"
          description="What the group's nodes are: requirements, labels, taints, limits and disruption." />
      </VStack></LayoutContent>}
      footer={<LayoutFooter padding={5} hasDivider><HStack hAlign="end" vAlign="center" gap={2} width="100%"><Button label="Close YAML" onClick={onClose} /></HStack></LayoutFooter>}
    />
  </Dialog>;
}
