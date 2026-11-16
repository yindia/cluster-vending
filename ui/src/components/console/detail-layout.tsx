"use client";

import type { ReactNode } from "react";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { Heading, Text } from "@astryxdesign/core/Text";
import { Grid } from "@astryxdesign/core/Grid";
import { Card } from "@astryxdesign/core/Card";
import { Divider } from "@astryxdesign/core/Divider";
import { StatusDot, type StatusDotVariant } from "@astryxdesign/core/StatusDot";

export function StatusValue({ label, tone = "neutral" }: { label: string; tone?: StatusDotVariant }) {
  return <HStack gap={2} vAlign="center" minHeight="1.5rem">
    <StatusDot label={label} variant={tone} aria-hidden="true" />
    <Text type="label" maxLines={1}>{label}</Text>
  </HStack>;
}

export function DetailHeader({ title, description, metadata, status, actions }: {
  title: string; description?: string; metadata?: ReactNode; status?: ReactNode; actions?: ReactNode;
}) {
  return <VStack gap={4}>
    <HStack hAlign="between" vAlign="center" gap={4} wrap="wrap">
      <HStack gap={3} wrap="wrap" vAlign="center"><Heading level={1}>{title}</Heading>{status}</HStack>
      {actions && <HStack gap={2} wrap="wrap" vAlign="center">{actions}</HStack>}
    </HStack>
    {description && <Text color="secondary">{description}</Text>}
    {metadata && <HStack gap={3} wrap="wrap" vAlign="center">{metadata}</HStack>}
    <Divider />
  </VStack>;
}

export function DetailSummary({ items }: { items: readonly { label: string; value: ReactNode; note?: ReactNode }[] }) {
  return <Grid columns={{ minWidth: 180, max: 4 }} gap={3} align="stretch">
    {items.map((item) => <Card key={item.label} padding={4} height="100%"><VStack gap={2}>
      <Text type="supporting">{item.label}</Text>
      {typeof item.value === "string" || typeof item.value === "number" ? <Text type="large" weight="semibold">{item.value}</Text> : item.value}
      {item.note && <Text type="supporting">{item.note}</Text>}
    </VStack></Card>)}
  </Grid>;
}
