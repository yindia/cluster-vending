import type { Timestamp } from "@bufbuild/protobuf/wkt";
import type { BrowserClients } from "../api/client";
import { ReleaseChannel, UpgradeMode } from "../../gen/cloud/v1/cluster_pb";
import { AwsSupportType } from "../../gen/cloud/v1/aws_pb";
import type { ReleaseChannelInfo, ReleaseMinor } from "../../gen/cloud/v1/admin_release_pb";

/**
 * A Kubernetes minor a cluster may ask for, as the release its channel offers
 * puts it. Extended: the release offers it on no channel while it puts other
 * minors on channels (EKS extended support, pkg/release.Ending). A cluster on
 * one runs with Manual upgrades and Extended support only.
 */
export interface KubernetesVersionOption {
  minor: string;
  channel: ReleaseChannel;
  extended: boolean;
  /** The minor a new cluster on no channel gets. */
  isDefault: boolean;
  endOfSupport?: Timestamp;
}

/** The channel whose release the server reads a spec's minors from: its own, else stable's. */
export function offerChannel(releaseChannel: string): ReleaseChannel {
  const value = Number(releaseChannel);
  return value === ReleaseChannel.ALPHA || value === ReleaseChannel.BETA ? value : ReleaseChannel.STABLE;
}

/** The release that channel offers now; undefined when it offers none. */
export function offeredRelease(channels: readonly ReleaseChannelInfo[], releaseChannel: string): string | undefined {
  const channel = offerChannel(releaseChannel);
  return channels.find((item) => item.channel === channel)?.release || undefined;
}

/** "1.33.2" and "v1.33" are minor "1.33"; anything else is kept as typed. */
export function minorOf(version: string): string {
  const match = /^v?(\d+)\.(\d+)(?:\.\d+)?$/.exec(version.trim());
  return match ? `${match[1]}.${match[2]}` : version.trim();
}

function newerFirst(a: string, b: string) { return b.localeCompare(a, undefined, { numeric: true }); }

/** The release's minors, newest first, each marked with its channel or extended support. */
export function kubernetesVersionOptions(minors: readonly ReleaseMinor[]): KubernetesVersionOption[] {
  const assigns = minors.some((item) => item.channel !== ReleaseChannel.UNSPECIFIED);
  return [...minors].sort((a, b) => newerFirst(a.minor, b.minor)).map((item) => ({
    minor: item.minor, channel: item.channel, isDefault: item.default, endOfSupport: item.endOfSupport,
    extended: assigns && item.channel === ReleaseChannel.UNSPECIFIED,
  }));
}

export function findVersionOption(options: readonly KubernetesVersionOption[], version: string): KubernetesVersionOption | undefined {
  const minor = minorOf(version);
  return minor ? options.find((item) => item.minor === minor) : undefined;
}

/** The version settings of a cluster spec that extended support constrains. */
export interface ExtendedSupportFields { kubernetesVersion: string; upgradeMode: UpgradeMode; supportType: AwsSupportType }

/**
 * What the server refuses on an extended minor (pkg/reconcile/cluster/extended.go): Automatic
 * upgrades, and the Standard support policy. `running` is the minor a cluster runs when the
 * spec pins none. Undefined when the fields are allowed.
 */
export function extendedSupportIssue(fields: ExtendedSupportFields, options: readonly KubernetesVersionOption[], running = ""): string | undefined {
  const option = findVersionOption(options, fields.kubernetesVersion || running);
  if (!option?.extended) return undefined;
  if (fields.upgradeMode === UpgradeMode.AUTOMATIC) return `Kubernetes ${option.minor} is on extended support: it runs with UPGRADE_MODE_MANUAL only. Set spec.upgradeMode: UPGRADE_MODE_MANUAL, or choose a minor on a release channel.`;
  if (fields.supportType === AwsSupportType.STANDARD) return `Kubernetes ${option.minor} is on extended support: AWS_SUPPORT_TYPE_STANDARD would have AWS upgrade the control plane by force when standard support ends. Set spec.aws.upgradePolicy.supportType: AWS_SUPPORT_TYPE_EXTENDED, or leave it out.`;
  return undefined;
}

/** The release's minors for a cluster's channel, as the server reads them (stable when it has none). */
export async function loadKubernetesOffer(api: BrowserClients, tenantId: string, releaseChannel: ReleaseChannel, signal: AbortSignal): Promise<KubernetesVersionOption[]> {
  const { channels } = await api.tenant.listReleaseChannels({ tenantId }, { signal });
  const name = offeredRelease(channels, String(releaseChannel));
  if (!name) return [];
  const { release } = await api.tenant.getRelease({ tenantId, name }, { signal });
  return kubernetesVersionOptions(release?.kubernetes ?? []);
}
