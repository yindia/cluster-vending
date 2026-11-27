"use client";

import { Heading } from "@astryxdesign/core/Heading";
import { VStack } from "@astryxdesign/core/Layout";
import { MetadataList, MetadataListItem } from "@astryxdesign/core/MetadataList";
import { Text } from "@astryxdesign/core/Text";
import type { ComponentCloudResources } from "@/gen/cloud/v1/component_cloud_pb";
import { componentIamSummary } from "@/lib/workflows/component-cloud";
import { CopyValue } from "./copy-value";
import { ValuesEditor } from "./values-editor";

function Values({ items, label, empty }: { items: string[]; label: string; empty: string }) {
  return items.length ? <VStack gap={1}>{items.map((item) => <CopyValue key={item} value={item} label={label} />)}</VStack> : <Text>{empty}</Text>;
}

/** What the platform creates in the tenant's AWS account for a component: its IAM role, read-only. */
export function ComponentCloudResourcesSection({ resources }: { resources: ComponentCloudResources | undefined }) {
  const role = componentIamSummary(resources);
  return <VStack as="section" aria-labelledby="component-cloud-resources" gap={3}>
    <Heading level={2} id="component-cloud-resources">Cloud resources</Heading>
    {!role ? <Text>This component creates no cloud resources in your account.</Text> : <>
      <Text type="supporting">Created in your tenant&apos;s AWS account on each cluster the component is enabled on, before its chart is installed, and removed after it is uninstalled. The role&apos;s trust policy is always the platform&apos;s.</Text>
      <MetadataList>
        <MetadataListItem label="IAM role">One role per cluster, named by the platform</MetadataListItem>
        <MetadataListItem label="Access type">{role.access}</MetadataListItem>
        <MetadataListItem label="Service accounts"><Values items={role.serviceAccounts} label="service account" empty="None" /></MetadataListItem>
        <MetadataListItem label="Managed policies"><Values items={role.managedPolicyArns} label="managed policy ARN" empty="None" /></MetadataListItem>
      </MetadataList>
      {role.policy
        ? <ValuesEditor label="IAM policy document" language="json" value={role.policy} readOnly height="16rem"
          description="Attached to the role. Read-only, as the catalog defines it; its placeholders are filled in for each cluster." />
        : <Text>No inline policy: the role has only its managed policies.</Text>}
    </>}
  </VStack>;
}
