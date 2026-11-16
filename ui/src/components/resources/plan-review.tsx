"use client";

import { Collapsible } from "@astryxdesign/core/Collapsible";
import { MetadataList, MetadataListItem } from "@astryxdesign/core/MetadataList";
import { Banner } from "@astryxdesign/core/Banner";
import { Heading } from "@astryxdesign/core/Heading";
import { ConsoleTable as Table } from "../console/console-table";
import { proportional, type TableColumn } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import { VStack } from "@astryxdesign/core/VStack";

/** Accepts generated changes directly, plus plain objects for a lightweight preview. */
export interface ReviewChange {
  /** cloud.v1.PlannedAction wire value; unknown future values remain visible. */
  action: number;
  kind: string;
  name: string;
  detail: string;
  apiVersion?: string;
  namespace?: string;
  externalName?: string;
  specPath?: string;
  requiresReplacement?: boolean;
  retainsCloudResource?: boolean;
  unifiedDiff?: string;
  waitsFor?: string;
}

export interface PlanReviewProps {
  plan: {
    planId: string;
    applicable: boolean;
    changes: readonly ReviewChange[];
    warnings?: readonly string[];
  };
}

// Numeric values are the cloud.v1.PlannedAction wire values. No generated runtime required.
const actionLabels: Partial<Record<number, string>> = {
  0: "Unspecified action",
  1: "Create",
  2: "Update in place",
  3: "Disruptive update",
  4: "Delete",
  5: "Rejected: immutable field",
};

interface ReviewRow extends Record<string, unknown> {
  id: number;
  change: ReviewChange;
}

function impactText(change: ReviewChange): string {
  const impact: string[] = [];
  if (change.action === 4) {
    impact.push(change.retainsCloudResource
      ? "Retains cloud resource; removes platform management."
      : change.retainsCloudResource === false
        ? "Destructive: deletes the cloud resource."
        : "Potentially destructive: cloud retention was not reported.");
  }
  if (change.requiresReplacement) impact.push("Part of a resource replacement.");
  if (change.action === 3) impact.push("Workloads affected during rollout.");
  if (change.action === 5) impact.push("Update refused; this field cannot change in place.");
  if (change.action === 0 || !(change.action in actionLabels)) {
    impact.push("Unknown impact; request a supported plan before applying.");
  }
  if (change.waitsFor) impact.push(`Deferred until: ${change.waitsFor}.`);
  return impact.join(" ") || "No additional impact reported.";
}

const columns: TableColumn<ReviewRow>[] = [
  {
    key: "resource", header: "Resource", width: proportional(2),
    renderCell: ({ change }) => (
      <VStack gap={1}>
        <Text type="label">{change.kind} / {change.name}</Text>
        {change.namespace !== undefined && <Text type="supporting">{change.namespace ? `Namespace: ${change.namespace}` : "Cluster scoped"}</Text>}
        {change.apiVersion && <Text type="supporting">{change.apiVersion}</Text>}
        {change.externalName && <Text type="code">{change.externalName}</Text>}
      </VStack>
    ),
  },
  {
    key: "action", header: "Action", width: proportional(1),
    renderCell: ({ change }) => <Text type="label">{actionLabels[change.action] ?? `Unknown action (${change.action})`}</Text>,
  },
  {
    key: "details", header: "Change details", width: proportional(3),
    renderCell: ({ change }) => (
      <VStack gap={1}>
        {change.specPath && <Text type="code">{change.specPath}</Text>}
        <Text>{change.detail || "No detail supplied by the server."}</Text>
        {change.unifiedDiff && <Collapsible trigger="View field changes" defaultIsOpen={false}><VStack as="pre" isScrollable gap={0}><Text type="code">{change.unifiedDiff}</Text></VStack></Collapsible>}
      </VStack>
    ),
  },
  {
    key: "impact", header: "Impact and timing", width: proportional(3),
    renderCell: ({ change }) => <Text>{impactText(change)}</Text>,
  },
];

/** Read-only review. Applying a reviewed plan belongs to the owning workflow. */
export function PlanReview({ plan }: PlanReviewProps) {
  const hasUnsupportedAction = plan.changes.some((change) =>
    change.action === 0 || !(change.action in actionLabels));
  const blocked = !plan.applicable || !plan.planId || hasUnsupportedAction ||
    plan.changes.some((change) => change.action === 5);
  const rows: ReviewRow[] = plan.changes.map((change, id) => ({ id, change }));

  return (
    <VStack gap={4}>
      <Heading level={3}>Resource changes</Heading>
      <Text type="supporting">Review resource changes in server apply order. No changes have been applied.</Text>
      <MetadataList orientation="horizontal">
        <MetadataListItem label="Create">{plan.changes.filter((change) => change.action === 1).length}</MetadataListItem>
        <MetadataListItem label="Update">{plan.changes.filter((change) => change.action === 2 || change.action === 3).length}</MetadataListItem>
        <MetadataListItem label="Remove">{plan.changes.filter((change) => change.action === 4).length}</MetadataListItem>
      </MetadataList>
      {blocked && <Banner status="error" title="Plan cannot be applied" description="The plan is not applicable, has no verifiable ID, or contains a rejected or unsupported action. Request a new plan before continuing." />}
      {!!plan.warnings?.length && (
        <Banner status="warning" title="Review plan warnings" collapsible={false}>
          <VStack as="ul" gap={2}>
            {plan.warnings.map((warning, index) => <VStack as="li" key={index}><Text>{warning}</Text></VStack>)}
          </VStack>
        </Banner>
      )}
      {rows.length ? (
        <Table data={rows} columns={columns} idKey="id" density="balanced" verticalAlign="top" textOverflow="wrap" aria-label="Planned resource changes" hasFullDetails />
      ) : (
        <Text>No resource changes in this plan.</Text>
      )}
      <Collapsible trigger="Plan reference" defaultIsOpen={false}><Text type="code">{plan.planId || "Missing"}</Text></Collapsible>
    </VStack>
  );
}
