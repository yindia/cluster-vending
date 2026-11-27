"use client";

import { useCallback, type ReactNode } from "react";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Dialog, DialogHeader } from "@astryxdesign/core/Dialog";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { MetadataList, MetadataListItem } from "@astryxdesign/core/MetadataList";
import { proportional } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import { usePlatform } from "@/components/platform-provider";
import { PlanReview } from "@/components/resources/plan-review";
import { useAsyncQuery } from "@/lib/api/use-query";
import { applyUpdate, planApplicable, reviewUpdate, updateManifestText, type UpdateBaseline, type UpdateKind, type UpdateReview } from "@/lib/workflows/manifest-update";
import { ConsoleTable as Table } from "./console-table";
import { ManifestWorkflow, ReviewNotes } from "./manifest-workflow";

export interface ManifestUpdateDialogProps {
  kind: UpdateKind;
  id: string;
  /** The etag the detail page shows: review is blocked when the editor opened another one. */
  currentEtag?: string;
  mark?: ReactNode;
  onClose: () => void;
  onApplied: (message: string) => void;
}

const nouns: Record<UpdateKind, string> = { Cluster: "cluster", Network: "network", CloudAccount: "account" };

/** Loads the object as it was requested (the stored input), the baseline every change is worked out from. */
function useBaseline(kind: UpdateKind, id: string) {
  const { api, tenantId } = usePlatform();
  const load = useCallback(async (signal: AbortSignal): Promise<UpdateBaseline> => {
    if (kind === "Cluster") {
      const response = await api.cluster.getCluster({ tenantId, clusterId: id, asRequested: true }, { signal });
      if (!response.cluster) throw new Error("The server returned no cluster to edit.");
      return { kind, resource: response.cluster, asRequested: response.asRequested };
    }
    if (kind === "Network") {
      const response = await api.network.getNetwork({ tenantId, networkId: id, asRequested: true }, { signal });
      if (!response.network) throw new Error("The server returned no network to edit.");
      return { kind, resource: response.network, asRequested: response.asRequested };
    }
    const response = await api.tenant.getCloudAccount({ tenantId, id }, { signal });
    if (!response.account) throw new Error("The server returned no account to edit.");
    return { kind, resource: response.account };
  }, [api, tenantId, kind, id]);
  return useAsyncQuery(load);
}

/** Update takes the object's manifest only: edit it, review the server's plan, apply exactly that plan. */
export function ManifestUpdateDialog({ kind, id, currentEtag, mark, onClose, onApplied }: ManifestUpdateDialogProps) {
  const baseline = useBaseline(kind, id);
  const noun = nouns[kind];
  const title = `Edit ${noun}`;
  if (!baseline.data) return <Dialog isOpen onOpenChange={(open) => { if (!open) onClose(); }} purpose="form" width="min(40rem, calc(100vw - 1.5rem))" padding={6}>
    <VStack gap={4}>
      <DialogHeader title={title} onOpenChange={(open) => { if (!open) onClose(); }} />
      {baseline.loading ? <Text role="status">Loading the {noun}’s manifest…</Text> : <VStack role="alert"><Banner status="error" title={`The ${noun} could not be loaded`} description={baseline.error} /></VStack>}
      <HStack gap={2} hAlign="end"><Button label="Cancel" variant="secondary" onClick={onClose} />{!baseline.loading && <Button label="Retry" onClick={baseline.refresh} />}</HStack>
    </VStack>
  </Dialog>;
  // A reload is a new baseline: the editor starts again from it.
  const key = baseline.data.kind === "CloudAccount" ? baseline.data.resource.updateTime?.seconds.toString() ?? id : baseline.data.resource.etag;
  return <ManifestEditor key={key} kind={kind} baseline={baseline.data} currentEtag={currentEtag} mark={mark} title={title} onReload={baseline.refresh} reloading={baseline.refreshing} onClose={onClose} onApplied={onApplied} />;
}

function ManifestEditor({ kind, baseline, currentEtag, mark, title, onReload, reloading, onClose, onApplied }: Omit<ManifestUpdateDialogProps, "id"> & { baseline: UpdateBaseline; title: string; onReload: () => void; reloading: boolean }) {
  const { api, tenantId } = usePlatform();
  const noun = nouns[kind];
  const name = baseline.kind === "CloudAccount" ? baseline.resource.accountName || baseline.resource.id : baseline.resource.name;
  const stale = baseline.kind !== "CloudAccount" && !!currentEtag && baseline.resource.etag !== currentEtag;
  const planned = kind !== "CloudAccount";
  const notice = stale ? <Banner status="warning" title={`The ${noun} changed`} description="The editor holds an older version. Reload it, then make your change again."
    endContent={<Button label={`Reload ${noun}`} onClick={onReload} isLoading={reloading} />} /> : undefined;
  return <ManifestWorkflow<UpdateReview> kind={kind} mode="update" mark={mark} title={title}
    description={`Change ${name} in tenant ${tenantId} with its manifest. Review shows ${planned ? "the server's plan" : "the changes"} before anything is applied.`}
    initialText={updateManifestText(baseline)} resetLabel={`Reset to the ${noun} as it is`}
    reviewLabel={planned ? "Preview changes" : "Review changes"} submitLabel={planned ? "Apply reviewed plan" : "Save account"}
    notice={notice} isBlocked={stale}
    review={(text, signal) => reviewUpdate(api, text, tenantId, baseline, signal)}
    canSubmit={(review) => !planned || planApplicable(review.plan)}
    submit={async (review, signal) => {
      await applyUpdate(api, review, signal);
      onApplied(planned ? "Update recorded. The controller is converging the desired state." : "Account update accepted. Access will be checked asynchronously.");
    }}
    renderReview={(review) => <UpdateSummary review={review} />}
    onClose={onClose} />;
}

function UpdateSummary({ review }: { review: UpdateReview }) {
  const rows = review.changes.map((change) => ({ id: change.path, ...change }));
  return <VStack gap={5}>
    <VStack gap={3} as="section" aria-labelledby="manifest-changes">
      <Heading level={3} id="manifest-changes">Fields this update changes</Heading>
      <Table data={rows} idKey="id" density="compact" textOverflow="wrap" aria-label="Fields this update changes" columns={[
        { key: "path", header: "Field", width: proportional(2, { minWidth: 120 }), renderCell: (row) => <Text type="code">{row.path}</Text> },
        { key: "from", header: "Now", width: proportional(2, { minWidth: 100 }), renderCell: (row) => <Text type="supporting">{row.from}</Text> },
        { key: "to", header: "After", width: proportional(2, { minWidth: 100 }), renderCell: (row) => <Text weight="medium">{row.to}</Text> },
      ]} />
    </VStack>
    <ReviewNotes notes={review.notes} />
    {review.plan && <VStack gap={3} as="section" aria-labelledby="manifest-plan" role="region">
      <Heading level={3} id="manifest-plan">Server plan</Heading>
      <PlanReview plan={review.plan} />
      {!planApplicable(review.plan) && <Banner status="error" title="This plan cannot be applied" description="Change the manifest and review again." />}
    </VStack>}
    <MetadataList columns="multi">
      <MetadataListItem label={review.kind === "CloudAccount" ? "Account ID" : "Update mask"}>{review.kind === "CloudAccount" ? review.id : review.changes.map((change) => change.path).join(", ")}</MetadataListItem>
      {review.plan && <MetadataListItem label="Plan ID">{review.plan.planId || "None"}</MetadataListItem>}
    </MetadataList>
  </VStack>;
}
