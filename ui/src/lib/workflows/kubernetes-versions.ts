import { timestampDate, type Timestamp } from "@bufbuild/protobuf/wkt";
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

export interface ExtendedSupportFields { upgradeMode: string; awsSupportType: string; kubernetesVersion: string }

const channelNames: Partial<Record<ReleaseChannel, string>> = { [ReleaseChannel.ALPHA]: "Alpha", [ReleaseChannel.BETA]: "Beta", [ReleaseChannel.STABLE]: "Stable" };

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

export function isExtendedVersion(options: readonly KubernetesVersionOption[], version: string): boolean {
  return !!findVersionOption(options, version)?.extended;
}

function day(value: Timestamp | undefined) { return value ? timestampDate(value).toISOString().slice(0, 10) : ""; }

/** "1.34 · Stable channel", "1.31 · Extended support". */
export function versionOptionLabel(option: KubernetesVersionOption): string {
  const where = option.extended ? "Extended support" : channelNames[option.channel] ? `${channelNames[option.channel]} channel` : "No channel";
  return `${option.minor} · ${where}${option.isDefault ? " · default" : ""}`;
}

/** The option's support end, and what extended support asks of the cluster. */
export function versionOptionDescription(option: KubernetesVersionOption): string {
  const ends = option.endOfSupport ? `Support ends ${day(option.endOfSupport)} (UTC).` : "Support end not reported.";
  return option.extended ? `${ends} Manual upgrades and the Extended support policy only.` : ends;
}

/**
 * What the server refuses on an extended minor (pkg/reconcile/cluster/extended.go): Automatic
 * upgrades, and the Standard support policy. Undefined when the fields are allowed.
 */
export function extendedSupportIssue(form: ExtendedSupportFields, options: readonly KubernetesVersionOption[], running = ""): string | undefined {
  const version = form.kubernetesVersion || running;
  const option = findVersionOption(options, version);
  if (!option?.extended) return undefined;
  if (Number(form.upgradeMode) === UpgradeMode.AUTOMATIC) return `Kubernetes ${option.minor} is on extended support: it runs with Manual upgrades only. Choose Manual, or a minor on a release channel.`;
  if (Number(form.awsSupportType) === AwsSupportType.STANDARD) return `Kubernetes ${option.minor} is on extended support: the Standard support policy would have AWS upgrade the control plane by force when standard support ends. Choose Extended or the server default.`;
  return undefined;
}

/** The fields an extended minor needs: Manual upgrades and Extended support. */
export const extendedSupportFields = { upgradeMode: String(UpgradeMode.MANUAL), awsSupportType: String(AwsSupportType.EXTENDED) } as const;
