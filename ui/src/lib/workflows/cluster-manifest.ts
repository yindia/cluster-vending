import type { Cluster, ClusterSpec } from "../../gen/cloud/v1/cluster_pb";
import { AwsSupportType } from "../../gen/cloud/v1/aws_pb";
import type { BrowserClients } from "../api/client";
import { extendedSupportIssue, loadKubernetesOffer } from "./kubernetes-versions";
import { ManifestError, type Manifest } from "./manifest-yaml";

/** Rules a Cluster manifest's spec meets before it is decoded: the tier is the network's. */
export function refuseClusterTier({ spec, at }: Manifest) {
  if (Object.hasOwn(spec, "tier")) throw new ManifestError("Cluster tier is inherited from its network. Remove spec.tier.", at.lineOf(["spec", "tier"]));
}

/** The warning a cluster with no administrators of its own gets: only the platform can use kubectl. */
export function administratorsNote(spec: ClusterSpec | undefined): string | undefined {
  if (spec?.access?.adminPrincipals.length) return undefined;
  return "No administrators: only the platform's own role is an admin by default, so no one else can use kubectl. Add your IAM role or user ARN to spec.access.adminPrincipals.";
}

/**
 * The server's extended-support rules for the minor the spec asks for (or `running`), checked
 * against the release its channel offers. Throws on a refusal, with the line to change.
 * Returns a note when the release could not be read: the server still checks.
 */
export async function checkExtendedSupport(api: BrowserClients, tenantId: string, spec: ClusterSpec | undefined, manifest: Manifest, signal: AbortSignal, running = ""): Promise<string | undefined> {
  if (!spec) return undefined;
  let options;
  try { options = await loadKubernetesOffer(api, tenantId, spec.releaseChannel, signal); }
  catch (cause) {
    signal.throwIfAborted();
    return `Extended-support rules could not be checked here (${cause instanceof Error ? cause.message : String(cause)}). The server checks them when you submit.`;
  }
  const aws = spec.provider.case === "aws" ? spec.provider.value : undefined;
  const issue = extendedSupportIssue({
    kubernetesVersion: spec.controlPlane?.kubernetesVersion ?? "", upgradeMode: spec.upgradeMode,
    supportType: aws?.upgradePolicy?.supportType ?? AwsSupportType.UNSPECIFIED,
  }, options, running);
  if (!issue) return undefined;
  const path = /UPGRADE_MODE/.test(issue) ? ["spec", "upgradeMode"] : ["spec", "aws", "upgradePolicy", "supportType"];
  throw new ManifestError(issue, manifest.at.lineOf(path) ?? manifest.at.lineOf(["spec", "controlPlane", "kubernetesVersion"]));
}

/** The minor a cluster runs, for rules that read it when the spec pins none. */
export const runningMinor = (cluster: Cluster | undefined) => cluster?.status?.currentKubernetesVersion ?? "";
