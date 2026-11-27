"use client";

import { useRef, type ReactNode } from "react";
import { Banner } from "@astryxdesign/core/Banner";
import { MetadataList, MetadataListItem } from "@astryxdesign/core/MetadataList";
import { usePlatform } from "@/components/platform-provider";
import type { CloudAccount } from "@/gen/cloud/v1/tenant_service_pb";
import type { Network } from "@/gen/cloud/v1/network_pb";
import type { Cluster } from "@/gen/cloud/v1/cluster_pb";
import { applyManifest, manifestTemplate, reviewManifest, type ManifestKind, type ManifestReview } from "@/lib/workflows/manifests";
import { CopyValue } from "./copy-value";
import { ManifestWorkflow, ReviewNotes } from "./manifest-workflow";

type Created = { CloudAccount: CloudAccount; Network: Network; NetworkImport: Network; Cluster: Cluster; ClusterImport: Cluster };

export interface ManifestCreateDialogProps<K extends ManifestKind> {
  kind: K;
  mark?: ReactNode;
  onClose: () => void;
  onCreated: (resource: Created[K]) => void;
}

const titles: Record<ManifestKind, { title: string; submit: string; description: string }> = {
  CloudAccount: { title: "Connect cloud account", submit: "Connect account", description: "Connect an existing cloud account to the tenant with a CloudAccount manifest." },
  Network: { title: "Create network", submit: "Create network", description: "Create a network with a Network manifest." },
  Cluster: { title: "Create cluster", submit: "Create cluster", description: "Provision a managed cluster with a Cluster manifest." },
  NetworkImport: { title: "Import network", submit: "Import network", description: "Discover an existing VPC read-only with a NetworkImport manifest." },
  ClusterImport: { title: "Import cluster", submit: "Import cluster", description: "Discover an existing cluster read-only with a ClusterImport manifest." },
};

/** Create and import take a manifest only: the starter template, review, then the exact reviewed request. */
export function ManifestCreateDialog<K extends ManifestKind>({ kind, mark, onClose, onCreated }: ManifestCreateDialogProps<K>) {
  const { api, tenantId } = usePlatform();
  // One request ID per manifest text: an unchanged retry is idempotent, any edit is a new request.
  const requestId = useRef<{ text: string; id: string } | null>(null);
  const importing = kind === "NetworkImport" || kind === "ClusterImport";
  const copy = titles[kind];
  const initialText = manifestTemplate(kind, tenantId);
  return <ManifestWorkflow<ManifestReview> kind={kind} mode={importing ? "import" : "create"} mark={mark}
    title={copy.title} description={`${copy.description} Tenant ${tenantId || "none selected"}.`}
    initialText={initialText} resetLabel="Reset to example" reviewLabel="Review" submitLabel={copy.submit}
    review={(text, signal) => {
      if (requestId.current?.text !== text) requestId.current = { text, id: crypto.randomUUID() };
      return reviewManifest(api, text, kind, tenantId, requestId.current.id, signal);
    }}
    submit={async (review, signal) => {
      const resource = await applyManifest(api, review, signal);
      requestId.current = null;
      // The reviewed kind fixes the generated resource type applyManifest returns.
      onCreated(resource as Created[K]);
    }}
    renderReview={(review) => <>
      <MetadataList columns="multi">{review.summary.map(item => <MetadataListItem key={item.label} label={item.label}>{item.label.endsWith("ID") ? <CopyValue value={item.value} label={item.label} /> : item.value}</MetadataListItem>)}</MetadataList>
      <ReviewNotes notes={review.notes} />
      {importing
        ? <Banner status="info" title="Import stays read-only" description="This records discovery of existing cloud resources. It does not create or take ownership of cloud infrastructure. Review discovery on the detail page, then explicitly take over when ready." />
        : kind === "CloudAccount"
          ? <Banner status="info" title="Connects an existing cloud account" description="Registers the account and its platform ProviderConfig. Reusing an AWS account ID updates its existing connection; no cloud account is created. Access is verified asynchronously." />
          : <Banner status="info" title="Creates managed cloud resources" description="The platform performs final validation when you confirm. Provisioning continues asynchronously after the request is accepted; cloud charges may apply." />}
    </>}
    onClose={onClose} />;
}
