"use client";

import type { Network } from "@/gen/cloud/v1/network_pb";
import type { Cluster } from "@/gen/cloud/v1/cluster_pb";
import { ManifestCreateDialog } from "./manifest-create-dialog";

type Props = {
  isOpen: boolean;
  onOpenChange: (open: boolean) => void;
  onUseForm?: () => void;
} & (
  | { kind: "NetworkImport"; onImported: (resource: Network) => void }
  | { kind: "ClusterImport"; onImported: (resource: Cluster) => void }
);

/** Reuse the create editor's draft, review, cancellation and scope protections. */
export function ImportManifestDialog(props: Props) {
  if (props.kind === "NetworkImport") return <ManifestCreateDialog {...props} onCreated={props.onImported} />;
  return <ManifestCreateDialog {...props} onCreated={props.onImported} />;
}
